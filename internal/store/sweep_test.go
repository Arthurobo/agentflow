package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// strand puts one member in the situation the sweep is meant to catch: it
// reported, holds nothing, mail is waiting, and it has not READ its inbox
// since before the grace.
func strand(t *testing.T, s *Store, member *LoopMember, msgID string, now int64) {
	t.Helper()
	ctx := context.Background()
	old := now - 10*time.Minute.Milliseconds()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET created_at = ? WHERE id = ?`, old, msgID); err != nil {
		t.Fatalf("age message: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET last_polled_at = ?, last_seen_at = ? WHERE id = ?`,
		old, now, member.ID); err != nil {
		t.Fatalf("age member: %v", err)
	}
}

// The transition is recorded once, the orchestrator is told once, and a second
// tick over the same situation says nothing.
//
// Both halves matter. Asserting only "the second tick added nothing" would
// also pass if the notice were never posted at all, which is the failure this
// whole mechanism exists to prevent, so the first tick's count is asserted to
// be exactly one rather than merely stable.
func TestTheSweepRecordsAStrandedMemberOnceAndTellsTheOrchestrator(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "sweeping")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	waiting := mustPost(t, s, orch, inv, "please check the second table")
	now := time.Now().UnixMilli()
	strand(t, s, inv, waiting.ID, now)

	first, err := s.sweepAt(ctx, now, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(first.Stranded) != 1 || first.Stranded[0].Role != "INVESTIGATION" {
		t.Fatalf("the sweep must find exactly the stranded member, got %+v", first.Stranded)
	}

	notices := engineNotices(t, s, loop.ID)
	if len(notices) != 1 {
		t.Fatalf("the orchestrator must be told exactly once, got %d", len(notices))
	}
	if notices[0].RecipientRole != RoleOrchestrator {
		t.Fatalf("the notice goes to the orchestrator, got %q", notices[0].RecipientRole)
	}
	if notices[0].SenderRole != RoleEngine {
		t.Fatalf("the notice is the engine speaking, got %q", notices[0].SenderRole)
	}
	if !strings.Contains(notices[0].Body, "INVESTIGATION") {
		t.Fatalf("the notice must name the member: %q", notices[0].Body)
	}

	// A second tick over the same unchanged situation.
	second, err := s.sweepAt(ctx, now+1000, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(second.Stranded) != 0 {
		t.Fatalf("a recorded transition must not be recorded again, got %+v", second.Stranded)
	}
	if got := engineNotices(t, s, loop.ID); len(got) != 1 {
		t.Fatalf("the orchestrator must not be told twice, got %d", len(got))
	}

	// And the member polling clears it, so a recovery is a recovery.
	if err := s.ClearStranded(ctx, inv.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	fresh, err := s.LoopMemberByID(ctx, inv.ID)
	if err != nil || fresh == nil {
		t.Fatalf("reread: %v", err)
	}
	if fresh.StrandedSince != 0 {
		t.Fatalf("polling must clear the verdict, still %d", fresh.StrandedSince)
	}
}

// A stranded ORCHESTRATOR has nobody to tell. The board still has to say so,
// or the one member whose silence matters most is the one nothing reports.
func TestAStrandedOrchestratorIsRecordedWithNoMailToItself(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "silent orchestrator")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	waiting := mustPost(t, s, inv, orch, "here is my report")
	now := time.Now().UnixMilli()
	strand(t, s, orch, waiting.ID, now)

	res, err := s.sweepAt(ctx, now, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Stranded) != 1 || res.Stranded[0].Role != RoleOrchestrator {
		t.Fatalf("the orchestrator must be reported stranded, got %+v", res.Stranded)
	}
	if got := engineNotices(t, s, loop.ID); len(got) != 0 {
		t.Fatalf("there is nobody to tell, got %d notices", len(got))
	}
	// The board's copy: an event, which is what the engineer reads.
	if !hasEvent(t, s, loop.ID, EvMemberStranded) {
		t.Fatal("the transition must reach the board even with no orchestrator to mail")
	}
}

// A member that is genuinely working must not be swept. This is the case that
// decides whether anyone trusts the badge.
func TestTheSweepLeavesAWorkingMemberAlone(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "busy")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	// Briefed, claimed, working. A second message arrives and waits.
	mustPost(t, s, orch, inv, "look into the export")
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{Limit: 10}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	waiting := mustPost(t, s, orch, inv, "and while you are there")
	now := time.Now().UnixMilli()
	strand(t, s, inv, waiting.ID, now) // ages the mail and the poll, but it HOLDS

	res, err := s.sweepAt(ctx, now, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Stranded) != 0 {
		t.Fatalf("a member holding an undischarged brief is working, got %+v", res.Stranded)
	}
	if got := engineNotices(t, s, loop.ID); len(got) != 0 {
		t.Fatalf("nothing to announce, got %d", len(got))
	}
}

// The lease is the long stop, and it never ran because nothing called it.
func TestTheSweepExpiresLeases(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "leases")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	brief := mustPost(t, s, orch, inv, "look into the export")
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{Limit: 10}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET delivered_at = ? WHERE id = ?`,
		now-2*DefaultLease.Milliseconds(), brief.ID); err != nil {
		t.Fatalf("age: %v", err)
	}

	res, err := s.sweepAt(ctx, now, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Requeued != 1 {
		t.Fatalf("the expired lease must be returned to the queue, requeued %d", res.Requeued)
	}
	got, err := s.GetLoopMessage(ctx, loop.ID, brief.ID)
	if err != nil || got == nil {
		t.Fatalf("reread: %v", err)
	}
	if got.Status != MailPending {
		t.Fatalf("an expired lease leaves the message claimable, got %q", got.Status)
	}
}

func engineNotices(t *testing.T, s *Store, loopID string) []*LoopMessage {
	t.Helper()
	rows, err := s.ListLoopMessages(context.Background(), loopID, 200)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	out := []*LoopMessage{}
	for _, m := range rows {
		if m.SenderRole == RoleEngine && strings.Contains(m.Subject, "Stranded member") {
			out = append(out, m)
		}
	}
	return out
}

func hasEvent(t *testing.T, s *Store, loopID, kind string) bool {
	t.Helper()
	// kind is a column of loop_events, so no LIKE over a JSON payload.
	var n int
	err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM loop_events WHERE loop_id = ? AND kind = ?`,
		loopID, kind).Scan(&n)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	return n > 0
}

// runFor gives a member a managed run in the state a real exit would leave.
func runFor(t *testing.T, s *Store, memberID, runID, state string, exitCode int, lastErr string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET run_id = ?, session_id = ? WHERE id = ?`,
		runID, "sess-"+runID, memberID); err != nil {
		t.Fatalf("link run: %v", err)
	}
	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: runID, Kind: "tty", CWD: "/tmp/w", State: state,
		ExitCode: exitCode, LastError: lastErr,
	}); err != nil {
		t.Fatalf("session: %v", err)
	}
}

// A member whose process died says so on its row, so the board can be red
// about something that is actually wrong.
func TestTheSweepRecordsAnAbnormalExitOnTheMemberRow(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "crashing")
	inv := roles["INVESTIGATION"]
	runFor(t, s, inv.ID, "run_inv", "crashed", 137, "claude exited with code 137")

	if _, err := s.sweepAt(ctx, time.Now().UnixMilli(), DefaultLease, StrandedGrace); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	fresh, err := s.LoopMemberByID(ctx, inv.ID)
	if err != nil || fresh == nil {
		t.Fatalf("reread: %v", err)
	}
	if fresh.LastError == "" {
		t.Fatal("a crashed member must say why on its own row")
	}
	if !strings.Contains(fresh.LastError, "137") {
		t.Fatalf("the reason must carry the exit: %q", fresh.LastError)
	}
	_ = loop
}

// And a member that ended CLEANLY must leave last_error empty.
//
// last_error is not a neutral field: the board checks it before every server
// verdict and paints the row red. A retired member, one the engineer stopped
// himself, and one that finished its work all leave a terminal run behind, and
// red that appears when nothing is wrong is red he stops reading.
func TestTheSweepLeavesACleanExitUnmarked(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "clean exits")

	for _, tc := range []struct {
		role, run, state string
		exit             int
	}{
		{"INVESTIGATION", "run_inv", "stopped", 0},
		{"REVIEW", "run_rev", "finished", 0},
	} {
		runFor(t, s, roles[tc.role].ID, tc.run, tc.state, tc.exit, "")
	}
	if _, err := s.sweepAt(ctx, time.Now().UnixMilli(), DefaultLease, StrandedGrace); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, role := range []string{"INVESTIGATION", "REVIEW"} {
		fresh, err := s.LoopMemberByID(ctx, roles[role].ID)
		if err != nil || fresh == nil {
			t.Fatalf("reread %s: %v", role, err)
		}
		if fresh.LastError != "" {
			t.Fatalf("%s ended cleanly and must not be marked failed: %q", role, fresh.LastError)
		}
	}

	// It still stops claiming to be alive: the terminal run reads stranded,
	// which is the honest signal without the alarm.
	byRole := healthByRole(t, s, roles["INVESTIGATION"].LoopID, time.Now().UnixMilli())
	if byRole["INVESTIGATION"].State != MemberStranded {
		t.Fatalf("a dead process cannot read its mail, got %q", byRole["INVESTIGATION"].State)
	}
}

// A member still running is untouched by any of this.
//
// The second case is what the terminal guard is FOR: a run row can carry an
// exit code from an earlier life, because the row is reused across a resume
// and nothing zeroes it on the way back up. Without the guard, a healthy
// member that happens to sit on such a row is painted failed, permanently,
// on the strength of a number describing a process that no longer exists.
func TestTheSweepDoesNotMarkALiveMember(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "alive")

	runFor(t, s, roles["INVESTIGATION"].ID, "run_inv", "running", 0, "")
	// Running again after a crash: the state is current, the exit code is not.
	runFor(t, s, roles["REVIEW"].ID, "run_rev", "running", 137, "claude exited with code 137")

	if _, err := s.sweepAt(ctx, time.Now().UnixMilli(), DefaultLease, StrandedGrace); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, role := range []string{"INVESTIGATION", "REVIEW"} {
		fresh, _ := s.LoopMemberByID(ctx, roles[role].ID)
		if fresh.LastError != "" {
			t.Fatalf("%s is running and must not be marked: %q", role, fresh.LastError)
		}
	}
}
