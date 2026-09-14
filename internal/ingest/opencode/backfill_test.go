package opencode_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/arthurobo/agentflow/internal/ingest"
	opencodeingest "github.com/arthurobo/agentflow/internal/ingest/opencode"
	"github.com/arthurobo/agentflow/internal/store"
)

// recorderSink captures every batch the ingest package hands to the writer
// interface. The test asserts on the aggregate (sessions, parts, tools).
type recorderSink struct {
	batches [][]store.Incoming
}

func (r *recorderSink) EnqueueLive(ctx context.Context, evs []store.Incoming, _ *store.SessionMeta) error {
	// copy so later tests can't see this batch's mutations
	cp := make([]store.Incoming, len(evs))
	copy(cp, evs)
	r.batches = append(r.batches, cp)
	return nil
}

// writeFixture writes a JSON fixture under root/<rel>.
func writeFixture(t *testing.T, root, rel string, body any) {
	t.Helper()
	dir := filepath.Dir(filepath.Join(root, rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b, _ := json.Marshal(body)
	if err := os.WriteFile(filepath.Join(root, rel), b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestBackfillAll_WalksAndEmits pins the end-to-end adoption flow: the
// on-disk storage tree gets normalized through the same path SSE events
// use, so a brand-new agentd can rebuild the OpenCode index without
// double-writing (the writer's dedupe key is (engine, sessionID, file)).
func TestBackfillAll_WalksAndEmits(t *testing.T) {
	root := t.TempDir()
	projectID := "testproject1234"
	sessionID := "ses_test_001"
	// session
	writeFixture(t, root, filepath.Join("session", projectID, sessionID+".json"), map[string]any{
		"id":        sessionID,
		"projectID": projectID,
		"directory": "/tmp/demo",
		"title":     "Demo session",
		"time":      map[string]any{"created": int64(1735689600000), "updated": int64(1735689610000)},
	})
	// message + parts
	msgID := "msg_test_001"
	writeFixture(t, root, filepath.Join("message", sessionID, msgID+".json"), map[string]any{
		"id":        msgID,
		"sessionID": sessionID,
		"role":      "user",
		"time":      map[string]any{"created": 1735689601000},
	})
	writeFixture(t, root, filepath.Join("part", msgID, "prt_text_001.json"), map[string]any{
		"id":        "prt_text_001",
		"sessionID": sessionID,
		"messageID": msgID,
		"type":      "text",
		"text":      "hello from opencode",
	})
	writeFixture(t, root, filepath.Join("part", msgID, "prt_step_001.json"), map[string]any{
		"id":        "prt_step_001",
		"sessionID": sessionID,
		"messageID": msgID,
		"type":      "step-finish",
		"reason":    "stop",
		"cost":      0.0007,
		"tokens": map[string]any{
			"total": 50, "input": 30, "output": 20, "reasoning": 0,
		},
	})

	// The backfill writes through the shared single writer; BackfillAll
	// waits for it to commit, so the events can be read back straight away.
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ing := opencodeingest.New(st, ingest.New(st, ingest.Options{}))

	report, err := ing.BackfillAll(context.Background(), root)
	if err != nil {
		t.Fatalf("BackfillAll: %v", err)
	}
	if report.Sessions != 1 {
		t.Fatalf("sessions: %d, want 1", report.Sessions)
	}
	// verify the adopted session is in managed_sessions (so the agentd
	// runs list shows it) AND the corpus events are linked to a session
	// row (so the sessions list can show them).
	if got := countEventsForSession(st, sessionID); got != 4 {
		t.Fatalf("events for %s: %d, want 4 (session_state + user_message + text + step-finish)", sessionID, got)
	}

	// assert one of the events is a step-finish carrying cost + tokens.
	// Read it back via the store's API (counts of cost across events).
	var stepFinishCost float64
	var stepFinishIn, stepFinishOut int64
	if err := st.DB().QueryRow(
		`SELECT cost_usd, tokens_in, tokens_out FROM events
		 WHERE session_id = ? AND event = 'result' LIMIT 1`,
		sessionID,
	).Scan(&stepFinishCost, &stepFinishIn, &stepFinishOut); err != nil {
		t.Fatalf("query step-finish event: %v", err)
	}
	if stepFinishCost <= 0 {
		t.Fatalf("step-finish cost: %v, want > 0", stepFinishCost)
	}
	if stepFinishIn != 30 || stepFinishOut != 20 {
		t.Fatalf("step-finish tokens: in=%d out=%d, want 30/20", stepFinishIn, stepFinishOut)
	}
}

// TestBackfillAll_NoStorageIsNotAnError covers the "fresh install" case:
// the OpenCode storage directory may not exist on a brand-new machine.
func TestBackfillAll_NoStorageIsNotAnError(t *testing.T) {
	sink := &recorderSink{}
	ing := opencodeingest.New(nil, sink)
	rep, err := ing.BackfillAll(context.Background(), "/nonexistent-storage-path-x9")
	if err != nil {
		t.Fatalf("BackfillAll: %v", err)
	}
	if rep.Sessions != 0 {
		t.Fatalf("sessions: %d, want 0", rep.Sessions)
	}
}

// TestBackfillAll_AdoptsManagedSessions pins the agentd runs-list adoption
// behavior ("no opencode sessions in the runs list"): every
// on-disk OpenCode session gets a managed_sessions row stamped with the
// adopted marker so /api/v1/agentd/sessions surfaces them. The control
// sheet stays greyed out (control_port=0) until the user re-spawns.
func TestBackfillAll_AdoptsManagedSessions(t *testing.T) {
	root := t.TempDir()
	projectID := "testproject1234"
	sessionID := "ses_adopt_me"
	writeFixture(t, root, filepath.Join("session", projectID, sessionID+".json"), map[string]any{
		"id":        sessionID,
		"projectID": projectID,
		"directory": "/tmp/adoptme",
		"title":     "Adopted Session",
		"time":      map[string]any{"created": int64(1735689600000), "updated": int64(1735689700000)},
	})

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "adopt.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ing := opencodeingest.New(st, ingest.New(st, ingest.Options{}))
	rep, err := ing.BackfillAll(context.Background(), root)
	if err != nil {
		t.Fatalf("BackfillAll: %v", err)
	}
	if rep.Sessions != 1 {
		t.Fatalf("sessions: %d, want 1", rep.Sessions)
	}

	// the row must exist with the adopted marker + control_port=0
	// (no live TUI bound; the control sheet greys out until re-spawned)
	row, err := st.GetManagedSession(context.Background(), "opencode-adopted-"+sessionID)
	if err != nil {
		t.Fatalf("GetManagedSession: %v", err)
	}
	if row == nil {
		t.Fatalf("adopted row missing")
	}
	if row.Engine != "opencode" {
		t.Fatalf("engine: %q, want opencode", row.Engine)
	}
	if row.SessionID != sessionID {
		t.Fatalf("session_id: %q, want %q", row.SessionID, sessionID)
	}
	if row.ControlPort != 0 {
		t.Fatalf("control_port: %d, want 0", row.ControlPort)
	}
	if row.CreatedBy != opencodeingest.AdoptMarkerOf() {
		t.Fatalf("created_by: %q, want %q", row.CreatedBy, opencodeingest.AdoptMarkerOf())
	}
	if row.State != "stopped" {
		t.Fatalf("state: %q, want stopped", row.State)
	}
	if row.Prompt != "Adopted Session" {
		t.Fatalf("prompt: %q, want %q (the title)", row.Prompt, "Adopted Session")
	}
	if row.TerminalReason != "adopted at agentd boot from on-disk storage" {
		t.Fatalf("terminal_reason: %q", row.TerminalReason)
	}
	if row.StartedAt == 0 || row.EndedAt == 0 {
		t.Fatalf("started_at=%d ended_at=%d (both must be non-zero — OpenCode stores them under time.created/updated)",
			row.StartedAt, row.EndedAt)
	}

	// The sessions list joins events → sessions on
	// session_id. For the OpenCode session to show up there, the
	// sessions table must have a row matching the events' session_id.
	// SessionMetaFromSession populates SessionID (the on-disk OpenCode
	// session id) so InsertBatch's session upsert links events to the
	// right row.
	if !sessionRowExists(t, st, sessionID) {
		t.Fatalf("sessions table missing row for adopted session %s — the sessions list won't show it", sessionID)
	}

	// A second backfill finds the session unchanged and leaves its row be.
	rep2, err := ing.BackfillAll(context.Background(), root)
	if err != nil {
		t.Fatalf("BackfillAll (rerun): %v", err)
	}
	if rep2.Sessions != 0 || rep2.Unchanged != 1 {
		t.Fatalf("rerun: %+v, want the session skipped as unchanged", rep2)
	}
	if row2, _ := st.GetManagedSession(context.Background(), "opencode-adopted-"+sessionID); row2 == nil {
		t.Fatal("the adopted row must still be there")
	}
}

// countEventsForSession counts how many events landed in the events table
// for a given session_id — used to assert the backfill linked the
// adopted session to the events the SSE path would have produced.
func countEventsForSession(st *store.Store, sessionID string) int {
	var n int
	err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE session_id = ?`, sessionID).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

// sessionRowExists asserts that the sessions table has a row for the
// given session_id. Required so the sessions list can show
// the OpenCode session.
func sessionRowExists(t *testing.T, st *store.Store, sessionID string) bool {
	t.Helper()
	row, err := st.GetSession(context.Background(), sessionID)
	if err != nil || row == nil {
		return false
	}
	return true
}
