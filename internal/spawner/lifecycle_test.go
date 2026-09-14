// lifecycle_test.go — process lifecycle regressions: output lost at exit,
// races between the pump and its readers, early deaths that read as alive,
// shutdown that left children behind, and session ids lost on the way out.
package spawner

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeScript puts an executable shell script in a temp dir.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-engine")
	mustWriteFile(t, path, "#!/bin/sh\n"+body)
	mustChmod(t, path, 0o755)
	return path
}

// The child's last lines are still in the pipe when it exits. Wait used to
// run the moment the child died and close that pipe, so a reader a moment
// behind lost the result event.
func TestHeadlessFinalResultNotLost(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real process")
	}
	// ~60 KB of output (fits the pipe buffer without blocking), then the
	// result line, then exit at once.
	script := writeScript(t, `i=0
while [ $i -lt 600 ]; do
  printf '%0100d\n' 0
  i=$((i+1))
done
echo '{"type":"result","final":true}'
exit 0
`)
	r := &ExecRunner{Path: script}
	child, err := r.Start(nil, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Let the child finish and be reapable before anyone reads a byte.
	time.Sleep(500 * time.Millisecond)
	out, err := io.ReadAll(child.Stdout)
	if err != nil {
		t.Fatalf("reading the output of a child that already exited failed: %v", err)
	}
	if !strings.Contains(string(out), `"final":true`) {
		t.Fatalf("the final line was lost: got %d bytes", len(out))
	}
	select {
	case werr := <-child.Waitch:
		if werr != nil {
			t.Fatalf("wait: %v", werr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait never ran after stdout reached EOF")
	}
}

// Waiting for stdout's EOF must not hang forever when something the child
// started keeps stdout open after the child itself is gone.
func TestHeadlessWaitDoesNotHangOnInheritedStdout(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real process")
	}
	if runtime.GOOS != "linux" {
		t.Skip("an unreaped exit is only observable on linux")
	}
	script := writeScript(t, "sleep 30 &\necho started\nexit 0\n")
	r := &ExecRunner{Path: script}
	child, err := r.Start(nil, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL) })
	go func() { _, _ = io.Copy(io.Discard, child.Stdout) }()
	select {
	case <-child.Waitch:
	case <-time.After(outputDrainGrace + 5*time.Second):
		t.Fatal("Wait hung on a pipe a grandchild kept open")
	}
}

// The pump's pending batch is touched only by the pump goroutine. The
// periodic flush used to run on a ticker goroutine of its own, and a stream
// that keeps a run busy across several ticks raced it. Run with -race.
func TestHeadlessPumpFlushIsRaceFree(t *testing.T) {
	sp, _ := newTestSpawner(t)
	runner, next := newScriptRunner()
	sp.SetRunner(runner)
	ctx := context.Background()

	opts := testOpts()
	opts.Kind = KindChat
	opts.Cwd = "/tmp/ws"
	sessC, errC, child := startAsync(t, sp, opts, next)
	child.emit(initLine)
	sess := awaitStart(t, sessC, errC)

	// Readers of the live session while the pump updates it: List and
	// Status used to read the live struct without its lock.
	stopReaders := make(chan struct{})
	readersDone := make(chan struct{})
	go func() {
		defer close(readersDone)
		for {
			select {
			case <-stopReaders:
				return
			default:
			}
			_, _ = sp.List(ctx, "")
			_, _ = sp.Status(ctx, sess.ID)
		}
	}()

	const lines = 40
	for i := 0; i < lines; i++ {
		child.emit(fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"line %d"}]},"session_id":"SESSION-ONE","uuid":"u-%d","timestamp":"2026-08-19T10:00:00.000Z"}`, i, i))
		time.Sleep(20 * time.Millisecond) // spans several flush ticks
	}
	child.emit(resultLine)
	child.exit(nil)
	fin, err := sp.Wait(ctx, sess.ID)
	close(stopReaders)
	<-readersDone
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if fin.EventCount < lines {
		t.Fatalf("every event must be counted exactly through the flushes, got %d", fin.EventCount)
	}
	sp.mu.Lock()
	_, stillThere := sp.procs[sess.ID]
	sp.mu.Unlock()
	if stillThere {
		t.Fatal("a finished headless run must be removed from the live set")
	}
}

// A stop requested while the run is still producing must not overwrite the
// row the pump writes at the end with the snapshot taken at the stop.
func TestStopKeepsThePumpsFinalRow(t *testing.T) {
	sp, st := newTestSpawner(t)
	runner, next := newScriptRunner()
	sp.SetRunner(runner)
	ctx := context.Background()

	opts := testOpts()
	opts.Kind = KindChat
	opts.Cwd = "/tmp/ws"
	sessC, errC, child := startAsync(t, sp, opts, next)
	child.emit(initLine)
	sess := awaitStart(t, sessC, errC)

	if err := sp.Stop(ctx, sess.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// The child finishes its turn on the way out.
	child.emit(assistantLine)
	child.emit(resultLine)
	child.exit(nil)
	if _, err := sp.Wait(ctx, sess.ID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		row, err := st.GetManagedSession(ctx, sess.ID)
		if err != nil {
			t.Fatalf("row: %v", err)
		}
		if row.StopReason == "user_stop" {
			if row.TotalCostUSD != 0.1 || row.EventCount < 2 || row.State != string(StateStopped) {
				t.Fatalf("the final row lost what the pump recorded: %+v", row)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stop reason was never recorded: %+v", row)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A TUI that dies at birth must read as dead at once, not after the 90
// second session discovery deadline.
func TestEarlyTTYDeathDetectedFast(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real process")
	}
	t.Setenv("HOME", t.TempDir()) // StartTTY pre-trusts the cwd in ~/.claude.json
	sp, _ := newTestSpawner(t)
	sp.SetCorpusRoot(t.TempDir())
	sp.SetClaudePath(writeScript(t, "exit 3\n"))
	ctx := context.Background()

	if _, err := sp.StartTTY(ctx, "run-early", Options{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		cur, err := sp.Status(ctx, "run-early")
		if err == nil && cur != nil && cur.State.Terminal() {
			if cur.State != StateCrashed || cur.ExitCode != 3 {
				t.Fatalf("an exit 3 at birth is a crash with its code: %+v", cur)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a TUI that exited at once still reads as alive: %+v", cur)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startIgnoringTERM starts a child that ignores SIGTERM and registers it as a
// live run, returning the Proc. It returns only once the trap is installed:
// a TERM that lands before that kills the shell, which is not what these
// tests are about.
func startIgnoringTERM(t *testing.T, sp *Spawner, runID string) *Proc {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}
	cmd := exec.Command(sh, "-c", "trap '' TERM HUP; echo ready; sleep 60")
	master, pid := startUnderPTY(t, cmd)
	_ = master.SetReadDeadline(time.Now().Add(5 * time.Second))
	var seen strings.Builder
	buf := make([]byte, 64)
	for !strings.Contains(seen.String(), "ready") {
		n, rerr := master.Read(buf)
		if rerr != nil {
			t.Fatalf("the child never said it was ready: %v (%q)", rerr, seen.String())
		}
		seen.Write(buf[:n])
	}
	waitch := make(chan error, 1)
	go func() { waitch <- cmd.Wait() }()
	// Headless kind: stopping it signals and nothing else, so only the
	// signals decide when it dies.
	p := newProc(&Session{ID: runID, Kind: KindOneShot, PID: pid, State: StateRunning},
		Child{Process: cmd.Process, Waitch: waitch}, func() {})
	sp.mu.Lock()
	sp.procs[runID] = p
	sp.mu.Unlock()
	return p
}

// Shutdown waits for children to be gone instead of returning as soon as it
// has asked them to go.
func TestStopAllWaitsForChildrenToExit(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	sp := testSpawner()
	a := startIgnoringTERM(t, sp, "a")
	b := startIgnoringTERM(t, sp, "b")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sp.StopAll(ctx); err != nil {
		t.Fatalf("StopAll within its budget: %v", err)
	}
	if !a.hasExited() || !b.hasExited() {
		t.Fatal("StopAll returned while children were still running")
	}
}

// A deadline shorter than the stop grace ends with the stragglers SIGKILLed
// at the deadline, not left for a timer that dies with the daemon.
func TestStopAllKillsStragglersAtTheDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	sp := testSpawner()
	p := startIgnoringTERM(t, sp, "stubborn")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := sp.StopAll(ctx)
	if err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("StopAll must report the runs it had to kill, got %v", err)
	}
	select {
	case <-p.exited:
	case <-time.After(stopGrace - 400*time.Millisecond):
		// The ordinary escalation fires at stopGrace; dying only then means
		// the deadline killed nothing.
		t.Fatal("the straggler was not killed at the deadline")
	}
}

// An engine session id learned outside the spawner is recorded on the live
// run and survives the run's exit.
func TestSetSessionIDSurvivesExit(t *testing.T) {
	sp, st := newTestSpawner(t)
	runner, next := newScriptRunner()
	sp.SetRunner(runner)
	ctx := context.Background()

	opts := testOpts()
	opts.Kind = KindChat
	opts.Cwd = "/tmp/ws"
	opts.InitTimeout = 50 * time.Millisecond
	sessC, errC, child := startAsync(t, sp, opts, next)
	sess := awaitStart(t, sessC, errC)
	if sess.SessionID != "" {
		t.Fatalf("no init was emitted, so no session id yet: %q", sess.SessionID)
	}
	if err := sp.SetSessionID(sess.ID, "ses_opencode"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if cur, _ := sp.Status(ctx, sess.ID); cur.SessionID != "ses_opencode" || cur.State != StateRunning {
		t.Fatalf("the live run must carry the id and be running: %+v", cur)
	}
	child.exit(nil)
	if _, err := sp.Wait(ctx, sess.ID); err != nil {
		t.Fatalf("wait: %v", err)
	}
	row, err := st.GetManagedSession(ctx, sess.ID)
	if err != nil || row == nil || row.SessionID != "ses_opencode" {
		t.Fatalf("the id must survive exit: %+v %v", row, err)
	}
	if got, _ := sp.ListBySessionID(ctx, "ses_opencode"); got == nil || got.ID != sess.ID {
		t.Fatalf("the run must be found by its session id: %+v", got)
	}
}

func TestChildSpawnEnvStripsAgentflowAndTailscaleEnv(t *testing.T) {
	base := []string{
		"TS_AUTHKEY=tskey-auth-secret",
		"TS_HOSTNAME=agentflow-abc",
		"TS_ANYTHING_NEW=1",
		"AF_REMOTE=tailscale",
		"AF_CLAUDE=/usr/local/bin/claude",
		"AF_RETENTION_DAYS=30",
		"HOME=/home/u",
		"PATH=/usr/bin",
		"TSX_NOT_TAILSCALE=keep",
	}
	got := ChildSpawnEnv(base, KindTTY, []string{"EXTRA=1"})
	joined := "\n" + strings.Join(got, "\n") + "\n"
	for _, gone := range []string{"TS_AUTHKEY=", "TS_HOSTNAME=", "TS_ANYTHING_NEW=", "AF_REMOTE=", "AF_CLAUDE=", "AF_RETENTION_DAYS="} {
		if strings.Contains(joined, "\n"+gone) {
			t.Errorf("%s reached the child", gone)
		}
	}
	for _, kept := range []string{"HOME=/home/u", "PATH=/usr/bin", "TSX_NOT_TAILSCALE=keep", "EXTRA=1"} {
		if !strings.Contains(joined, "\n"+kept+"\n") {
			t.Errorf("%s must survive", kept)
		}
	}
}

// The tty registrar is installed after the spawner exists, while runs may
// already be starting. Reading it without the lock raced that install. Run
// with -race.
func TestTTYRegistrarCanBeInstalledWhileRunsStart(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	t.Setenv("HOME", t.TempDir())
	sp, _ := newTestSpawner(t)
	sp.SetCorpusRoot(t.TempDir())
	sp.SetClaudePath(writeScript(t, "exit 0\n"))
	ctx := context.Background()

	stop := make(chan struct{})
	installed := make(chan struct{})
	go func() {
		defer close(installed)
		for {
			select {
			case <-stop:
				return
			default:
			}
			sp.SetTTYRegistrar(func(string, io.ReadWriteCloser, func(cols, rows uint16) error, uint16) {})
			sp.SetPostStart(func(context.Context, string, *Session, Options) {})
		}
	}()
	for i := 0; i < 5; i++ {
		if _, err := sp.StartTTY(ctx, fmt.Sprintf("run-reg-%d", i), Options{Cwd: t.TempDir()}); err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	close(stop)
	<-installed
}
