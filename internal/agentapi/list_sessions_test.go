package agentapi_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// GET /sessions is the console's run list. A terminal run has no prompt, so
// the list had nothing per-row to show but the project — every named agent
// in a repo rendered as the repo's name. The name lives in the transcript,
// keyed by claude session id, and the handler joins the two.
func TestListSessionsCarriesTheNameTheTranscriptKnows(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	st, err := store.Open(filepath.Join(dir, "runs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	ingSvc := ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"})
	sp := spawner.New(st, ingSvc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := agentapi.New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")

	if err := st.UpsertDevice(ctx, &store.Device{
		ID: "dev-list", Name: "test", MachineID: "test",
		Kind: "device", TokenHash: store.HashToken("test-token"),
	}, 0); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	now := time.Now().UnixMilli()
	seedRun := func(runID, sessionID, prompt, title string) {
		t.Helper()
		if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
			ID: runID, SessionID: sessionID, Kind: "tty", Engine: "claude",
			State: "running", StartedAt: now, UpdatedAt: now,
			CWD: "/home/u/code/webapp", Project: "webapp", Prompt: prompt, Title: title,
		}); err != nil {
			t.Fatalf("seed run %s: %v", runID, err)
		}
	}
	// A terminal run: no prompt at all, which is the whole problem.
	seedRun("run_named", "sess-named", "", "")
	// A run whose transcript does not exist.
	seedRun("run_unknown", "sess-unknown", "", "")
	// A chat run that leads with its prompt.
	seedRun("run_prompted", "sess-prompted", "fix the export", "")
	// A run the engineer named at spawn: the transcript's name must not
	// replace it.
	seedRun("run_spawn_named", "sess-spawn-named", "", "RELEASE CAPTAIN")

	writeTranscript(t, dir, "/home/u/code/webapp", "sess-named",
		`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"sess-named"}`,
		`{"type":"custom-title","customTitle":"AgentFlow REVIEW","sessionId":"sess-named"}`,
	)
	writeTranscript(t, dir, "/home/u/code/webapp", "sess-spawn-named",
		`{"type":"ai-title","aiTitle":"something claude guessed","sessionId":"sess-spawn-named"}`,
	)

	out := doJSON(t, srv, "GET", "/api/v1/agentd/sessions", nil)
	items, _ := out["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("want the four seeded runs, got %d: %v", len(items), out)
	}
	byID := map[string]map[string]any{}
	for _, raw := range items {
		if m, ok := raw.(map[string]any); ok {
			byID[m["id"].(string)] = m
		}
	}

	if got := byID["run_named"]["title"]; got != "AgentFlow REVIEW" {
		t.Fatalf("the named terminal run must carry its title, got %v", got)
	}
	// Absent rather than invented: the client falls back to prompt/project,
	// and a made-up title would be worse than none.
	if got, ok := byID["run_unknown"]["title"]; ok && got != "" {
		t.Fatalf("a run without a transcript must carry no title, got %v", got)
	}
	if got := byID["run_spawn_named"]["title"]; got != "RELEASE CAPTAIN" {
		t.Fatalf("the spawn title must win over the transcript's, got %v", got)
	}
	// The join must not disturb anything the list already showed.
	if got := byID["run_prompted"]["prompt"]; got != "fix the export" {
		t.Fatalf("prompt lost, got %v", got)
	}
	if got := byID["run_named"]["state"]; got != "running" {
		t.Fatalf("state lost, got %v", got)
	}
	if got := byID["run_named"]["project"]; got != "webapp" {
		t.Fatalf("project lost, got %v", got)
	}
}

// The run page's header reads this. Before it, a resumed run was titled
// "resumed 290d8…" — the run key, which is the one thing that is never the
// name anyone gave the session. Same source as the list, so the two cannot
// disagree.
func TestSingleSessionStatusCarriesItsTitle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	st, err := store.Open(filepath.Join(dir, "one.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	ingSvc := ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"})
	sp := spawner.New(st, ingSvc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp.SetCorpusRoot(dir)
	srv := agentapi.New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")

	if err := st.UpsertDevice(ctx, &store.Device{
		ID: "dev-one", Name: "test", MachineID: "test", Kind: "device",
		TokenHash: store.HashToken("test-token"),
	}, 0); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	now := time.Now().UnixMilli()
	// A resumed terminal run: no prompt at all, which is the whole problem.
	if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
		ID: "run_resumed", SessionID: "sess-named", Kind: "tty", Engine: "claude",
		State: "running", StartedAt: now, UpdatedAt: now,
		CWD: dir, Project: "myapp", ResumeFrom: "290d8f2e-1111-2222-3333-444455556666",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	writeTranscript(t, dir, dir, "sess-named",
		`{"type":"custom-title","customTitle":"agentflow ORCHESTRATOR","sessionId":"sess-named"}`,
	)

	out := doJSON(t, srv, "GET", "/api/v1/agentd/sessions/run_resumed", nil)
	if got := out["title"]; got != "agentflow ORCHESTRATOR" {
		t.Fatalf("single-run status title = %v, want the session's name", got)
	}
	// Nothing the status response already carried is disturbed.
	if out["state"] != "running" || out["project"] != "myapp" {
		t.Fatalf("status lost fields: %v", out)
	}

	// A run whose transcript has no title record carries no title, rather than an
	// invented one — the client falls back to prompt/project itself.
	if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
		ID: "run_unknown", SessionID: "sess-nope", Kind: "tty", Engine: "claude",
		State: "running", StartedAt: now, UpdatedAt: now, CWD: dir, Project: "myapp",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	out = doJSON(t, srv, "GET", "/api/v1/agentd/sessions/run_unknown", nil)
	if got, ok := out["title"]; ok && got != "" {
		t.Fatalf("a run with no transcript title must carry none, got %v", got)
	}
}

// writeTranscript puts a Claude transcript where Claude keeps it: under
// home/.claude/projects, in the folder named after the encoded cwd.
func writeTranscript(t *testing.T, home, cwd, sessionID string, lines ...string) {
	t.Helper()
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	enc := []byte(cwd)
	for i, c := range enc {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			enc[i] = '-'
		}
	}
	folder := filepath.Join(home, ".claude", "projects", string(enc))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var body []byte
	for _, l := range lines {
		body = append(body, l...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(filepath.Join(folder, sessionID+".jsonl"), body, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}
