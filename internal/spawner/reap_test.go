package spawner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/store"
)

// reapFixture is a spawner over a real store with no live processes — which
// is precisely the state a fresh agentd boot is in when it finds rows left
// running by a previous life.
func reapFixture(t *testing.T) (*Spawner, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reap.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"})
	return New(st, svc, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

// deadPID returns a pid that is not running. Above pid_max nothing can ever
// have it, so /proc/<pid>/stat is unreadable and the identity check fails —
// the state every one of the stale rows is in.
const deadPID = 4 << 20

func waitForReapedRow(t *testing.T, st *store.Store, runID string, want string) *store.ManagedSession {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, err := st.GetManagedSession(context.Background(), runID)
		if err == nil && m != nil && m.State == want {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	m, _ := st.GetManagedSession(context.Background(), runID)
	t.Fatalf("run %s never reached %q: %+v", runID, want, m)
	return nil
}

// The 16 stale rows: state=running, a PID from before the restart, nothing
// alive behind them. Reap must write them terminal and say why.
func TestReapMarksAnOrphanStoppedAndRecordsWhy(t *testing.T) {
	sp, st := reapFixture(t)
	ctx := context.Background()

	if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
		ID: "run-orphan", Kind: "tty", State: "running", PID: deadPID,
		Generation: "gen-previous-life", StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := sp.Reap(ctx, "run-orphan"); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	got := waitForReapedRow(t, st, "run-orphan", "stopped")
	// "orphaned" is the reason names; "_unverified" is the identity
	// check saying it never signaled anything, which is the honest record
	// for a process that was already gone.
	if got.StopReason != "orphaned_unverified" {
		t.Fatalf("stop reason = %q, want orphaned_unverified", got.StopReason)
	}
	if got.EndedAt == 0 {
		t.Fatal("a reaped row must carry an end time")
	}
	// The generation is what identified it as an orphan; losing it on the
	// way out would make the audit trail unreadable.
	if got.Generation != "gen-previous-life" {
		t.Fatalf("generation lost on reap: %+v", got)
	}
}

// Stop and Reap are the same primitive and differ only in the record they
// leave. A run stopped by a person must not read as an orphan, or the
// restart audit trail is worthless.
func TestReapAndStopAreDistinguishableInTheRow(t *testing.T) {
	sp, st := reapFixture(t)
	ctx := context.Background()

	for _, id := range []string{"run-reaped", "run-stopped"} {
		if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
			ID: id, Kind: "tty", State: "running", PID: deadPID,
			Generation: "gen-previous-life", StartedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := sp.Reap(ctx, "run-reaped"); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if err := sp.Stop(ctx, "run-stopped"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := waitForReapedRow(t, st, "run-reaped", "stopped"); got.StopReason != "orphaned_unverified" {
		t.Fatalf("reaped row reads %q", got.StopReason)
	}
	if got := waitForReapedRow(t, st, "run-stopped", "stopped"); got.StopReason != "user_stop_unverified" {
		t.Fatalf("stopped row reads %q", got.StopReason)
	}
}

// Already-terminal rows are left alone: re-terminating one would overwrite
// the reason it actually ended for.
func TestReapLeavesTerminalRowsAlone(t *testing.T) {
	sp, st := reapFixture(t)
	ctx := context.Background()

	if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
		ID: "run-done", Kind: "tty", State: "finished", PID: deadPID,
		Generation: "gen-previous-life", StopReason: "user_stop", EndedAt: 4242,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := sp.Reap(ctx, "run-done"); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	got, err := st.GetManagedSession(ctx, "run-done")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.State != "finished" || got.StopReason != "user_stop" || got.EndedAt != 4242 {
		t.Fatalf("a terminal row must be untouched, got %+v", got)
	}
}

// A run id nobody has ever heard of is a no-op, not an error: the scan runs
// at boot and must not be able to stop the daemon coming up.
func TestReapOfAnUnknownRunIsANoOp(t *testing.T) {
	sp, _ := reapFixture(t)
	if err := sp.Reap(context.Background(), "run-never-existed"); err != nil {
		t.Fatalf("Reap of a missing run: %v", err)
	}
}

// A row whose PID cannot be verified is marked gone and NEVER
// signaled. It used to be signaled anyway — SIGTERM, then SIGKILL 1.5s
// later — and because identityCheck hands back the LIVE process's pgid when
// a PID has been recycled, both landed on kill(-pgid) of a process group we
// do not own. Reaping a restart's worth of stale rows is exactly when PIDs
// are most likely to have been reused.
//
// The victim here is this test process's own group. If the guard regresses,
// the SIGTERM below kills the test binary and the failure is impossible to
// miss.
func TestReapNeverSignalsAPidItCannotVerify(t *testing.T) {
	sp, st := reapFixture(t)
	ctx := context.Background()

	got := make(chan os.Signal, 1)
	signal.Notify(got, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(got) })

	// A live PID with a stored identity that does NOT match: the recycled-
	// PID case. os.Getpid() is alive, so /proc/<pid>/stat reads fine and
	// yields a real pgid, but the stored start-ticks are a lie.
	if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
		ID: "run-recycled", Kind: "tty", State: "running", PID: os.Getpid(),
		Pgid: syscall.Getpgrp(), ProcStartTicks: 1, // never a real start time
		Generation: "gen-previous-life", StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := sp.Reap(ctx, "run-recycled"); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	row := waitForReapedRow(t, st, "run-recycled", "stopped")
	if row.StopReason != "orphaned_unverified" {
		t.Fatalf("stop reason = %q, want orphaned_unverified", row.StopReason)
	}
	// Well past the 1.5s grace the escalation would have waited.
	select {
	case sig := <-got:
		t.Fatalf("reap signaled a process it could not verify: %v", sig)
	case <-time.After(2500 * time.Millisecond):
	}
}

// The reaper is only as good as the stamp it reads. A spawn stamps the boot
// generation, and the pump's status persists must not blank it — that was
// the whole reason every row read generation=” and nothing could ever be
// identified as an orphan.
func TestASpawnKeepsItsGenerationThroughStatusPersists(t *testing.T) {
	sp, st := newTestSpawner(t)
	sp.SetGeneration("gen-this-boot")
	runner, next := newScriptRunner()
	sp.SetRunner(runner)

	sessC, errC, child := startAsync(t, sp, Options{Cwd: t.TempDir()}, next)
	child.emit(initLine)
	sess := awaitStart(t, sessC, errC)

	row, err := st.GetManagedSession(context.Background(), sess.ID)
	if err != nil || row == nil {
		t.Fatalf("get: %v %v", row, err)
	}
	if row.Generation != "gen-this-boot" {
		t.Fatalf("the spawn stamp did not land: %+v", row)
	}

	// drive more output so the pump persists status again — the write that
	// used to blank the column
	child.emit(rateLine)
	child.exit(nil)
	_, _ = sp.Wait(context.Background(), sess.ID)

	row, err = st.GetManagedSession(context.Background(), sess.ID)
	if err != nil || row == nil {
		t.Fatalf("get after pump: %v %v", row, err)
	}
	if row.Generation != "gen-this-boot" {
		t.Fatalf("a status persist blanked the generation: %+v", row)
	}

	// And the row this boot owns is not an orphan to itself.
	orphans, err := st.ListActiveManagedSessionsNotInGeneration(
		context.Background(), sp.Generation())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, m := range orphans {
		if m.ID == sess.ID {
			t.Fatalf("this boot's own run was listed as an orphan: %+v", m)
		}
	}
}
