package main

import (
	"context"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/store"
)

// The boot reaper marked a restarted loop's member RUNS stopped and left the
// parent loops row reading "active" forever, because the loops table carried
// no generation. Six such loops were alive on the live database when this was
// written, every member of every one of them already dead.

// genOnly is the slice of the daemon the loop sweep needs: nothing but the
// current generation. No spawner, because ending a loop kills no process.
type genOnly struct{ gen string }

func (g genOnly) Generation() string { return g.gen }

func seedLoop(t *testing.T, st *store.Store, id, title, gen string) {
	t.Helper()
	if _, err := st.CreateCrew(context.Background(),
		&store.Loop{ID: id, Title: title, Generation: gen},
		store.RoleSpecs(store.RoleOrchestrator)); err != nil {
		t.Fatalf("seed loop %s: %v", id, err)
	}
}

func loopStatus(t *testing.T, st *store.Store, id string) *store.Loop {
	t.Helper()
	l, err := st.GetLoop(context.Background(), id)
	if err != nil || l == nil {
		t.Fatalf("get loop %s: %+v %v", id, l, err)
	}
	return l
}

func TestReapOrphanLoopsEndsAPreviousGenerationsLoops(t *testing.T) {
	st := reaperStore(t)
	ctx := context.Background()

	seedLoop(t, st, "loop_old", "From a dead daemon", "gen-old")
	// The six on disk today: created before the column existed, so unstamped.
	seedLoop(t, st, "loop_unstamped", "Predates the stamp", "")
	seedLoop(t, st, "loop_mine", "This life's", "gen-now")

	if n := reapOrphanLoops(ctx, st, genOnly{"gen-now"}, quietLog()); n != 2 {
		t.Fatalf("ended %d loops, want the two foreign ones", n)
	}

	for _, id := range []string{"loop_old", "loop_unstamped"} {
		l := loopStatus(t, st, id)
		if l.Status != store.LoopInterrupted {
			t.Errorf("%s status = %q, want interrupted", id, l.Status)
		}
		if !strings.Contains(l.EndReason, "gen-now") {
			t.Errorf("%s reason = %q, want the generation transition named", id, l.EndReason)
		}
		if !l.Ended() {
			t.Errorf("%s must read as ended, or the mailbox keeps taking work for it", id)
		}
	}
	// The unstamped one has to say so rather than printing an empty arrow.
	if r := loopStatus(t, st, "loop_unstamped").EndReason; !strings.Contains(r, "(unstamped)") {
		t.Errorf("reason = %q, want the blank generation named", r)
	}

	// This life's loop is untouched, which is the half that matters: a reaper
	// that ends everything is not a reaper.
	if l := loopStatus(t, st, "loop_mine"); l.Status != store.LoopActive {
		t.Fatalf("the current generation's loop was ended: %q", l.Status)
	}
}

// Ending is not dismissing: an interrupted loop stays visible with its crew
// parked, because "this was interrupted" is evidence worth keeping.
func TestReapOrphanLoopsParksTheCrewWithoutDismissingIt(t *testing.T) {
	st := reaperStore(t)
	ctx := context.Background()

	seedLoop(t, st, "loop_old", "From a dead daemon", "gen-old")
	reapOrphanLoops(ctx, st, genOnly{"gen-now"}, quietLog())

	members, err := st.ListLoopMembers(ctx, "loop_old")
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(members) == 0 {
		t.Fatal("the roster must survive; a vanished loop loses the evidence")
	}
	for _, m := range members {
		if m.Status == store.LoopMemberDismissed {
			t.Errorf("%s was dismissed; the reaper ends the loop, it does not retire the crew", m.Role)
		}
	}
}

func TestReapOrphanLoopsIsANoOpWhenEverythingIsCurrent(t *testing.T) {
	st := reaperStore(t)
	seedLoop(t, st, "loop_a", "a", "gen-now")
	seedLoop(t, st, "loop_b", "b", "gen-now")

	if n := reapOrphanLoops(context.Background(), st, genOnly{"gen-now"}, quietLog()); n != 0 {
		t.Fatalf("ended %d loops on a clean boot, want 0", n)
	}
	if l := loopStatus(t, st, "loop_a"); l.Status != store.LoopActive {
		t.Fatalf("status = %q", l.Status)
	}
}

// Without a generation every loop looks foreign, so the scan must decline
// rather than end the loops this boot is about to start.
func TestReapOrphanLoopsRefusesToRunWithoutAGeneration(t *testing.T) {
	st := reaperStore(t)
	seedLoop(t, st, "loop_old", "From a dead daemon", "gen-old")

	if n := reapOrphanLoops(context.Background(), st, genOnly{""}, quietLog()); n != 0 {
		t.Fatalf("ended %d loops with no generation, want 0", n)
	}
	if l := loopStatus(t, st, "loop_old"); l.Status != store.LoopActive {
		t.Fatalf("an empty generation must reap nothing, got %q", l.Status)
	}
}

func TestReapOrphanLoopsToleratesMissingCollaborators(t *testing.T) {
	st := reaperStore(t)
	if n := reapOrphanLoops(context.Background(), nil, genOnly{"g"}, quietLog()); n != 0 {
		t.Errorf("no store: %d", n)
	}
	if n := reapOrphanLoops(context.Background(), st, nil, quietLog()); n != 0 {
		t.Errorf("no generation source: %d", n)
	}
}

// The two halves of the boot reconciler are independent: the runs are reaped
// through the spawner, the loops are ended in the store, and neither needs
// the other to have worked.
func TestBootReconcilerEndsTheLoopAndReapsItsRuns(t *testing.T) {
	st := reaperStore(t)
	ctx := context.Background()

	seedRun(t, st, "run-old", "running", "gen-old")
	seedLoop(t, st, "loop_old", "From a dead daemon", "gen-old")

	f := &fakeReaper{gen: "gen-now"}
	reapOrphans(ctx, st, f, quietLog())
	reapOrphanLoops(ctx, st, f, quietLog())

	if got := f.reaped(); len(got) != 1 || got[0] != "run-old" {
		t.Fatalf("reaped runs = %v, want [run-old]", got)
	}
	if l := loopStatus(t, st, "loop_old"); l.Status != store.LoopInterrupted {
		t.Fatalf("loop status = %q, want interrupted", l.Status)
	}
}
