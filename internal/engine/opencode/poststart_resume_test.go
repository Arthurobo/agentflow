package opencode_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/arthurobo/agentflow/internal/engine/opencode"
)

// PostStart used to create a session unconditionally. On a resume that is not
// a harmless extra row: FocusSession then pulls the TUI off the session the
// child was told to reopen and onto the new empty one, and the managed row is
// bound to the wrong id.
//
// Verified live against opencode 1.18.29 before writing this. The TUI reports
// which session it has open in the terminal title, and the sequence observed
// while driving the OLD behaviour by hand was:
//
//	OpenCode -> OC | round13 resume probe -> OpenCode -> OC | WRONG new session
//
// The middle step is `opencode --session <id>` reopening the real session with
// no API call at all; the last is PostStart taking it away again.

type fakeTUI struct {
	mu       sync.Mutex
	created  int
	focused  []string
	newID    string
	srv      *httptest.Server
	healthOK bool
}

func newFakeTUI(t *testing.T) *fakeTUI {
	t.Helper()
	f := &fakeTUI{newID: "ses_freshly_created", healthOK: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		if !f.healthOK {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true})
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.created++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": f.newID})
	})
	mux.HandleFunc("/tui/select-session", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"sessionID"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.focused = append(f.focused, body.SessionID)
		f.mu.Unlock()
		_, _ = w.Write([]byte("true"))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTUI) creates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

func (f *fakeTUI) focusedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.focused...)
}

func TestPostStartOnAResumeBindsTheExistingSessionAndCreatesNothing(t *testing.T) {
	f := newFakeTUI(t)
	e := opencode.New(opencode.Config{Hostname: "127.0.0.1"})

	const resuming = "ses_f887405aeffeI7QkYkZPitJurA"
	var bound string
	if err := e.PostStart(context.Background(), f.srv.URL, "/tmp/ws", "a title",
		resuming, func(id string) { bound = id }); err != nil {
		t.Fatalf("PostStart: %v", err)
	}

	if n := f.creates(); n != 0 {
		t.Fatalf("POST /session was called %d times on a resume; the session already exists", n)
	}
	if bound != resuming {
		t.Fatalf("bound %q, want the session being resumed %q", bound, resuming)
	}
	// Focus is what puts it on screen; binding without focusing leaves the
	// row right and the terminal showing something else.
	if got := f.focusedIDs(); len(got) != 1 || got[0] != resuming {
		t.Fatalf("focused %v, want exactly the resumed session", got)
	}
}

// The fresh path is unchanged: there is nothing to reopen, so one is created.
func TestPostStartOnAFreshSpawnStillCreatesAndBindsANewSession(t *testing.T) {
	f := newFakeTUI(t)
	e := opencode.New(opencode.Config{Hostname: "127.0.0.1"})

	var bound string
	if err := e.PostStart(context.Background(), f.srv.URL, "/tmp/ws", "a title",
		"", func(id string) { bound = id }); err != nil {
		t.Fatalf("PostStart: %v", err)
	}
	if n := f.creates(); n != 1 {
		t.Fatalf("POST /session called %d times on a fresh spawn, want 1", n)
	}
	if bound != f.newID {
		t.Fatalf("bound %q, want the created session %q", bound, f.newID)
	}
	if got := f.focusedIDs(); len(got) != 1 || got[0] != f.newID {
		t.Fatalf("focused %v, want the created session", got)
	}
}

// Whitespace is not a session id. A row whose session_id is " " must take the
// fresh path rather than binding a blank.
func TestPostStartTreatsABlankResumeIDAsAFreshSpawn(t *testing.T) {
	f := newFakeTUI(t)
	e := opencode.New(opencode.Config{Hostname: "127.0.0.1"})

	if err := e.PostStart(context.Background(), f.srv.URL, "/tmp/ws", "t",
		"   ", func(string) {}); err != nil {
		t.Fatalf("PostStart: %v", err)
	}
	if n := f.creates(); n != 1 {
		t.Fatalf("a blank resume id must create a session, got %d creates", n)
	}
}

// An empty base is a programming error, not a resume.
func TestPostStartRefusesAnEmptyBase(t *testing.T) {
	e := opencode.New(opencode.Config{Hostname: "127.0.0.1"})
	if err := e.PostStart(context.Background(), "", "/tmp", "t", "ses_x", nil); err == nil {
		t.Fatal("an empty base URL must be refused")
	}
}
