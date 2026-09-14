package store

import (
	"context"
	"testing"
	"time"
)

const testGrace = 90 * time.Second

// The four states, stated as the situations they are meant to distinguish.
// The one that matters most is working: a member mid-way through a twenty
// minute task holds its brief and is not polling, and calling that stranded is
// how a detector like this loses its credibility on the first day.
func TestMemberStateSeparatesWorkingFromStranded(t *testing.T) {
	now := int64(1_000_000_000)
	old := now - 10*time.Minute.Milliseconds()
	fresh := now - 5*time.Second.Milliseconds()

	for _, tc := range []struct {
		name string
		h    MemberHealth
		want string
	}{
		{
			"a held line is direct evidence and beats everything",
			MemberHealth{Listening: true, OldestPendingAt: old, LastSeenAt: old},
			MemberListening,
		},
		{
			"holding a brief for twenty minutes is working, not stranded",
			MemberHealth{Held: 1, OldestHeldAt: now - 20*time.Minute.Milliseconds(), LastSeenAt: old},
			MemberWorking,
		},
		{
			"new mail arriving while it works does not strand it",
			MemberHealth{Held: 1, OldestHeldAt: fresh, Pending: 1, OldestPendingAt: old, LastSeenAt: old},
			MemberWorking,
		},
		{
			"past the lease with no extend is overdue, not stranded",
			MemberHealth{Held: 1, OldestHeldAt: now - 40*time.Minute.Milliseconds(), LastSeenAt: old},
			MemberOverdue,
		},
		{
			"the observed failure: reported, holds nothing, mail waiting, gone quiet",
			MemberHealth{Held: 0, Pending: 1, OldestPendingAt: old, LastSeenAt: old, LastPolledAt: old},
			MemberStranded,
		},
		{
			"a short-poll loop between polls has read its inbox and is not stranded",
			MemberHealth{Held: 0, Pending: 1, OldestPendingAt: old, LastSeenAt: fresh, LastPolledAt: fresh},
			MemberWorking,
		},
		{
			// The case one step past the observed failure: the discharge
			// correctly emptied what it held, and it then kept narrating its
			// own work. Alive on every other measure, and still nobody is
			// reading its mail. last_seen_at would call this healthy forever.
			"busy writing notes but not reading its inbox is still stranded",
			MemberHealth{Held: 0, Pending: 1, OldestPendingAt: old, LastSeenAt: fresh, LastPolledAt: old},
			MemberStranded,
		},
		{
			"mail that only just arrived is not evidence of anything yet",
			MemberHealth{Held: 0, Pending: 1, OldestPendingAt: fresh, LastSeenAt: old, LastPolledAt: old},
			MemberWorking,
		},
		{
			"an empty queue and no line is idle, which is not stranded",
			MemberHealth{Held: 0, Pending: 0, LastSeenAt: old, LastPolledAt: old},
			MemberWorking,
		},
		{
			"a dead process cannot read anything, whatever the queue says",
			MemberHealth{Held: 1, OldestHeldAt: fresh, RunState: "crashed", LastSeenAt: fresh},
			MemberStranded,
		},
		{
			"but a listening member with a stale run row is still listening",
			MemberHealth{Listening: true, RunState: "crashed"},
			MemberListening,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := memberStateFrom(tc.h, now, DefaultLease, testGrace); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// End to end against real rows: the investigator that reported and stopped.
func TestLoopHealthReadsTheRowsNotAGuess(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "health")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	// It is briefed and claims, so it holds one: working.
	mustPost(t, s, orch, inv, "look into the export")
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{Limit: 10}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	now := time.Now().UnixMilli()
	byRole := healthByRole(t, s, loop.ID, now)
	if byRole["INVESTIGATION"].State != MemberWorking {
		t.Fatalf("holding a brief is working, got %q", byRole["INVESTIGATION"].State)
	}
	if byRole["INVESTIGATION"].Held != 1 {
		t.Fatalf("held = %d", byRole["INVESTIGATION"].Held)
	}
	if _, ok := byRole[RoleEngineer]; ok {
		t.Fatal("the engineer is a human, not a poller, and has no health")
	}

	// It reports and then new mail arrives and sits. There is no play here,
	// so the discharge does not fire; that path has its own test and this one
	// is about reading the rows.
	mustPost(t, s, inv, orch, "here is what I found")
	waiting := mustPost(t, s, orch, inv, "one follow-up question")

	// Age that waiting message past the grace.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET created_at = ? WHERE id = ?`,
		now-10*time.Minute.Milliseconds(), waiting.ID); err != nil {
		t.Fatalf("age: %v", err)
	}
	// Its LAST SEEN is fresh, because it is still writing notes. Only its
	// last POLL is old. The verdict must not depend on the fresh one.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET last_seen_at = ?, last_polled_at = ?, last_posted_at = ? WHERE id = ?`,
		now, now-10*time.Minute.Milliseconds(), now-10*time.Minute.Milliseconds(), inv.ID); err != nil {
		t.Fatalf("age member: %v", err)
	}
	// And discharge what it still holds, since it demonstrably acted.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET status = ? WHERE recipient_id = ? AND status = ?`,
		MailAcked, inv.ID, MailDelivered); err != nil {
		t.Fatalf("discharge: %v", err)
	}

	byRole = healthByRole(t, s, loop.ID, now)
	if got := byRole["INVESTIGATION"].State; got != MemberStranded {
		t.Fatalf("nobody is reading its mail, got %q (%+v)", got, byRole["INVESTIGATION"])
	}
	if byRole["INVESTIGATION"].Pending != 1 {
		t.Fatalf("pending = %d", byRole["INVESTIGATION"].Pending)
	}
}

func healthByRole(t *testing.T, s *Store, loopID string, now int64) map[string]MemberHealth {
	t.Helper()
	rows, err := s.loopHealthAt(context.Background(), loopID, now, DefaultLease, testGrace)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	out := map[string]MemberHealth{}
	for _, h := range rows {
		out[h.Role] = h
	}
	return out
}

// A spawn in flight and a member running elsewhere are process facts, not
// mailbox facts, and the server answers them so the board has one source of
// truth rather than two that can disagree.
func TestHealthAnswersTheProcessQuestionsToo(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "spawning")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	// The orchestrator is coming up: a run id exists, no session yet.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET run_id = ?, session_id = '' WHERE id = ?`, "run_o", orch.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	now := time.Now().UnixMilli()
	byRole := healthByRole(t, s, loop.ID, now)
	if byRole[RoleOrchestrator].State != MemberStarting {
		t.Fatalf("a run with no session is starting, got %q", byRole[RoleOrchestrator].State)
	}
	// The investigator has neither, so it runs somewhere we did not start.
	if byRole["INVESTIGATION"].State != MemberExternal {
		t.Fatalf("no run at all is external, got %q", byRole["INVESTIGATION"].State)
	}

	// But an EXTERNAL member can still be stranded, and that is exactly the
	// case a nudge cannot reach, so the mailbox verdict must win.
	waiting := mustPost(t, s, orch, inv, "please look at this")
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET created_at = ? WHERE id = ?`,
		now-10*time.Minute.Milliseconds(), waiting.ID); err != nil {
		t.Fatalf("age: %v", err)
	}
	byRole = healthByRole(t, s, loop.ID, now)
	if byRole["INVESTIGATION"].State != MemberStranded {
		t.Fatalf("a laptop session that stopped reading is still stranded, got %q",
			byRole["INVESTIGATION"].State)
	}
}
