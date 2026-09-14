package store

import (
	"context"
	"testing"
)

// Every agentd boot mints a generation, stamps it on every managed row it
// creates, and the boot reconciler reaps
// active rows whose generation is not the current one.
//
// The reap cannot work if the stamp does not survive, and it did not:
// UpsertManagedSession assigned `generation = excluded.generation`
// unconditionally, so the first status persist after spawn blanked the
// column of a live run. Every writer but the spawn path rebuilds the row
// from a spawner.Session, which has no field to copy the generation, the
// pgid or the start ticks from. A blank generation is indistinguishable
// from "never stamped", so the reconciler could never tell a run from a
// previous agentd life from a current one.
//
// The rule now: a writer that does not carry the kill identity leaves it
// alone. An explicit value still wins.
func TestUpsertPreservesTheKillIdentityAWriterDoesNotCarry(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	// spawn: persistInitial stamps the boot generation and the kill identity
	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-1", Kind: "tty", State: "running",
		Generation: "gen-boot-a", Pgid: 4242, ProcStartTicks: 99,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := s.GetManagedSession(ctx, "run-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Generation != "gen-boot-a" || got.Pgid != 4242 || got.ProcStartTicks != 99 {
		t.Fatalf("the spawn stamp did not land: %+v", got)
	}

	// a later full-row upsert from a struct that carries no kill identity —
	// which is every caller built from a spawner.Session, because that type
	// has no Generation/Pgid/ProcStartTicks field to copy from
	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-1", Kind: "tty", State: "running", EventCount: 7,
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, err = s.GetManagedSession(ctx, "run-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Generation != "gen-boot-a" || got.Pgid != 4242 || got.ProcStartTicks != 99 {
		t.Fatalf("a status persist must not blank the kill identity, got %+v", got)
	}
	if got.EventCount != 7 {
		t.Fatalf("the fields the writer DOES carry must still apply, got %+v", got)
	}

	// An explicit value still overwrites: this is how a fresh spawn restamps
	// a reused run id, and how terminate() records why it killed something.
	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-1", Kind: "tty", State: "stopped",
		Generation: "gen-boot-b", Pgid: 5150, ProcStartTicks: 111,
		StopReason: "orphaned",
	}); err != nil {
		t.Fatalf("restamp: %v", err)
	}
	got, err = s.GetManagedSession(ctx, "run-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Generation != "gen-boot-b" || got.Pgid != 5150 ||
		got.ProcStartTicks != 111 || got.StopReason != "orphaned" {
		t.Fatalf("an explicit stamp must win, got %+v", got)
	}

	// And a later status persist must not blank the reason either — that is
	// what left rows reading `stopped` with no explanation.
	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-1", Kind: "tty", State: "stopped",
	}); err != nil {
		t.Fatalf("post-terminate persist: %v", err)
	}
	got, _ = s.GetManagedSession(ctx, "run-1")
	if got.StopReason != "orphaned" {
		t.Fatalf("stop reason lost, got %+v", got)
	}
}

// The narrow writers exist precisely because of the wipe above: a column
// update must not take the rest of the row with it.
func TestNarrowModelUpdateLeavesTheKillIdentityAlone(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-2", Kind: "tty", State: "running", Model: "sonnet",
		Generation: "gen-boot-a", Pgid: 4242, ProcStartTicks: 99,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SetManagedSessionModel(ctx, "run-2", "opus"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	got, err := s.GetManagedSession(ctx, "run-2")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Model != "opus" {
		t.Fatalf("model not applied: %+v", got)
	}
	if got.Generation != "gen-boot-a" || got.Pgid != 4242 || got.ProcStartTicks != 99 {
		t.Fatalf("a model change must not touch the kill identity, got %+v", got)
	}
}

// The boot reconciler asks for every live row that is
// not this boot's. What it gets back is what gets killed, so the boundaries
// matter more than the happy path.
func TestListActiveManagedSessionsNotInGeneration(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	seed := func(id, state, gen string) {
		t.Helper()
		if err := s.UpsertManagedSession(ctx, &ManagedSession{
			ID: id, Kind: "tty", State: state, Generation: gen, StartedAt: 1,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	seed("run-mine", "running", "gen-now")       // this boot: leave alone
	seed("run-foreign", "running", "gen-before") // a previous life: reap
	seed("run-foreign-starting", "starting", "gen-before")
	seed("run-foreign-awaiting", "awaiting", "gen-before")
	// Rows that predate the stamp, or that a writer blanked, are orphans
	// too: there is no way to tell "never stamped" from "stamped by a life
	// we cannot name", and both mean nobody owns the process.
	seed("run-unstamped", "running", "")
	// Already terminal: nothing to kill, and re-terminating would rewrite
	// the reason it actually ended for.
	seed("run-done", "finished", "gen-before")
	seed("run-stopped", "stopped", "gen-before")
	seed("run-crashed", "crashed", "gen-before")

	got, err := s.ListActiveManagedSessionsNotInGeneration(ctx, "gen-now")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range got {
		ids[m.ID] = true
	}
	for _, want := range []string{
		"run-foreign", "run-foreign-starting", "run-foreign-awaiting", "run-unstamped",
	} {
		if !ids[want] {
			t.Fatalf("%s is an orphan and must be reaped, got %v", want, ids)
		}
	}
	for _, unwanted := range []string{"run-mine", "run-done", "run-stopped", "run-crashed"} {
		if ids[unwanted] {
			t.Fatalf("%s must NOT be reaped, got %v", unwanted, ids)
		}
	}
	if len(got) != 4 {
		t.Fatalf("want exactly the four orphans, got %d: %v", len(got), ids)
	}

	// An empty generation would match every stamped row — every live run
	// this boot just started. Refusing beats reaping the world.
	if _, err := s.ListActiveManagedSessionsNotInGeneration(ctx, ""); err == nil {
		t.Fatal("an empty generation must be refused, not treated as a filter")
	}
}

// A tty restart reuses the run id, so the row comes back live on top of its
// own terminal record. The reason it died of last time must not ride along:
// mailapi reports that field, and a running row wearing "orphaned" is a lie.
func TestRestartClearsTheStopReasonOfThePreviousLife(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-restart", Kind: "tty", State: "stopped",
		StopReason: "orphaned", Generation: "gen-old", Pgid: 10, ProcStartTicks: 20,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// respawn: persistInitial writes the row live with a fresh stamp
	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-restart", Kind: "tty", State: "starting",
		Generation: "gen-new", Pgid: 30, ProcStartTicks: 40,
	}); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	got, err := s.GetManagedSession(ctx, "run-restart")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.StopReason != "" {
		t.Fatalf("a live row must not carry the previous life's stop reason, got %q", got.StopReason)
	}
	if got.Generation != "gen-new" || got.Pgid != 30 || got.ProcStartTicks != 40 {
		t.Fatalf("the respawn stamp must win, got %+v", got)
	}

	// And the reaper's own write survives a later status persist on a
	// terminal row — that persist is how the reason used to get lost.
	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-restart", Kind: "tty", State: "stopped", StopReason: "orphaned_unverified",
	}); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-restart", Kind: "tty", State: "stopped", EventCount: 3,
	}); err != nil {
		t.Fatalf("late persist: %v", err)
	}
	got, _ = s.GetManagedSession(ctx, "run-restart")
	if got.StopReason != "orphaned_unverified" {
		t.Fatalf("the reap reason must survive a later persist, got %q", got.StopReason)
	}
}
