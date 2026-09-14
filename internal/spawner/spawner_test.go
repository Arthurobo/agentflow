package spawner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
	"github.com/arthurobo/agentflow/internal/claudelog/parse"
	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/engine/claude"
	"github.com/arthurobo/agentflow/internal/engine/opencode"
	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/store"
)

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// --- fake claude child ----------------------------------------------------------

// fakeClaude is a scriptable stand-in for the claude binary: the test feeds it
// stream-json lines via emit(); whatever the spawner writes to its stdin is
// captured for envelope assertions; Signal calls are recorded. exit() closes
// stdout (EOF for the pump) and the fake's wait goroutine reports the result
// to the spawner's Waitch.
type fakeClaude struct {
	argv []string
	cwd  string
	env  []string

	outW *io.PipeWriter
	outR *io.PipeReader
	inW  *io.PipeWriter
	inR  *io.PipeReader

	mu      sync.Mutex
	written syncBuilder
	signals []os.Signal
	waitch  chan error
	exitErr error
}

// syncBuilder is a mutex-guarded strings.Builder (the drain goroutine writes,
// assertions read).
type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (sb *syncBuilder) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.b.Write(p)
}

func (sb *syncBuilder) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.b.String()
}

func newFakeClaude(argv []string, cwd string, env []string) *fakeClaude {
	f := &fakeClaude{argv: argv, cwd: cwd, env: env}
	f.outR, f.outW = io.Pipe()
	f.inR, f.inW = io.Pipe()
	go func() {
		_, _ = io.Copy(&f.written, f.inR)
	}()
	return f
}

func (f *fakeClaude) emit(line string) {
	_, _ = f.outW.Write([]byte(line + "\n"))
}

// exit closes the child's stdout (EOF for the pump) and reports waitErr to
// the spawner over Waitch. The pump reads stdout exclusively; nothing else may
// read outR.
func (f *fakeClaude) exit(waitErr error) {
	f.mu.Lock()
	f.exitErr = waitErr
	f.mu.Unlock()
	_ = f.outW.Close()
	_ = f.inW.Close()
	select {
	case f.waitch <- waitErr:
	default:
	}
}

func (f *fakeClaude) input() string { return f.written.String() }

func (f *fakeClaude) gotSignals() []os.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]os.Signal(nil), f.signals...)
}

// fakeRunner implements Runner and hands out one fakeClaude per Start.
type fakeRunner struct {
	mu    sync.Mutex
	next  chan *fakeClaude
	calls int
}

func (r *fakeRunner) Start(argv []string, cwd string, env []string) (Child, error) {
	f := newFakeClaude(argv, cwd, env)
	child := Child{
		Process: &os.Process{Pid: 12345},
		Stdin:   f.inW,
		Stdout:  f.outR,
		Stderr:  io.NopCloser(strings.NewReader("")),
		Waitch:  make(chan error, 1),
		Signal: func(sig os.Signal) error {
			f.mu.Lock()
			f.signals = append(f.signals, sig)
			f.mu.Unlock()
			return nil
		},
	}
	f.waitch = child.Waitch
	r.mu.Lock()
	r.calls++
	select {
	case r.next <- f:
	default:
	}
	r.mu.Unlock()
	return child, nil
}

var _ Runner = (*fakeRunner)(nil)

// newScriptRunner returns a runner whose child is delivered on the channel so
// tests can feed fixtures and inspect argv/stdin/signals.
func newScriptRunner() (*fakeRunner, chan *fakeClaude) {
	r := &fakeRunner{next: make(chan *fakeClaude, 1)}
	return r, r.next
}

// --- fixture data ----------------------------------------------------------------

var (
	initLine = `{"type":"system","subtype":"init","cwd":"/tmp/ws","session_id":"SESSION-ONE","model":"claude-fable-5","permissionMode":"default","tools":["Bash","Read","Edit"],"claude_code_version":"2.1.235"}`

	rateLine = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning"}}`

	assistantLine = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello phase 4"}],"usage":{"input_tokens":3,"output_tokens":2},"model":"claude-fable-5"},"session_id":"SESSION-ONE","uuid":"u-assistant-1","timestamp":"2026-08-19T10:00:00.000Z"}`

	resultLine = `{"type":"result","is_error":false,"subtype":"success","terminal_reason":"completed","total_cost_usd":0.1,"result":"hello phase 4","session_id":"SESSION-ONE","uuid":"u-result-1","duration_ms":1000,"ttft_ms":900}`

	deniedLine = `{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"toolu_01DENY","decision_reason_type":"rule","message":"Claude requested permissions to use Bash, but you haven't granted it yet.","uuid":"u-deny-1","session_id":"SESSION-ONE"}`
)

// --- helpers -----------------------------------------------------------------------

var testOpts = func() Options {
	return Options{InitTimeout: 2 * time.Second}
}

func newTestSpawner(t *testing.T) (*Spawner, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := ingest.New(st, ingest.Options{Live: false})
	return New(st, svc, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

type fakeExitError struct {
	code int
}

func (e *fakeExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *fakeExitError) ExitCode() int { return e.code }

// startAsync kicks off sp.Start in a goroutine and returns channels for the
// resulting session/error plus the live fake child (delivered as soon as the
// runner spawns). Tests feed the child's fixtures to unblock Start, then read
// the session.
func startAsync(t *testing.T, sp *Spawner, opts Options, next chan *fakeClaude) (chan *Session, chan error, *fakeClaude) {
	t.Helper()
	sessC := make(chan *Session, 1)
	errC := make(chan error, 1)
	go func() {
		s, err := sp.Start(context.Background(), opts)
		sessC <- s
		errC <- err
	}()
	var child *fakeClaude
	select {
	case child = <-next:
	case <-time.After(3 * time.Second):
		t.Fatal("runner never delivered a child")
	}
	return sessC, errC, child
}

func awaitStart(t *testing.T, sessC chan *Session, errC chan error) *Session {
	t.Helper()
	var sess *Session
	select {
	case s := <-sessC:
		sess = s
	case <-time.After(3 * time.Second):
		t.Fatal("Start never returned")
	}
	if err := <-errC; err != nil {
		t.Fatalf("start: %v", err)
	}
	return sess
}

// --- tests ------------------------------------------------------------------------

// TestBuildArgs covers the claude argv contract (ordering is load-bearing —
// FLAG-VERIFIED live: chat has NO positional prompt [], one-shot
// puts the prompt immediately after -p [], and value flags follow).
func TestBuildArgs(t *testing.T) {
	args, err := buildArgs(Options{
		Kind:           KindChat,
		Model:          "claude-opus-5",
		AllowedTools:   []string{"Read", "Bash"},
		PermissionMode: "acceptEdits",
		Effort:         "high",
		Prompt:         "do work",
	})
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	join := strings.Join(args, " ")
	for _, want := range []string{
		"-p", "--input-format", "stream-json",
		"--output-format", "stream-json", "--verbose",
		"--model", "claude-opus-5",
		"--allowedTools", "Read,Bash",
		"--permission-mode", "acceptEdits",
		"--effort", "high",
	} {
		if !strings.Contains(join, want) {
			t.Errorf("argv missing %q: %s", want, join)
		}
	}
	if strings.Contains(join, "do work") {
		t.Errorf("chat argv must NOT carry a positional prompt (the stdin envelope carries the turn): %s", join)
	}
	if strings.Contains(join, "--resume") {
		t.Errorf("no resume expected: %s", join)
	}

	// one-shot: prompt immediately after -p
	osArgs, _ := buildArgs(Options{Kind: KindOneShot, Prompt: "hello", Model: "m"})
	osJoin := strings.Join(osArgs, " ")
	if !strings.HasPrefix(osJoin, "-p hello ") {
		t.Fatalf("one-shot prompt must follow -p directly: %s", osJoin)
	}

	// resume flag survives alongside the prompt-first ordering
	rsArgs, _ := buildArgs(Options{Kind: KindOneShot, ResumeSessionID: "abc-123", Prompt: "x"})
	rsJoin := strings.Join(rsArgs, " ")
	if !strings.HasPrefix(rsJoin, "-p x ") {
		t.Fatalf("resume prompt must follow -p: %s", rsJoin)
	}
	if !strings.Contains(rsJoin, `--resume abc-123`) {
		t.Errorf("resume flag missing: %v", rsArgs)
	}

	// additional args (--mcp-config etc.) come after the standard flags
	extra, _ := buildArgs(Options{Kind: KindChat, Prompt: "p", AdditionalArgs: []string{"--mcp-config", "/tmp/cfg.json"}})
	extraJoin := strings.Join(extra, " ")
	if !strings.Contains(extraJoin, "--output-format stream-json --verbose --dangerously-skip-permissions --mcp-config /tmp/cfg.json") {
		t.Fatalf("additional args not after standard flags: %s", extraJoin)
	}

	// product default: full autonomy on every run unless explicitly scoped —
	// an empty PermissionMode must yield --dangerously-skip-permissions for
	// both kinds, and an explicit mode still passes through as a mode flag.
	for _, kind := range []Kind{KindChat, KindOneShot} {
		dflt, err := buildArgs(Options{Kind: kind, Prompt: "x"})
		if err != nil {
			t.Fatalf("buildArgs(%s): %v", kind, err)
		}
		if !strings.Contains(strings.Join(dflt, " "), "--dangerously-skip-permissions") {
			t.Errorf("%s: default argv must be full autonomy: %v", kind, dflt)
		}
	}
	pmArgs, _ := buildArgs(Options{Kind: KindOneShot, Prompt: "x", PermissionMode: "plan"})
	if strings.Contains(strings.Join(pmArgs, " "), "--dangerously-skip-permissions") {
		t.Errorf("explicit mode must not add skip-permissions: %v", pmArgs)
	}
}

// TestEnvelopeFormat checks the user-turn envelope byte-for-byte.
func TestEnvelopeFormat(t *testing.T) {
	want := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello mid"}]}}` + "\n"
	if got := userEnvelope("hello mid"); got != want {
		t.Fatalf("envelope mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestOneShotLifecycle replays a full one-shot fixture: init -> rate_limit ->
// assistant -> result -> exit 0; the session finishes with cost/terminal
// reason captured and events landed in the shared store.
func TestOneShotLifecycle(t *testing.T) {
	sp, st := newTestSpawner(t)
	runner, next := newScriptRunner()
	sp.SetRunner(runner)

	ctx := context.Background()
	opts := testOpts()
	opts.Kind = KindOneShot
	opts.Cwd = "/tmp/ws"
	opts.Prompt = "hello"
	sessC, errC, child := startAsync(t, sp, opts, next)

	child.emit(initLine)
	child.emit(rateLine)
	child.emit(assistantLine)
	child.emit(resultLine)
	child.exit(nil)

	sess := awaitStart(t, sessC, errC)
	if sess.SessionID != "SESSION-ONE" {
		t.Fatalf("session id not captured from init: %q", sess.SessionID)
	}

	fin, err := sp.Wait(ctx, sess.ID)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if fin.State != StateFinished {
		t.Fatalf("expected finished, got %s (%s)", fin.State, fin.LastError)
	}
	if fin.TotalCostUSD != 0.1 {
		t.Errorf("cost: got %v", fin.TotalCostUSD)
	}
	if fin.TerminalReason != "completed" {
		t.Errorf("terminalReason: got %q", fin.TerminalReason)
	}
	if fin.EventCount < 3 {
		t.Errorf("expected >=3 events ingested, got %d", fin.EventCount)
	}

	// The store write is asynchronous: flush hands the batch to ingest's
	// single writer goroutine over a channel and returns, so the process
	// exiting says nothing about the rows being committed. Poll, the way the
	// chat test below already does.
	//
	// Reading it straight used to pass only because the store pinned itself
	// to one connection, which parked this SELECT behind the very insert it
	// was waiting on. The pool is four connections now, on purpose, so a
	// reader no longer queues behind a writer and the missing wait shows.
	deadline := time.Now().Add(5 * time.Second)
	var n int64
	for time.Now().Before(deadline) {
		if n, err = st.EventCount(ctx, "SESSION-ONE"); err == nil && n >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || n < 3 {
		t.Errorf("store has %d events for session (err %v), want >=3", n, err)
	}
}

// TestChatEnvelopeRoundTrip runs a persistent chat session, verifies the
// initial prompt envelope and a mid-run SendMessage envelope on stdin, and
// that turns stream into events. Closes stdin for the clean-exit path.
func TestChatEnvelopeRoundTrip(t *testing.T) {
	sp, st := newTestSpawner(t)
	runner, next := newScriptRunner()
	sp.SetRunner(runner)

	ctx := context.Background()
	opts := testOpts()
	opts.Kind = KindChat
	opts.Cwd = "/tmp/ws"
	opts.Prompt = "first prompt"
	sessC, errC, child := startAsync(t, sp, opts, next)

	child.emit(initLine)

	sess := awaitStart(t, sessC, errC)

	// initial prompt should have been written as the user envelope
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(child.input(), "first prompt") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(child.input(), `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"first prompt"}]}}`) {
		t.Fatalf("initial envelope missing; stdin=%q", child.input())
	}

	child.emit(rateLine)
	child.emit(assistantLine)
	child.emit(resultLine)

	if err := sp.SendMessage(ctx, sess.ID, "please continue"); err != nil {
		t.Fatalf("send message: %v", err)
	}
	if env := child.input(); !strings.Contains(env, `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"please continue"}]}}`) {
		t.Fatalf("mid-run envelope missing; stdin=%q", env)
	}

	// second turn + re-init
	child.emit(initLine)
	child.emit(assistantLine)
	child.emit(resultLine)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := st.EventCount(ctx, "SESSION-ONE"); n >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	n, _ := st.EventCount(ctx, "SESSION-ONE")
	if n < 3 {
		t.Errorf("expected >=3 events ingested for chat session, got %d", n)
	}

	if err := sp.CloseStdin(ctx, sess.ID); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	child.exit(nil)
	fin, err := sp.Wait(ctx, sess.ID)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if fin.State != StateStopped {
		t.Fatalf("expected stopped after stdin close, got %s", fin.State)
	}
}

// TestSigintRouting asserts the Stop button route sends the raw signal to the
// child process (recorder injected) without killing the spawner.
func TestSigintRouting(t *testing.T) {
	sp, _ := newTestSpawner(t)
	runner, next := newScriptRunner()
	sp.SetRunner(runner)

	ctx := context.Background()
	opts := testOpts()
	opts.Kind = KindChat
	opts.Cwd = "/tmp/ws"
	opts.Prompt = "p"
	sessC, errC, child := startAsync(t, sp, opts, next)
	child.emit(initLine)
	sess := awaitStart(t, sessC, errC)

	if err := sp.Interrupt(ctx, sess.ID, os.Interrupt); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	sigs := child.gotSignals()
	if len(sigs) != 1 || sigs[0] != os.Interrupt {
		t.Fatalf("expected SIGINT routed to child, got %v", sigs)
	}

	child.exit(nil)
	_, _ = sp.Wait(ctx, sess.ID)
	if err := sp.Interrupt(ctx, sess.ID, os.Interrupt); err == nil {
		t.Fatalf("interrupting a terminal session must error")
	}
}

// TestCrashResumable simulates an abnormal exit (non-zero) and asserts the run
// is marked crashed and preserves the session id so the UI can offer --resume.
func TestCrashResumable(t *testing.T) {
	sp, _ := newTestSpawner(t)
	runner, next := newScriptRunner()
	sp.SetRunner(runner)

	ctx := context.Background()
	opts := testOpts()
	opts.Kind = KindChat
	opts.Cwd = "/tmp/ws"
	opts.Prompt = "p"
	sessC, errC, child := startAsync(t, sp, opts, next)
	child.emit(initLine)
	sess := awaitStart(t, sessC, errC)

	child.emit(rateLine)
	child.emit(assistantLine)
	child.exit(&fakeExitError{code: 2})

	fin, err := sp.Wait(ctx, sess.ID)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if fin.State != StateCrashed {
		t.Fatalf("expected crashed, got %s", fin.State)
	}
	if fin.SessionID != "SESSION-ONE" {
		t.Errorf("session id lost on crash: %q", fin.SessionID)
	}
	list, err := sp.List(ctx, "crashed")
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 crashed run listed, got %d (%v)", len(list), err)
	}
	if list[0].State != StateCrashed || list[0].ExitCode != 2 {
		t.Errorf("unexpected persisted crash row: %+v", list[0])
	}
}

// TestPermissionDenialSurfacing feeds a permission_denied stream event and
// asserts the run transitions to awaiting/blocked, then back to running when
// the model continues.
func TestPermissionDenialSurfacing(t *testing.T) {
	sp, _ := newTestSpawner(t)
	runner, next := newScriptRunner()
	sp.SetRunner(runner)

	ctx := context.Background()
	opts := testOpts()
	opts.Kind = KindChat
	opts.Cwd = "/tmp/ws"
	opts.Prompt = "p"
	sessC, errC, child := startAsync(t, sp, opts, next)
	child.emit(initLine)
	sess := awaitStart(t, sessC, errC)

	child.emit(deniedLine)
	waitForState(t, sp, sess.ID, StateAwaiting)
	cur := sp.copySession(sess.ID)
	if !cur.Blocked || cur.PermissionDenial != 1 {
		t.Fatalf("expected blocked + 1 denial while awaiting, got %+v", cur)
	}

	child.emit(assistantLine)
	waitForState(t, sp, sess.ID, StateRunning)
	cur = sp.copySession(sess.ID)
	if cur.Blocked {
		t.Fatalf("expected not blocked after model continues")
	}

	child.exit(nil)
	_, _ = sp.Wait(ctx, sess.ID)
}

// waitForState polls the in-memory session until it reaches want (or times out).
func waitForState(t *testing.T, sp *Spawner, runID string, want State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cur := sp.copySession(runID); cur.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s never reached %s (last %s)", runID, want, sp.copySession(runID).State)
}

// TestResumeReusesSession verifies that resuming with --resume <uuid> passes
// the flag through and records resume_from.
func TestResumeReusesSession(t *testing.T) {
	sp, _ := newTestSpawner(t)
	runner, next := newScriptRunner()
	sp.SetRunner(runner)

	ctx := context.Background()
	opts := testOpts()
	opts.Kind = KindChat
	opts.Cwd = "/tmp/ws"
	opts.Prompt = "continue"
	opts.ResumeSessionID = "SESSION-ONE"
	sessC, errC, child := startAsync(t, sp, opts, next)
	if !strings.Contains(strings.Join(child.argv, " "), "--resume SESSION-ONE") {
		t.Fatalf("resume flag not passed through; argv=%v", child.argv)
	}
	child.emit(initLine)
	sess := awaitStart(t, sessC, errC)
	if sess.ResumeFrom != "SESSION-ONE" {
		t.Fatalf("resume_from not recorded: %q", sess.ResumeFrom)
	}
	child.emit(assistantLine)
	child.exit(nil)
	_, _ = sp.Wait(ctx, sess.ID)
}

// TestBuildArgsTTY covers the tty argv contract: the genuine TUI has no -p and
// no stream-json wrappers, keeps the autonomy default, and carries --resume.
func TestBuildArgsTTY(t *testing.T) {
	args, err := buildArgs(Options{Kind: KindTTY})
	if err != nil {
		t.Fatalf("buildArgs(tty): %v", err)
	}
	join := strings.Join(args, " ")
	for _, bad := range []string{"--output-format", "--input-format", "--verbose"} {
		if strings.Contains(join, bad) {
			t.Errorf("tty argv must not carry %q: %s", bad, join)
		}
	}
	for _, a := range args {
		if a == "-p" {
			t.Errorf("tty argv must not carry the headless -p flag: %s", join)
		}
	}
	if !strings.Contains(join, "--dangerously-skip-permissions") {
		t.Errorf("tty default argv must be full autonomy (same contract as other kinds): %s", join)
	}

	// the user's session name rides the claude-native --name flag
	nm, _ := buildArgs(Options{Kind: KindTTY, Title: "SUP-603 -> REVIEW"})
	if !strings.Contains(strings.Join(nm, " "), `--name SUP-603 -> REVIEW`) {
		t.Errorf("tty argv must carry the session name: %v", nm)
	}

	// resume + model ride along; an explicit permission mode stays a mode flag
	rs, _ := buildArgs(Options{Kind: KindTTY, ResumeSessionID: "abc", Model: "m", PermissionMode: "plan"})
	rsJoin := strings.Join(rs, " ")
	if !strings.HasPrefix(rsJoin, "--resume abc ") || !strings.Contains(rsJoin, "--model m ") {
		t.Errorf("tty resume/model ordering: %s", rsJoin)
	}
	if strings.Contains(rsJoin, "--dangerously-skip-permissions") {
		t.Errorf("explicit mode must not add skip-permissions: %s", rsJoin)
	}
}

// TestBuildArgsTTYAppendSystemPrompt pins the engine's new --append-system-
// prompt contract: when set, the flag comes BEFORE the positional prompt
// (claude rejects flags after the positional) and is omitted when empty.
// TestBuildArgsTTYLoopMember (below) covers the loop's full argv.
func TestBuildArgsTTYAppendSystemPrompt(t *testing.T) {
	args, err := buildArgs(Options{
		Kind:               KindTTY,
		Title:              "ORCHESTRATOR -> Auth Refactor",
		AppendSystemPrompt: "loop constitution summary",
		Prompt:             "first user turn",
	})
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	// The flag must come before the positional prompt.
	sysIdx := -1
	posIdx := -1
	for i, a := range args {
		if a == "--append-system-prompt" {
			sysIdx = i
		}
		if a == "first user turn" {
			posIdx = i
		}
	}
	if sysIdx < 0 || posIdx < 0 {
		t.Fatalf("missing --append-system-prompt or positional in %v", args)
	}
	if sysIdx >= posIdx {
		t.Errorf("--append-system-prompt must precede positional: %v", args)
	}
	if args[sysIdx+1] != "loop constitution summary" {
		t.Errorf("append value wrong: %v", args)
	}
	// omitting the flag when empty
	empty, _ := buildArgs(Options{Kind: KindTTY, Title: "X", Prompt: "p"})
	for _, a := range empty {
		if a == "--append-system-prompt" {
			t.Errorf("--append-system-prompt must be omitted when empty: %v", empty)
		}
	}
}

// TestBuildArgsTTYLoopMember pins the loop member's birth argv contract
// : the 6-digit-code name rides --name, full autonomy is the default,
// and the birth brief is the FINAL positional argument (claude processes it as
// the interactive session's first turn).
func TestBuildArgsTTYLoopMember(t *testing.T) {
	const brief = "You are the REVIEWER session of loop 938493."
	args, err := buildArgs(Options{
		Kind: KindTTY, Title: "REVIEWER 938493", Model: "claude-opus-5", Prompt: brief,
	})
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	join := strings.Join(args, " ")
	if !strings.Contains(join, "--name REVIEWER 938493") {
		t.Errorf("loop member must carry its SendMessage name: %v", args)
	}
	if !strings.Contains(join, "--dangerously-skip-permissions") {
		t.Errorf("NON-NEGOTIABLE: loop members spawn with full autonomy: %s", join)
	}
	if !strings.Contains(join, "--model claude-opus-5") {
		t.Errorf("per-role model missing: %s", join)
	}
	if args[len(args)-1] != brief {
		t.Errorf("birth brief must be the final positional arg: last=%q", args[len(args)-1])
	}
	for i, a := range args {
		if a == "-p" {
			t.Errorf("tty argv must not carry -p: %v (idx %d)", args, i)
		}
	}
}

// TestArgvFor_PicksOpenCodeEngine pins the dual-engine fix: when the
// Spawner has an engine registry with an opencode engine, opts.Engine =
// "opencode" must produce the opencode argv (`--port`, `--hostname`, ...)
// NOT the claude argv (`--name`, `--dangerously-skip-permissions`, ...).
// Before the fix, start()/StartTTY hard-coded LookupClaude regardless of
// opts.Engine and the user got Claude even when they picked OpenCode.
func TestArgvFor_PicksOpenCodeEngine(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", tmpHome+"/.opencode/bin:/usr/bin")

	// fake a path-resolvable opencode binary so engine.LookupBinary succeeds
	mustMkdirAll(t, tmpHome+"/.opencode/bin")
	mustWriteFile(t, tmpHome+"/.opencode/bin/opencode", "#!/bin/sh\nexit 0\n")
	mustChmod(t, tmpHome+"/.opencode/bin/opencode", 0o755)

	reg := engine.NewRegistry()
	reg.Register(claude.New(""))
	reg.Register(opencode.New(opencode.Config{Hostname: "127.0.0.1"}))

	s := New(nil, &ingest.Service{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetEngines(reg)

	args, err := s.argvFor(Options{Engine: "opencode", Cwd: "/tmp/demo", ResumeSessionID: "ses_abc"})
	if err != nil {
		t.Fatalf("argvFor opencode: %v", err)
	}
	join := strings.Join(args, " ")
	if !strings.Contains(join, "--port ") {
		t.Errorf("opencode argv must include --port (got %q)", join)
	}
	if !strings.Contains(join, "--hostname 127.0.0.1") {
		t.Errorf("opencode argv must include --hostname 127.0.0.1 (got %q)", join)
	}
	if !strings.Contains(join, "--session ses_abc") {
		t.Errorf("opencode argv must include --session for resume (got %q)", join)
	}
	// the opencode argv must NOT carry claude flags
	for _, bad := range []string{"--dangerously-skip-permissions", "--append-system-prompt"} {
		if strings.Contains(join, bad) {
			t.Errorf("opencode argv must not carry claude flag %q (got %q)", bad, join)
		}
	}

	// and binaryFor picks the opencode binary, not claude
	bin, err := s.binaryFor(Options{Engine: "opencode"})
	if err != nil {
		t.Fatalf("binaryFor opencode: %v", err)
	}
	if !strings.HasSuffix(bin, "opencode") {
		t.Errorf("binaryFor opencode = %q, want suffix /opencode", bin)
	}

	// sanity: claude path still works
	args, err = s.argvFor(Options{Engine: "claude", Model: "claude-opus-5", Title: "demo"})
	if err != nil {
		t.Fatalf("argvFor claude: %v", err)
	}
	join = strings.Join(args, " ")
	if !strings.Contains(join, "--name demo") {
		t.Errorf("claude argv must include --name (got %q)", join)
	}
	if !strings.Contains(join, "--dangerously-skip-permissions") {
		t.Errorf("claude argv must include --dangerously-skip-permissions (got %q)", join)
	}
}

// TestAppendCustomTitle pins the named-session contract: the user's title is
// appended to the corpus in claude's own custom-title format, which the parser
// maps to session_state/custom-title (the sessions list's top title source).
func TestAppendCustomTitle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"user\",\"sessionId\":\"s-1\"}\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	appendCustomTitle(path, dir, "s-1", "Fix the login bug")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	p, err := parse.Normalize([]byte(lines[1]), "corpus")
	if err != nil || len(p.Events) != 1 {
		t.Fatalf("normalize: %v events=%d", err, len(p.Events))
	}
	ev := p.Events[0]
	if ev.Type != eventmodel.EventSessionState || ev.Subtype != "custom-title" || ev.Content != "Fix the login bug" {
		t.Fatalf("want session_state/custom-title with the name, got %s/%s %q", ev.Type, ev.Subtype, ev.Content)
	}
}
