package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/arthurobo/agentflow/internal/store"
)

// fakeReaper stands in for the spawner: the boot scan's job is deciding
// WHICH rows to hand over, and that decision is what these tests pin.
type fakeReaper struct {
	mu   sync.Mutex
	gen  string
	seen []string
	err  error
}

func (f *fakeReaper) Generation() string { return f.gen }

func (f *fakeReaper) Reap(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, runID)
	return f.err
}

func (f *fakeReaper) reaped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func reaperStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "boot.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seedRun(t *testing.T, st *store.Store, id, state, gen string) {
	t.Helper()
	if err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: id, Kind: "tty", State: state, Generation: gen, StartedAt: 1, PID: 4 << 20,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// The restart case: rows left running by a previous agentd life get reaped,
// and the runs this boot owns are left strictly alone.
func TestBootScanReapsForeignGenerationsOnly(t *testing.T) {
	st := reaperStore(t)
	seedRun(t, st, "run-previous", "running", "gen-before")
	seedRun(t, st, "run-unstamped", "running", "") // predates the stamp
	seedRun(t, st, "run-current", "running", "gen-now")
	seedRun(t, st, "run-finished", "finished", "gen-before")

	sp := &fakeReaper{gen: "gen-now"}
	n := reapOrphans(context.Background(), st, sp, quietLog())

	if n != 2 {
		t.Fatalf("reaped %d, want the two orphans", n)
	}
	got := map[string]bool{}
	for _, id := range sp.reaped() {
		got[id] = true
	}
	if !got["run-previous"] || !got["run-unstamped"] {
		t.Fatalf("both orphans must be reaped, got %v", sp.reaped())
	}
	if got["run-current"] {
		t.Fatal("this boot's own run must never be reaped")
	}
	if got["run-finished"] {
		t.Fatal("an already-terminal row must not be re-terminated")
	}
}

// Nothing to do is the common case and must be silent and cheap.
func TestBootScanDoesNothingWhenEveryRunIsCurrent(t *testing.T) {
	st := reaperStore(t)
	seedRun(t, st, "run-a", "running", "gen-now")
	seedRun(t, st, "run-b", "starting", "gen-now")

	sp := &fakeReaper{gen: "gen-now"}
	if n := reapOrphans(context.Background(), st, sp, quietLog()); n != 0 {
		t.Fatalf("reaped %d, want 0", n)
	}
	if len(sp.reaped()) != 0 {
		t.Fatalf("nothing should have been touched, got %v", sp.reaped())
	}
}

// Without a generation every row looks foreign, so a scan that ran anyway
// would kill the runs this boot just started. It must refuse instead.
func TestBootScanRefusesWithoutAGeneration(t *testing.T) {
	st := reaperStore(t)
	seedRun(t, st, "run-live", "running", "gen-now")

	sp := &fakeReaper{gen: ""}
	if n := reapOrphans(context.Background(), st, sp, quietLog()); n != 0 {
		t.Fatalf("reaped %d, want 0", n)
	}
	if len(sp.reaped()) != 0 {
		t.Fatalf("an ungenerationed scan must reap NOTHING, got %v", sp.reaped())
	}
}

// One row that will not die must not stop the others being reaped, and must
// not stop the daemon booting.
func TestBootScanKeepsGoingWhenOneReapFails(t *testing.T) {
	st := reaperStore(t)
	seedRun(t, st, "run-one", "running", "gen-before")
	seedRun(t, st, "run-two", "running", "gen-before")

	sp := &fakeReaper{gen: "gen-now", err: errors.New("nope")}
	if n := reapOrphans(context.Background(), st, sp, quietLog()); n != 0 {
		t.Fatalf("a failed reap must not be counted, got %d", n)
	}
	if len(sp.reaped()) != 2 {
		t.Fatalf("both rows must still have been attempted, got %v", sp.reaped())
	}
}

// The scan runs before anything serves; a nil dependency is a programming
// error, not a reason to panic the daemon on boot.
func TestBootScanIsSafeWithNothingWiredUp(t *testing.T) {
	if n := reapOrphans(context.Background(), nil, &fakeReaper{gen: "g"}, quietLog()); n != 0 {
		t.Fatalf("nil store: %d", n)
	}
	if n := reapOrphans(context.Background(), reaperStore(t), nil, quietLog()); n != 0 {
		t.Fatalf("nil spawner: %d", n)
	}
}
