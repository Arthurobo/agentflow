package agentapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// newTestServer wires the real store + spawner (in-memory db file) under the
// agent control API, registering one active device for bearer auth.
func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := ingest.New(st, ingest.Options{Live: false})
	sp := spawner.New(st, svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "dev-1", Name: "test", MachineID: "m-1", Kind: "device",
		Status: "active", TokenHash: store.HashToken("secret-token"),
	}, 0); err != nil {
		t.Fatalf("upsert device: %v", err)
	}
	return New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "m-1"), st
}

func doGet(t *testing.T, srv *Server, path string, want int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s: status = %d, want %d (body %s)", path, rec.Code, want, rec.Body.String())
	}
	return rec
}

func TestRunBySession(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	// unknown session id -> 404
	doGet(t, srv, "/api/v1/agentd/runs/by-session/UNKNOWN", http.StatusNotFound)

	// a stored managed run owning a claude session id -> bridged run
	if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
		ID: "run-1", SessionID: "SID-123", Kind: "chat", Prompt: "hi",
		State: string(spawner.StateFinished), StartedAt: 1, EndedAt: 2,
	}); err != nil {
		t.Fatalf("upsert managed session: %v", err)
	}
	rec := doGet(t, srv, "/api/v1/agentd/runs/by-session/SID-123", http.StatusOK)
	var body struct {
		Run struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionId"`
			State     string `json:"state"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Run.ID != "run-1" || body.Run.SessionID != "SID-123" || body.Run.State != "finished" {
		t.Fatalf("unexpected run: %+v", body.Run)
	}
}

func TestRunBySessionRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agentd/runs/by-session/SID-123", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestRecoverSeamRemoved pins the single-token contract (0012): the recovery
// endpoint is gone — a lost device token can only be replaced by a fresh
// `agentd pair` pairing token.
func TestRecoverSeamRemoved(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/v1/agentd/devices/recover",
		strings.NewReader(`{"code":"ABCD-EFGH-JKLM-NPQR"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("recover seam must be gone, got %d", rec.Code)
	}
}

// TestPairingTokenIsOneShot pins the new one-shot contract: a pairing
// token is consumed on first successful pair; subsequent pairs with the
// same token are 401 even well within the 15-minute window. The previous
// "one printed token enrolls every device for 30 days" behaviour is still
// available via `agentflow pair --reusable` (see TestPairingTokenReusable
// when the relay-bootstrap path needs reuse).
func TestPairingTokenIsOneShot(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()
	svc := ingest.New(st, ingest.Options{Live: false})
	sp := spawner.New(st, svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "m-test")

	const pairTok = "pair-one-shot"
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "p1", Kind: "pairing", Status: "pending",
		TokenHash: store.HashToken(pairTok),
	}, time.Now().Add(15*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// First pair: consumes the slot.
	req := httptest.NewRequest("POST", "/api/v1/agentd/pair/complete",
		strings.NewReader(`{"token":"`+pairTok+`","name":"dev-a"}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("first pair must succeed (one-shot token), got %d: %s", w.Code, w.Body.String())
	}

	// Second pair, same token: now revoked, must 401.
	req = httptest.NewRequest("POST", "/api/v1/agentd/pair/complete",
		strings.NewReader(`{"token":"`+pairTok+`","name":"dev-b"}`))
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("pairing token must be one-shot; second pair got %d: %s", w.Code, w.Body.String())
	}
}

// A loop member's terminal is not an anonymous session.
//
// Every member row in managed_sessions has an empty prompt, because StartTTY
// builds the Session without one, and managed_sessions has no title column at
// all. The name lived in memory, in the --name argv and in the corpus title
// record, none of which this page reads, so the header said "Terminal session"
// for every member of every loop and the subtitle showed the loop cwd, which
// is identical for all of them.
func TestStatusCarriesTheLoopAMemberBelongsTo(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	loop := &store.Loop{Title: "Flaky guest export", Task: "Find why it goes flaky above 500 rows", CWD: "/tmp/w"}
	if _, err := st.CreateCrew(ctx, loop, []store.MemberSpec{
		{Role: store.RoleOrchestrator, RunID: "run_orch"},
		{Role: "INVESTIGATION", RunID: "run_inv"},
	}); err != nil {
		t.Fatalf("crew: %v", err)
	}
	if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
		ID: "run_orch", Kind: "tty", CWD: "/tmp/w", State: "running",
	}); err != nil {
		t.Fatalf("session: %v", err)
	}

	rec := doGet(t, srv, "/api/v1/agentd/sessions/run_orch", http.StatusOK)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The session shape every existing caller parses is untouched.
	if out["id"] != "run_orch" || out["state"] != "running" {
		t.Fatalf("the session fields must survive: %v", out)
	}
	ctxOut, _ := out["loop"].(map[string]any)
	if ctxOut == nil {
		t.Fatalf("a member run must carry its loop: %v", out)
	}
	if ctxOut["role"] != store.RoleOrchestrator {
		t.Fatalf("role: %v", ctxOut["role"])
	}
	if ctxOut["title"] != "Flaky guest export" {
		t.Fatalf("the loop's NAME is what the header shows, got %v", ctxOut["title"])
	}
	if ctxOut["id"] != loop.ID {
		t.Fatalf("the back link needs the loop id, got %v", ctxOut["id"])
	}
}

// An ordinary run is not a loop member and must carry nothing extra, or every
// terminal would grow a loop header it does not belong to.
func TestStatusOfAnOrdinaryRunCarriesNoLoop(t *testing.T) {
	srv, st := newTestServer(t)
	if err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: "run_solo", Kind: "tty", CWD: "/tmp/w", State: "running", Prompt: "fix the thing",
	}); err != nil {
		t.Fatalf("session: %v", err)
	}
	rec := doGet(t, srv, "/api/v1/agentd/sessions/run_solo", http.StatusOK)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := out["loop"]; present {
		t.Fatalf("a run form session belongs to no loop: %v", out["loop"])
	}
	if out["prompt"] != "fix the thing" {
		t.Fatalf("its own prompt is still its name: %v", out["prompt"])
	}
}
