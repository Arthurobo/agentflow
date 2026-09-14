// stop_test.go — the universal terminate()/Stop() escalation path.
// Regression tests for the "cancel
// doesn't kill the loop" bug and for the four kill-path defects found
// afterwards:
//
// 1. ReadProcIdentity read ppid as the pgid and rss as the start-ticks, so
// kill(-pgid) targeted agentd's OWN process group and every identity
// check looked like PID reuse.
// 2. The graceful phase sent SIGINT to the bare pid (a TUI treats that as
// Ctrl-C and stays alive) instead of SIGTERM to the group.
// 3. Orphans (no in-memory Proc) selected on a pre-closed channel and so
// never escalated to SIGKILL.
// 4. Liveness was probed with os.FindProcess, which never fails on Unix.
package spawner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// testSpawner is a store-less Spawner: terminate()'s resolve/kill path
// doesn't need one (persistTerminated no-ops when st == nil).
func testSpawner() *Spawner {
	return &Spawner{
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		procs: map[string]*Proc{},
		now:   func() int64 { return time.Now().UnixMilli() },
	}
}

// startUnderPTY starts cmd with its own session/pgroup attached to a fresh
// PTY, returning the master fd and the pid. Skips the test when the sandbox
// forbids fork+exec or PTY allocation.
func startUnderPTY(t *testing.T, cmd *exec.Cmd) (*os.File, int) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("pty.Open not available: %v", err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })

	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("fork+exec not permitted in this environment: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return master, pid
}

// waitGone polls until pid is gone (or the deadline passes). Reaps in the
// background so a killed child doesn't linger as a zombie and read as alive.
func waitGone(t *testing.T, cmd *exec.Cmd, pid int, within time.Duration) bool {
	t.Helper()
	if cmd != nil {
		go func() { _, _ = cmd.Process.Wait() }()
	}
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !ProcessAlive(pid) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// TestReadProcIdentityMatchesKernel pins the /proc/<pid>/stat field offsets
// against the kernel's own answer. This is the test that would have caught
// the pgid==ppid bug: it asserts the pgid we would signal is the process's
// OWN group (pgrp == pid for a Setsid child), never its parent — because
// kill(-ppid) means "SIGKILL the daemon and everything it spawned".
func TestReadProcIdentityMatchesKernel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode (spawns a real process)")
	}
	cmd := exec.Command("sleep", "60")
	_, pid := startUnderPTY(t, cmd)

	pgid, ticks := ReadProcIdentity(pid)

	// A Setsid child leads its own group, so pgrp == pid.
	if pgid != pid {
		t.Fatalf("pgid = %d, want %d (the process's own group). If this is our ppid, kill(-pgid) would signal agentd's group", pgid, pid)
	}
	if pgid == os.Getpid() {
		t.Fatalf("pgid resolved to OUR pid (%d) — kill(-pgid) would kill the test runner", pgid)
	}
	// Cross-check against the kernel via the syscall.
	if kern, err := syscall.Getpgid(pid); err == nil && kern != pgid {
		t.Fatalf("pgid = %d, kernel Getpgid = %d", pgid, kern)
	}
	// starttime is monotonic-ish and large; rss (the old wrong field) is a
	// small page count that changes as the process runs.
	if ticks <= 0 {
		t.Fatalf("startTicks = %d, want > 0", ticks)
	}
	if want := procField(t, pid, 22); ticks != want {
		t.Fatalf("startTicks = %d, want field 22 (starttime) = %d", ticks, want)
	}
	// Stable across reads — an rss read would drift.
	if _, again := ReadProcIdentity(pid); again != ticks {
		t.Fatalf("startTicks not stable across reads: %d then %d", ticks, again)
	}
}

// procField reads 1-based field n from /proc/<pid>/stat, independently of
// ReadProcIdentity, so the assertion above can't share its bug.
func procField(t *testing.T, pid, n int) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Skipf("cannot read /proc/%d/stat: %v", pid, err)
	}
	s := string(data)
	fields := strings.Fields(s[strings.LastIndex(s, ")")+2:])
	idx := n - 3 // fields[0] is field 3 (state)
	if idx < 0 || idx >= len(fields) {
		t.Fatalf("field %d out of range", n)
	}
	v, err := strconv.ParseInt(fields[idx], 10, 64)
	if err != nil {
		t.Fatalf("field %d not an int: %v", n, err)
	}
	return v
}

// TestProcessAliveDetectsZombies: a killed-but-unreaped child is a zombie.
// signal 0 still succeeds on it, so a naive probe reports "alive" and
// death is never confirmed.
func TestProcessAliveDetectsZombies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("fork+exec not permitted: %v", err)
	}
	pid := cmd.Process.Pid
	if !ProcessAlive(pid) {
		t.Fatal("freshly started process reads as not alive")
	}
	// Kill but do NOT Wait — the child becomes a zombie we still parent.
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessAlive(pid) {
			_, _ = cmd.Process.Wait() // reap
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	_, _ = cmd.Process.Wait()
	t.Fatal("zombie still reported alive — death would never be confirmed")
}

// TestStopKillsTTYProcess is the original regression test: a `sleep` under a
// PTY ignores SIGHUP/EOF, so the pre-fix CloseStdin path never killed it.
func TestStopKillsTTYProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	cmd := exec.Command("sleep", "60")
	master, pid := startUnderPTY(t, cmd)

	sp := testSpawner()
	sp.procs["run-1"] = &Proc{
		sess:      &Session{ID: "run-1", Kind: KindTTY, PID: pid, State: StateRunning},
		child:     Child{Process: cmd.Process},
		ptyMaster: master,
		done:      make(chan struct{}),
	}

	if err := sp.Stop(context.Background(), "run-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !waitGone(t, cmd, pid, stopGrace+3*time.Second) {
		t.Fatalf("pid %d survived Stop()", pid)
	}
}

// TestStopKillsWholeProcessGroup: claude's children must die with it.
// A shell that forks a grandchild into the same group stands in for
// claude + its bash/MCP subprocesses. Killing only the direct pid leaves
// the grandchild running — the leak this test exists to catch.
func TestStopKillsWholeProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}
	// The child prints its grandchild's pid, then waits.
	cmd := exec.Command(sh, "-c", "sleep 60 & echo $!; wait")
	master, pid := startUnderPTY(t, cmd)

	// Read the grandchild pid off the PTY.
	var kidPID int
	buf := make([]byte, 256)
	_ = master.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, rerr := master.Read(buf); rerr == nil {
		if v, perr := strconv.Atoi(strings.TrimSpace(strings.Split(string(buf[:n]), "\n")[0])); perr == nil {
			kidPID = v
		}
	}
	_ = master.SetReadDeadline(time.Time{})
	if kidPID == 0 {
		t.Skip("could not read grandchild pid from the pty")
	}
	t.Cleanup(func() { _ = syscall.Kill(kidPID, syscall.SIGKILL) })
	if !ProcessAlive(kidPID) {
		t.Fatalf("grandchild %d not running at test start", kidPID)
	}

	sp := testSpawner()
	sp.procs["grp"] = &Proc{
		sess:      &Session{ID: "grp", Kind: KindTTY, PID: pid, State: StateRunning},
		child:     Child{Process: cmd.Process},
		ptyMaster: master,
		done:      make(chan struct{}),
	}
	if err := sp.Stop(context.Background(), "grp"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !waitGone(t, cmd, pid, stopGrace+3*time.Second) {
		t.Fatalf("group leader %d survived Stop()", pid)
	}
	if !waitGone(t, nil, kidPID, 3*time.Second) {
		t.Fatalf("grandchild %d survived Stop() — the kill did not reach the process group", kidPID)
	}
}

// TestStopKillsOrphanWithNoLiveProc is the reconciler's path: the
// agentd life that spawned the process is gone, so there is NO entry in
// s.procs — only a persisted row. terminate() must still escalate to
// SIGKILL. The bug this pins: the orphan branch waited on a pre-closed
// channel, so it recorded "stopped" and never signaled again, leaving the
// process alive forever.
func TestStopKillsOrphanWithNoLiveProc(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	// A process that ignores SIGTERM, so only the SIGKILL escalation can
	// end it — exactly like a TUI that ignores polite signals.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}
	cmd := exec.Command(sh, "-c", "trap '' TERM HUP; sleep 60")
	_, pid := startUnderPTY(t, cmd)
	pgid, ticks := ReadProcIdentity(pid)
	if pgid == 0 {
		t.Skip("could not read pgid")
	}

	sp := testSpawner()
	// procs is intentionally EMPTY: no in-memory state, mirroring a
	// post-restart orphan. We drive terminateAsync the way terminate()
	// does for a row-resolved session.
	sess := &Session{ID: "orphan", Kind: KindTTY, PID: pid, State: StateRunning}
	sp.terminateAsync(nil, sess, pgid, true, "orphaned")

	if !waitGone(t, cmd, pid, stopGrace+5*time.Second) {
		t.Fatalf("orphan pid %d survived termination — SIGKILL never escalated", pid)
	}
	_ = ticks
}

// TestStopIsIdempotent: calling Stop twice on the same run is safe.
func TestStopIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	cmd := exec.Command("sleep", "60")
	master, pid := startUnderPTY(t, cmd)

	sp := testSpawner()
	sp.procs["run-2"] = &Proc{
		sess:      &Session{ID: "run-2", Kind: KindTTY, PID: pid, State: StateRunning},
		child:     Child{Process: cmd.Process},
		ptyMaster: master,
		done:      make(chan struct{}),
	}
	if err := sp.Stop(context.Background(), "run-2"); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	// The second call must not panic or block. It may return a benign
	// error if the pump has already removed the run from s.procs.
	if err := sp.Stop(context.Background(), "run-2"); err != nil {
		t.Logf("second Stop returned (acceptable): %v", err)
	}
	if !waitGone(t, cmd, pid, stopGrace+3*time.Second) {
		t.Fatalf("pid %d survived Stop()", pid)
	}
}
