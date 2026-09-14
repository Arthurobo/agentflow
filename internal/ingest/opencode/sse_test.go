package opencode_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/ingest"
	opencodeingest "github.com/arthurobo/agentflow/internal/ingest/opencode"
	"github.com/arthurobo/agentflow/internal/store"
)

type lockedSink struct {
	mu    sync.Mutex
	evs   []store.Incoming
	metas []*store.SessionMeta
}

func (l *lockedSink) EnqueueLive(_ context.Context, evs []store.Incoming, meta *store.SessionMeta) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evs = append(l.evs, evs...)
	for range evs {
		l.metas = append(l.metas, meta)
	}
	return nil
}

func (l *lockedSink) snapshot() ([]store.Incoming, []*store.SessionMeta) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]store.Incoming(nil), l.evs...), append([]*store.SessionMeta(nil), l.metas...)
}

// sseServer streams the given events and then holds the connection open.
func sseServer(t *testing.T, events ...map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func partEvent(sessionID, partID, text string, extra map[string]any) map[string]any {
	part := map[string]any{"id": partID, "sessionID": sessionID, "type": "text", "text": text}
	for k, v := range extra {
		part[k] = v
	}
	return map[string]any{"type": "message.part.updated", "properties": map[string]any{
		"sessionID": sessionID, "directory": "/home/u/code/app", "part": part,
	}}
}

// The live stream indexes events under their session, with the session's
// directory and project, keyed by the part id. An event it cannot place in a
// session is dropped, and a text part still streaming waits until it is done.
func TestSSEEventsCarryTheirSessionAndPartIdentity(t *testing.T) {
	done := map[string]any{"time": map[string]any{"start": float64(1757000000000), "end": float64(1757000001000)}}
	srv := sseServer(t,
		partEvent("ses_live", "prt_1", "the finished answer", done),
		partEvent("ses_live", "prt_2", "still typi", map[string]any{"time": map[string]any{"start": float64(1757000002000)}}),
		map[string]any{"type": "session.error", "properties": map[string]any{"error": "no session here"}},
	)
	sink := &lockedSink{}
	ing := opencodeingest.New(nil, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := ing.StartSSE(ctx, "run-1", srv.URL, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the finished part", func() bool { evs, _ := sink.snapshot(); return len(evs) > 0 })
	time.Sleep(100 * time.Millisecond) // let anything wrongly indexed arrive too
	evs, metas := sink.snapshot()
	if len(evs) != 1 {
		t.Fatalf("want only the finished part, got %d: %+v", len(evs), evs)
	}
	if evs[0].UUID != "prt_1" || evs[0].TS != 1757000000000 {
		t.Fatalf("the event must be keyed by its part and carry its own time: %+v", evs[0])
	}
	m := metas[0]
	if m.SessionID != "ses_live" || m.CWD != "/home/u/code/app" || m.Project == "" {
		t.Fatalf("the live meta must name the session, its cwd and project: %+v", m)
	}
}

// One consumer per run, even when two runs share a control URL, and it ends
// and is forgotten when the run's context ends.
func TestSSEConsumersAreKeyedByRunAndEndWithIt(t *testing.T) {
	srv := sseServer(t)
	ing := opencodeingest.New(nil, &lockedSink{})
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	if err := ing.StartSSE(ctxA, "run-a", srv.URL, "ses_a"); err != nil {
		t.Fatal(err)
	}
	if err := ing.StartSSE(ctxB, "run-b", srv.URL, "ses_b"); err != nil {
		t.Fatal(err)
	}
	if n := ing.ConsumerCount(); n != 2 {
		t.Fatalf("two runs on one control URL need two consumers, got %d", n)
	}
	cancelA()
	waitFor(t, "run-a's consumer to go", func() bool { return ing.ConsumerCount() == 1 })
	ing.StopSSE("run-b")
	if n := ing.ConsumerCount(); n != 0 {
		t.Fatalf("StopSSE must end and forget the consumer, %d left", n)
	}
}

// A single event larger than the old 1 MB line limit still arrives.
func TestSSEAcceptsLargeEvents(t *testing.T) {
	big := strings.Repeat("x", 3<<20)
	done := map[string]any{"time": map[string]any{"start": float64(1), "end": float64(2)}}
	srv := sseServer(t, partEvent("ses_big", "prt_big", big, done))
	sink := &lockedSink{}
	ing := opencodeingest.New(nil, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := ing.StartSSE(ctx, "run-big", srv.URL, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large event", func() bool { evs, _ := sink.snapshot(); return len(evs) == 1 })
}

// Two parts of the same type in one session are two rows; the same part seen
// twice (by the live stream and by the backfill) is one.
func TestPartIDsDedupeInTheIndex(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "dedupe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sess := &opencodeingest.SessionDoc{ID: "ses_d", ProjectID: "p", Directory: "/w"}
	var batch []store.Incoming
	for _, p := range []*opencodeingest.PartDoc{
		{ID: "prt_a", SessionID: "ses_d", Type: "text", Text: "first"},
		{ID: "prt_b", SessionID: "ses_d", Type: "text", Text: "second"},
		{ID: "prt_a", SessionID: "ses_d", Type: "text", Text: "first"},
	} {
		for _, ev := range opencodeingest.NormalizePart(p, sess) {
			batch = append(batch, opencodeingest.ToIncoming(ev))
		}
	}
	if err := st.InsertBatch(context.Background(), &store.Batch{Events: batch,
		Session: &store.SessionMeta{SessionID: "ses_d", FilePath: "x"}}); err != nil {
		t.Fatal(err)
	}
	if n := countEventsForSession(st, "ses_d"); n != 2 {
		t.Fatalf("want two distinct parts stored once each, got %d", n)
	}
}

// A session whose storage has not changed is not walked again on the next
// boot; one that has is. Events carry the part's and message's own times.
func TestBackfillSkipsUnchangedSessionsAndUsesStorageTimes(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join("session", "proj", "ses_w.json")
	writeSession := func(updated int64) {
		writeFixture(t, root, sessionPath, map[string]any{
			"id": "ses_w", "projectID": "proj", "directory": "/tmp/w", "title": "Watermarked",
			"time": map[string]any{"created": int64(1735689600000), "updated": updated},
		})
	}
	writeSession(1735689610000)
	writeFixture(t, root, filepath.Join("message", "ses_w", "msg_1.json"), map[string]any{
		"id": "msg_1", "sessionID": "ses_w", "role": "user", "time": map[string]any{"created": 1735689601000},
	})
	writeFixture(t, root, filepath.Join("part", "msg_1", "prt_1.json"), map[string]any{
		"id": "prt_1", "sessionID": "ses_w", "messageID": "msg_1", "type": "text", "text": "timed",
		"time": map[string]any{"start": 1735689602000, "end": 1735689603000},
	})
	writeFixture(t, root, filepath.Join("part", "msg_1", "prt_2.json"), map[string]any{
		"id": "prt_2", "sessionID": "ses_w", "messageID": "msg_1", "type": "step-finish", "reason": "stop",
	})

	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ing := opencodeingest.New(st, ingest.New(st, ingest.Options{}))
	ctx := context.Background()

	first, err := ing.BackfillAll(ctx, root)
	if err != nil || first.Sessions != 1 {
		t.Fatalf("first pass: %+v %v", first, err)
	}
	var textTS, stepTS int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE uuid = 'prt_1'`).Scan(&textTS); err != nil {
		t.Fatalf("text part: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE uuid = 'prt_2'`).Scan(&stepTS); err != nil {
		t.Fatalf("step part: %v", err)
	}
	if textTS != 1735689602000 || stepTS != 1735689601000 {
		t.Fatalf("events must carry the part's time, or its message's: text=%d step=%d", textTS, stepTS)
	}

	second, err := ing.BackfillAll(ctx, root)
	if err != nil || second.Sessions != 0 || second.Unchanged != 1 {
		t.Fatalf("an unchanged session must be skipped: %+v %v", second, err)
	}

	writeSession(1735689999000)
	if err := os.Chtimes(filepath.Join(root, sessionPath), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	third, err := ing.BackfillAll(ctx, root)
	if err != nil || third.Sessions != 1 {
		t.Fatalf("a session OpenCode wrote to since must be walked again: %+v %v", third, err)
	}
}
