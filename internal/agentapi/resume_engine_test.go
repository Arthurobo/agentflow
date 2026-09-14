package agentapi_test

import (
	"context"
	"io"
	"log/slog"
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

// Tapping a dead OpenCode run re-spawned it as CLAUDE. The resume paths each
// built their own spawner.Options and set only Cwd and Model, so Engine was
// empty, resolved to claude by default, and the machine ran
// `claude --resume ses_f88c4e99...` — a session id Claude has never heard of.
// What the engineer saw was a blank terminal and Claude's resume picker
// saying "No sessions match".
//
// These tests capture the ARGV the resume actually produces, because argv is
// where the bug was visible and where a regression would be.

// argvRecorder is a stand-in engine binary that writes its own argv and exits.
// It replaces the engine's real binary so nothing is launched but the command
// line is still the real one the spawner built.
func argvRecorder(t *testing.T, dir string) (bin, out string) {
	t.Helper()
	out = filepath.Join(dir, "argv.txt")
	bin = filepath.Join(dir, "recorder.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + out + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write recorder: %v", err)
	}
	return bin, out
}

func recordedArgv(t *testing.T, out string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(out); err == nil && len(raw) > 0 {
			return strings.Split(strings.TrimSpace(string(raw)), "\n")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the engine binary was never launched, so no argv was recorded")
	return nil
}

type resumeHarness struct {
	st   *store.Store
	srv  *agentapi.Server
	out  string
	port int
}

func newResumeHarness(t *testing.T, engineID string) *resumeHarness {
	return newResumeHarnessInState(t, engineID, "stopped")
}

func newResumeHarnessInState(t *testing.T, engineID, state string) *resumeHarness {
	t.Helper()
	dir := t.TempDir()
	// Sandboxed: this spawns through a real Spawner.
	t.Setenv("HOME", dir)

	st, err := store.Open(filepath.Join(dir, "runs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	bin, out := argvRecorder(t, dir)
	const port = 47311

	reg := engine.NewRegistry()
	reg.Register(claude.New(bin))
	reg.Register(opencode.New(opencode.Config{
		Hostname: "127.0.0.1", Path: bin,
		PortAllocator: func() (int, error) { return port, nil },
	}))

	ing := ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"})
	sp := spawner.New(st, ing, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp.SetCorpusRoot(dir)
	sp.SetEngines(reg)

	srv := agentapi.New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	srv.Engines = reg

	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "dev", Name: "test", MachineID: "test", Kind: "device",
		TokenHash: store.HashToken("test-token"),
	}, 0); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	now := time.Now().UnixMilli()
	sid := "ses_f88c4e99affeXXXXXXXXXXXXXX"
	if engineID == engine.IDClaude {
		sid = "290d80eb-9150-4b56-bef3-7628b480da1a"
	}
	// A run whose process has ended: exactly what tapping a dead member hits.
	if err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: "run_dead", SessionID: sid, Kind: "tty", Engine: engineID,
		State: state, StartedAt: now, UpdatedAt: now, EndedAt: now,
		CWD: dir, Project: "demo", Title: "INVESTIGATION",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return &resumeHarness{st: st, srv: srv, out: out, port: port}
}

// The whole bug in one assertion.
func TestResumingAnOpenCodeRunSpawnsOpenCodeOnItsOwnSession(t *testing.T) {
	h := newResumeHarness(t, engine.IDOpenCode)

	doJSON(t, h.srv, "POST", "/api/v1/agentd/sessions/run_dead/tty?restart=1", nil)
	argv := recordedArgv(t, h.out)
	joined := strings.Join(argv, " ")

	// OpenCode reopens with --session. Claude's flag appearing here IS the
	// bug: it is what produced "No sessions match ses_f88c4e99...".
	if !strings.Contains(joined, "--session ses_f88c4e99affeXXXXXXXXXXXXXX") {
		t.Fatalf("the resume must reopen the OpenCode session: %v", argv)
	}
	if strings.Contains(joined, "--resume") {
		t.Fatalf("an OpenCode resume must never use claude's flag: %v", argv)
	}
	// And the control port has to be there, or the run binds nothing and
	// sits in "starting" forever.
	if !strings.Contains(joined, "--port 47311") {
		t.Fatalf("the resume must carry the allocated control port: %v", argv)
	}

	// The row records the engine it actually ran as, not the default.
	m, err := h.st.GetManagedSession(context.Background(), "run_dead")
	if err != nil || m == nil {
		t.Fatalf("row: %+v %v", m, err)
	}
	if m.Engine != engine.IDOpenCode {
		t.Fatalf("row engine = %q, want opencode", m.Engine)
	}
	if m.ControlPort != 47311 {
		t.Fatalf("row control port = %d, want the allocated one", m.ControlPort)
	}
}

// The claude path is unchanged: no port, and its own resume flag.
func TestResumingAClaudeRunIsUnchanged(t *testing.T) {
	h := newResumeHarness(t, engine.IDClaude)

	doJSON(t, h.srv, "POST", "/api/v1/agentd/sessions/run_dead/tty?restart=1", nil)
	argv := recordedArgv(t, h.out)
	joined := strings.Join(argv, " ")

	if !strings.Contains(joined, "--resume 290d80eb-9150-4b56-bef3-7628b480da1a") {
		t.Fatalf("a claude resume keeps its own flag: %v", argv)
	}
	if strings.Contains(joined, "--port") {
		t.Fatalf("claude has no control API and must be allocated no port: %v", argv)
	}
}

// The by-session bridge is the other door to the same room: the phone opens a
// terminal for a session id, not a run id. The run ended on its own; one that
// was stopped on purpose stays stopped (see the tty lifecycle tests).
func TestResumingAnOpenCodeSessionByIDAlsoSpawnsOpenCode(t *testing.T) {
	h := newResumeHarnessInState(t, engine.IDOpenCode, "finished")

	doJSON(t, h.srv, "POST",
		"/api/v1/agentd/sessions/by-session/ses_f88c4e99affeXXXXXXXXXXXXXX/tty", nil)
	argv := recordedArgv(t, h.out)
	joined := strings.Join(argv, " ")

	if !strings.Contains(joined, "--session ses_f88c4e99affeXXXXXXXXXXXXXX") ||
		!strings.Contains(joined, "--port 47311") {
		t.Fatalf("the by-session bridge must resume on the right engine: %v", argv)
	}
	if strings.Contains(joined, "--resume") {
		t.Fatalf("claude's flag must not appear: %v", argv)
	}
}

// POST /resume makes a NEW run key from an old session. Same bug, third door.
func TestTheResumeEndpointCarriesTheEngineToo(t *testing.T) {
	h := newResumeHarness(t, engine.IDOpenCode)

	doJSON(t, h.srv, "POST", "/api/v1/agentd/sessions/run_dead/resume", map[string]any{})
	argv := recordedArgv(t, h.out)
	joined := strings.Join(argv, " ")

	if !strings.Contains(joined, "--session ses_f88c4e99affeXXXXXXXXXXXXXX") {
		t.Fatalf("POST /resume must reopen the OpenCode session: %v", argv)
	}
	if strings.Contains(joined, "--resume") {
		t.Fatalf("claude's flag must not appear: %v", argv)
	}
}
