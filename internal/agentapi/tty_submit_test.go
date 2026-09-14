package agentapi

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingMaster stands in for a PTY master: it records every Write as a
// separate entry with the time it landed, which is the whole point of the
// submit path — the text and the carriage return must NOT arrive as one
// write, because a TUI reads a multi-character chunk as a paste and treats
// the CR inside it as a newline in the prompt box instead of a submit.
type recordingMaster struct {
	mu     sync.Mutex
	writes []string
	at     []time.Time
	closed chan struct{}
	once   sync.Once
}

func newRecordingMaster() *recordingMaster {
	return &recordingMaster{closed: make(chan struct{})}
}

func (m *recordingMaster) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes = append(m.writes, string(b))
	m.at = append(m.at, time.Now())
	return len(b), nil
}

// Read blocks until Close so the hub's read loop stays alive for the test.
func (m *recordingMaster) Read(_ []byte) (int, error) {
	<-m.closed
	return 0, io.EOF
}

func (m *recordingMaster) Close() error {
	m.once.Do(func() { close(m.closed) })
	return nil
}

func (m *recordingMaster) snapshot() ([]string, []time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.writes...), append([]time.Time(nil), m.at...)
}

func TestSubmitPacesTheCarriageReturnApartFromTheText(t *testing.T) {
	m := newRecordingMaster()
	if err := ttySubmit(m, "ship it", false); err != nil {
		t.Fatalf("ttySubmit: %v", err)
	}
	writes, at := m.snapshot()
	if len(writes) != 2 {
		t.Fatalf("want text and CR as two writes, got %d: %q", len(writes), writes)
	}
	if writes[0] != "ship it" {
		t.Fatalf("first write must be the text alone, got %q", writes[0])
	}
	if writes[1] != "\r" {
		t.Fatalf("last write must be the bare CR, got %q", writes[1])
	}
	if gap := at[1].Sub(at[0]); gap < ttySubmitPause {
		t.Fatalf("CR followed the text after %v, want at least %v", gap, ttySubmitPause)
	}
}

func TestSubmitBracketsOnlyWhenAsked(t *testing.T) {
	plain := newRecordingMaster()
	if err := ttySubmit(plain, "line one\nline two", false); err != nil {
		t.Fatalf("ttySubmit: %v", err)
	}
	writes, _ := plain.snapshot()
	if strings.Contains(writes[0], "\x1b[200~") {
		t.Fatalf("unbracketed submit must not wrap the text: %q", writes[0])
	}

	wrapped := newRecordingMaster()
	if err := ttySubmit(wrapped, "line one\nline two", true); err != nil {
		t.Fatalf("ttySubmit: %v", err)
	}
	writes, _ = wrapped.snapshot()
	if writes[0] != "\x1b[200~line one\nline two\x1b[201~" {
		t.Fatalf("bracketed submit must wrap the text in paste markers, got %q", writes[0])
	}
	if writes[len(writes)-1] != "\r" {
		t.Fatalf("the CR must still be its own write, got %q", writes[len(writes)-1])
	}
}

// An empty submit is still a submit: it is how the key row's Enter clears a
// TUI menu, and it must not write an empty payload first.
func TestSubmitOfNothingIsJustTheCarriageReturn(t *testing.T) {
	m := newRecordingMaster()
	if err := ttySubmit(m, "", true); err != nil {
		t.Fatalf("ttySubmit: %v", err)
	}
	writes, _ := m.snapshot()
	if len(writes) != 1 || writes[0] != "\r" {
		t.Fatalf("want a single CR write, got %q", writes)
	}
}

func TestHubSubmitReachesTheRegisteredMaster(t *testing.T) {
	h := newTTYHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m := newRecordingMaster()
	h.Register("run-1", m, func(_, _ uint16) error { return nil }, 80)
	t.Cleanup(func() { _ = m.Close() })

	if err := h.Submit("run-1", "/model opus", false); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	writes, _ := m.snapshot()
	if len(writes) != 2 || writes[0] != "/model opus" || writes[1] != "\r" {
		t.Fatalf("want the command then a separate CR, got %q", writes)
	}
	if err := h.Submit("run-missing", "x", false); err == nil {
		t.Fatal("submitting to a run with no live session must fail")
	}
}
