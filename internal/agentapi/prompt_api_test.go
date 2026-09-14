package agentapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/engine/claude"
	"github.com/arthurobo/agentflow/internal/engine/opencode"
	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// promptFixture seeds a store with one run and its attachments.
func promptFixture(t *testing.T, engineID string) (*agentapi.Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	// Sandbox: nothing in this test may touch the real corpus or home.
	t.Setenv("HOME", dir)

	st, err := store.Open(filepath.Join(dir, "prompt.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ing := ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"})
	sp := spawner.New(st, ing, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp.SetCorpusRoot(dir)
	srv := agentapi.New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	registry := engine.NewRegistry()
	registry.Register(claude.New(""))
	registry.Register(opencode.New(opencode.Config{}))
	srv.Engines = registry

	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "dev", Name: "test", MachineID: "test", Kind: "device",
		TokenHash: store.HashToken("test-token"),
	}, 0); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: "run-1", SessionID: "sess-1", Kind: "tty", Engine: engineID,
		State: "running", StartedAt: now, UpdatedAt: now, CWD: dir, Project: "webapp",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return srv, st, dir
}

func seedAttachment(t *testing.T, st *store.Store, dir, id, runID string) string {
	t.Helper()
	path := filepath.Join(dir, id+".png")
	if err := os.WriteFile(path, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := st.InsertAttachment(context.Background(), &store.Attachment{
		ID: id, RunID: runID, SessionID: "sess-1", Path: path,
		Mime: "image/png", Bytes: 11, CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	return path
}

func postPrompt(t *testing.T, s *agentapi.Server, runID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	blob, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/v1/agentd/sessions/"+runID+"/control/prompt",
		strings.NewReader(string(blob)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

// An id that belongs to another run must not resolve. This is the whole
// authorization story for attachments: the client only ever holds ids, and
// an id is only meaningful against the run that owns it.
func TestPromptRefusesAnotherRunsAttachment(t *testing.T) {
	srv, st, dir := promptFixture(t, "claude")

	// A second run, with its own file.
	now := time.Now().UnixMilli()
	if err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: "run-2", SessionID: "sess-2", Kind: "tty", Engine: "claude",
		State: "running", StartedAt: now, UpdatedAt: now, CWD: dir, Project: "webapp",
	}); err != nil {
		t.Fatalf("seed run-2: %v", err)
	}
	seedAttachment(t, st, dir, "att-theirs", "run-2")

	rr := postPrompt(t, srv, "run-1", map[string]any{
		"text": "look at this", "attachmentIds": []string{"att-theirs"},
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403 for another run's attachment, got %d %s", rr.Code, rr.Body.String())
	}

	// And an id that exists nowhere is refused the same way, rather than
	// being silently dropped from the turn.
	rr = postPrompt(t, srv, "run-1", map[string]any{
		"text": "hi", "attachmentIds": []string{"does-not-exist"},
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403 for an unknown id, got %d %s", rr.Code, rr.Body.String())
	}

	// A partial match is still a refusal: delivering fewer images than the
	// engineer attached, silently, would be worse than an error.
	seedAttachment(t, st, dir, "att-mine", "run-1")
	rr = postPrompt(t, srv, "run-1", map[string]any{
		"text": "hi", "attachmentIds": []string{"att-mine", "att-theirs"},
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403 when only some ids belong to the run, got %d", rr.Code)
	}
}

// The body carries text and ids. It is capped well below anything that could
// be a payload, because the payload has its own channel.
func TestPromptRefusesAnOversizedBody(t *testing.T) {
	srv, _, _ := promptFixture(t, "claude")

	rr := postPrompt(t, srv, "run-1", map[string]any{
		"text": strings.Repeat("A", 17<<10),
	})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413 for a 17KB body, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestPromptNeedsSomethingToSend(t *testing.T) {
	srv, _, _ := promptFixture(t, "claude")
	rr := postPrompt(t, srv, "run-1", map[string]any{"text": "   "})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an empty turn, got %d", rr.Code)
	}
}

func TestPromptRefusesTooManyAttachments(t *testing.T) {
	srv, st, dir := promptFixture(t, "claude")
	ids := []string{}
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		seedAttachment(t, st, dir, id, "run-1")
		ids = append(ids, id)
	}
	rr := postPrompt(t, srv, "run-1", map[string]any{"text": "hi", "attachmentIds": ids})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for five attachments, got %d", rr.Code)
	}
}

// The exact turn Claude gets. It is asserted character for character because
// the format is the whole delivery: the paths lead so the model has them
// before the sentence about them, and the Read tool is what opens them.
func TestAttachmentTurnFormat(t *testing.T) {
	files := []engine.Attachment{
		{ID: "a", Path: "/home/user/.agentflow/uploads/webapp/sess-1/20260906T110512Z-shot.png"},
		{ID: "b", Path: "/home/user/.agentflow/uploads/webapp/sess-1/20260906T110513Z-two.png"},
	}

	got := agentapi.AttachmentTurn(files, "why is this button misaligned?")
	want := "Attached image: /home/user/.agentflow/uploads/webapp/sess-1/20260906T110512Z-shot.png\n" +
		"Attached image: /home/user/.agentflow/uploads/webapp/sess-1/20260906T110513Z-two.png\n" +
		"\n" +
		"why is this button misaligned?"
	if got != want {
		t.Fatalf("turn mismatch:\n got %q\nwant %q", got, want)
	}

	// An empty message still says what to do with the images rather than
	// delivering a bare path and hoping.
	got = agentapi.AttachmentTurn(files[:1], "   ")
	want = "Attached image: /home/user/.agentflow/uploads/webapp/sess-1/20260906T110512Z-shot.png\n" +
		"\n" +
		"Please look at the attached image(s)."
	if got != want {
		t.Fatalf("empty-text turn mismatch:\n got %q\nwant %q", got, want)
	}
}

// Claude has no file part, so the host names the paths and the TUI's Read
// tool opens them. The response says which delivery happened, because
// "delivered" meaning two different things silently is how this gets
// debugged badly later.
func TestPromptWithAttachmentsFallsBackToPathsForClaude(t *testing.T) {
	srv, st, dir := promptFixture(t, "claude")
	path := seedAttachment(t, st, dir, "att-1", "run-1")

	rr := postPrompt(t, srv, "run-1", map[string]any{
		"text": "what is wrong here?", "attachmentIds": []string{"att-1"},
	})
	// This fixture has no live PTY, so the submit cannot land. What the
	// status proves is WHICH route was taken: a 502 from the tty hub means
	// the handler resolved the ids, skipped the native path because Claude
	// declares no file part, built the turn and tried to type it. An
	// authorization or capability error would mean it never got that far.
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("want 502 from the PTY submit, got %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no live session") {
		t.Fatalf("the failure should be the missing PTY, got %s", rr.Body.String())
	}
	// The turn itself is asserted exactly in TestAttachmentTurnFormat; here
	// we only confirm the file it would have named is the one we seeded.
	att, err := st.GetAttachment(context.Background(), "att-1")
	if err != nil || att == nil || att.Path != path {
		t.Fatalf("attachment lookup: %v %v", att, err)
	}
}

// Claude's capability mask must say it has no native file part, so the host
// knows to build the turn instead of trying and failing.
func TestClaudeReportsNoNativeAttachments(t *testing.T) {
	srv, _, _ := promptFixture(t, "claude")
	caps := doJSON(t, srv, "GET", "/api/v1/agentd/sessions/run-1/control/caps", nil)
	capMap, _ := caps["caps"].(map[string]any)
	if capMap["attachFiles"] != false {
		t.Fatalf("claude has no file part; caps.attachFiles = %v", capMap["attachFiles"])
	}
	// But it can still take a prompt, which is what the fallback rides on.
	if capMap["prompt"] != true {
		t.Fatalf("caps.prompt = %v", capMap["prompt"])
	}
}
