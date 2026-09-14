package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Migration 0027 gives loops the generation column managed_sessions has had
// since 0014. Without it the boot reaper was only half implemented: the
// boot reaper ended a restarted loop's member runs and left the parent row
// reading "active" with nothing behind it.

func TestTheLoopsTableCarriesAGenerationThatDefaultsToEmpty(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	var name, typ string
	var notNull int
	var dflt any
	err := s.db.QueryRowContext(ctx,
		`SELECT name, type, "notnull", dflt_value FROM pragma_table_info('loops')
		 WHERE name = 'generation'`).Scan(&name, &typ, &notNull, &dflt)
	if err != nil {
		t.Fatalf("the migration must add loops.generation: %v", err)
	}
	if typ != "TEXT" || notNull != 1 {
		t.Fatalf("generation is %s notnull=%d, want TEXT notnull=1", typ, notNull)
	}

	// A loop created without one reads back empty rather than NULL, which is
	// what lets the reaper treat "never stamped" and "stamped by a life we
	// cannot name" identically.
	l := &Loop{Task: "unstamped"}
	if _, err := s.CreateLoop(ctx, l, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetLoop(ctx, l.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %+v %v", got, err)
	}
	if got.Generation != "" {
		t.Fatalf("generation = %q, want empty", got.Generation)
	}
}

func TestCreateLoopAndCreateCrewStampTheGeneration(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	viaLoop := &Loop{Task: "one", Generation: "gen-abc"}
	if _, err := s.CreateLoop(ctx, viaLoop, nil); err != nil {
		t.Fatalf("CreateLoop: %v", err)
	}
	viaCrew := &Loop{Task: "two", Generation: "gen-abc"}
	if _, err := s.CreateCrew(ctx, viaCrew, RoleSpecs(RoleOrchestrator)); err != nil {
		t.Fatalf("CreateCrew: %v", err)
	}
	for _, id := range []string{viaLoop.ID, viaCrew.ID} {
		got, err := s.GetLoop(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("get %s: %+v %v", id, got, err)
		}
		if got.Generation != "gen-abc" {
			t.Errorf("%s generation = %q, want gen-abc", id, got.Generation)
		}
	}
	// And it survives the list read, not only the single-row read: the
	// reaper scans through a list.
	loops, err := s.ListLoops(ctx, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, l := range loops {
		if l.Generation != "gen-abc" {
			t.Errorf("%s read back %q from the list", l.ID, l.Generation)
		}
	}
}

func TestListActiveLoopsNotInGeneration(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	mk := func(task, gen, status string) *Loop {
		l := &Loop{Task: task, Generation: gen}
		if _, err := s.CreateLoop(ctx, l, nil); err != nil {
			t.Fatalf("create %s: %v", task, err)
		}
		if status != LoopActive {
			if _, err := s.EndLoop(ctx, l.ID, status, "seeded", false); err != nil {
				t.Fatalf("end %s: %v", task, err)
			}
		}
		return l
	}

	foreign := mk("from a dead daemon", "gen-old", LoopActive)
	unstamped := mk("predates the column", "", LoopActive)
	mine := mk("this life", "gen-now", LoopActive)
	done := mk("finished", "gen-old", LoopComplete)
	killed := mk("killed", "gen-old", LoopKilled)
	already := mk("already reaped", "gen-old", LoopInterrupted)

	got, err := s.ListActiveLoopsNotInGeneration(ctx, "gen-now")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := map[string]bool{}
	for _, l := range got {
		found[l.ID] = true
	}
	if !found[foreign.ID] {
		t.Error("a foreign generation's active loop is an orphan")
	}
	if !found[unstamped.ID] {
		t.Error("an unstamped active loop is an orphan; this is what catches the ones already on disk")
	}
	if found[mine.ID] {
		t.Error("this life's loop is not an orphan")
	}
	for _, l := range []*Loop{done, killed, already} {
		if found[l.ID] {
			t.Errorf("%s is terminal and must not be reaped again", l.Task)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want exactly the two orphans, got %d", len(got))
	}
}

// With no generation every loop looks foreign, so obeying the caller would
// end the loops the boot is about to start.
func TestListActiveLoopsNotInGenerationRefusesAnEmptyGeneration(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()

	got, err := s.ListActiveLoopsNotInGeneration(context.Background(), "")
	if err == nil {
		t.Fatalf("an empty generation must be refused, got %d loops", len(got))
	}
	if !strings.Contains(err.Error(), "empty generation") {
		t.Errorf("error = %v, want it to name the reason", err)
	}
}

// An interrupted loop is ended, so nothing keeps taking work for it.
func TestAnInterruptedLoopReadsAsEnded(t *testing.T) {
	l := &Loop{Status: LoopInterrupted}
	if !l.Ended() {
		t.Fatal("interrupted must be terminal, or the mailbox keeps accepting posts")
	}
}

// decision 9, verified rather than assumed: the sweep is what
// announces trouble, and it iterates only ACTIVE loops. Interrupting a loop
// is therefore what stops it being swept — no separate gate is needed, but
// it has to actually hold.
//
// Before the reaper existed this was the live pathology: six dead loops were
// still being leased, still having members marked stranded, and still having
// notices posted into a mailbox nobody was reading.
func TestTheSweepIgnoresAnInterruptedLoop(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	loop, roles := mustLoop(t, s, "about to be interrupted")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]
	waiting := mustPost(t, s, orch, inv, "please check the second table")
	now := time.Now().UnixMilli()
	strand(t, s, inv, waiting.ID, now)

	// Control first: while it is active the sweep does find it, so the
	// assertion below is about the status and not about a quiet fixture.
	live, err := s.sweepAt(ctx, now, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(live.Stranded) != 1 {
		t.Fatalf("an active loop's stranded member must be found, got %+v", live.Stranded)
	}

	// Now interrupt it exactly as the boot reaper does, on a fresh loop in
	// the same situation.
	loop2, roles2 := mustLoop(t, s, "already interrupted")
	orch2, inv2 := roles2[RoleOrchestrator], roles2["INVESTIGATION"]
	waiting2 := mustPost(t, s, orch2, inv2, "please check the second table")
	strand(t, s, inv2, waiting2.ID, now)
	if _, err := s.EndLoop(ctx, loop2.ID, LoopInterrupted, "agentd restarted", false); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	after, err := s.sweepAt(ctx, now, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, st := range after.Stranded {
		if st.LoopID == loop2.ID {
			t.Fatalf("an interrupted loop must not be swept, got %+v", st)
		}
	}
	if n := len(engineNotices(t, s, loop2.ID)); n != 0 {
		t.Fatalf("an interrupted loop must announce nothing, got %d notices", n)
	}
	_ = loop
}
