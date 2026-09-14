package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func loopEventCount(t *testing.T, s *Store, loopID, kind string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM loop_events WHERE loop_id = ? AND kind = ?`, loopID, kind).Scan(&n); err != nil {
		t.Fatalf("events: %v", err)
	}
	return n
}

// A loop row with no start time must not read as decades over its wall-clock
// limit. The migration that added the column only stamped active loops, so
// every other loop had a zero there.
func TestBudgetFallsBackToCreationWhenStartIsUnset(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, _ := mustLoop(t, s, "an old loop")
	if _, err := s.db.ExecContext(ctx, `UPDATE loops SET started_at = 0 WHERE id = ?`, loop.ID); err != nil {
		t.Fatalf("unset: %v", err)
	}
	b, err := s.Budget(ctx, loop.ID)
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	if b.Tripped {
		t.Fatalf("a freshly created loop with no start time must not be over budget: %+v", b)
	}
}

func TestBudgetTripsOnEachLimit(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	t.Run("messages", func(t *testing.T) {
		loop, roles := mustLoop(t, s, "chatty")
		if _, err := s.db.ExecContext(ctx, `UPDATE loops SET max_messages_per_round = 2 WHERE id = ?`, loop.ID); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			mustPost(t, s, roles[RoleOrchestrator], roles["REVIEW"], "again")
		}
		b, err := s.Budget(ctx, loop.ID)
		if err != nil || !b.Tripped || !strings.Contains(b.Reason, "messages") {
			t.Fatalf("three messages over a limit of two must trip: %+v %v", b, err)
		}
	})
	t.Run("wall clock", func(t *testing.T) {
		loop, _ := mustLoop(t, s, "slow")
		b, err := s.budgetAt(ctx, loop.ID, time.Now().Unix()+DefaultMaxWallClockSeconds+60)
		if err != nil || !b.Tripped || !strings.Contains(b.Reason, "ran for") {
			t.Fatalf("a loop past its wall clock must trip: %+v %v", b, err)
		}
		if b, _ := s.budgetAt(ctx, loop.ID, time.Now().Unix()+60); b.Tripped {
			t.Fatalf("a loop inside its wall clock must not trip: %+v", b)
		}
	})
	t.Run("members", func(t *testing.T) {
		loop, _ := mustLoop(t, s, "crowded")
		if _, err := s.db.ExecContext(ctx, `UPDATE loops SET max_members = 2 WHERE id = ?`, loop.ID); err != nil {
			t.Fatal(err)
		}
		b, err := s.Budget(ctx, loop.ID)
		if err != nil || !b.Tripped || !strings.Contains(b.Reason, "members") {
			t.Fatalf("a crew over its member limit must trip: %+v %v", b, err)
		}
	})
}

// Capped is an ended state, capping only ever ends an ACTIVE loop, and it
// leaves a record of why.
func TestCapLoopEndsOnlyAnActiveLoop(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "runaway")
	mustPost(t, s, roles[RoleOrchestrator], roles["REVIEW"], "pending work")

	ended, err := s.CapLoop(ctx, loop.ID, "it carried too many messages")
	if err != nil || !ended {
		t.Fatalf("cap: %v %v", ended, err)
	}
	got, _ := s.GetLoop(ctx, loop.ID)
	if got.Status != LoopCapped || !got.Ended() {
		t.Fatalf("a capped loop must read as ended: %+v", got)
	}
	if n := loopEventCount(t, s, loop.ID, EvLoopCapped); n != 1 {
		t.Fatalf("capping must record one event, got %d", n)
	}
	counts, _ := s.CountMailByStatus(ctx, loop.ID)
	if counts[MailPending] != 0 {
		t.Fatalf("a capped loop must cancel its queue: %v", counts)
	}

	killed, _ := mustLoop(t, s, "the engineer got there first")
	if _, err := s.EndLoop(ctx, killed.ID, LoopKilled, "stop", false); err != nil {
		t.Fatal(err)
	}
	if ended, err := s.CapLoop(ctx, killed.ID, "late"); err != nil || ended {
		t.Fatalf("capping a loop that already ended must do nothing: %v %v", ended, err)
	}
	if got, _ := s.GetLoop(ctx, killed.ID); got.Status != LoopKilled {
		t.Fatalf("the engineer's choice must stand, got %s", got.Status)
	}
}

// A finished play is ended once the orchestrator has reported, or once the
// grace for doing so has passed, and not before.
func TestLoopsToFinishWaitsForTheOrchestratorToReport(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "finish me")
	orch, inv, engineer := roles[RoleOrchestrator], roles["INVESTIGATION"], roles[RoleEngineer]
	mustStartPlay(t, s, loop.ID, "recon")
	send(t, s, loop.ID, orch, inv, "", "investigate")
	send(t, s, loop.ID, inv, orch, "", "conclude")

	now := time.Now().UnixMilli()
	due, err := s.loopsToFinishAt(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("the orchestrator has not reported yet and must be given the chance: %+v", due)
	}
	if due, _ := s.loopsToFinishAt(ctx, now+PlayWrapUpGrace.Milliseconds()+1000); len(due) != 1 || due[0].Status != LoopComplete {
		t.Fatalf("after the grace the loop must be finished as complete: %+v", due)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := s.PostMail(ctx, orch, engineer, MailPost{Body: "done, here is the answer"}); err != nil {
		t.Fatal(err)
	}
	due, err = s.loopsToFinishAt(ctx, time.Now().UnixMilli())
	if err != nil || len(due) != 1 || due[0].LoopID != loop.ID {
		t.Fatalf("once the orchestrator reported the loop must be finished: %+v %v", due, err)
	}
}

func TestCreateCrewAndAddMemberRefuseOverTheMemberLimit(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	roles := []string{RoleOrchestrator, "A", "B", "C", "D", "E", "F", "G", "H"}
	if _, err := s.CreateLoop(ctx, &Loop{Task: "too many"}, roles); !errors.Is(err, ErrTooManyMembers) {
		t.Fatalf("nine roles plus the engineer is over the default limit, got %v", err)
	}
	loop := &Loop{Task: "just enough", MaxMembers: 3}
	if _, err := s.CreateLoop(ctx, loop, []string{RoleOrchestrator, "REVIEW"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := s.AddLoopMember(ctx, loop.ID, MemberSpec{Role: "INVESTIGATION"}); !errors.Is(err, ErrTooManyMembers) {
		t.Fatalf("a fourth member over a limit of three must be refused, got %v", err)
	}
	if roster, _ := s.ListLoopMembers(ctx, loop.ID); len(roster) != 3 {
		t.Fatalf("a refused add must write nothing, roster is %d", len(roster))
	}
}

func TestEngineIsAReservedRole(t *testing.T) {
	for _, role := range []string{"ENGINE", "engine", "ENGINE#2"} {
		if err := ValidateRole(role); !errors.Is(err, ErrInvalidRole) {
			t.Errorf("%q must be refused, got %v", role, err)
		}
	}
}

// Retiring a role takes it out of the play in the same transaction: off the
// step's awaiting list, and with its pending and held mail cancelled.
func TestRetireLoopMemberTakesTheRoleOutOfThePlay(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "retire the investigator")
	orch, inv, engineer := roles[RoleOrchestrator], roles["INVESTIGATION"], roles[RoleEngineer]
	mustStartPlay(t, s, loop.ID, "recon")
	held := send(t, s, loop.ID, orch, inv, "", "investigate")
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{}); err != nil {
		t.Fatal(err)
	}
	pending := mustPost(t, s, engineer, inv, "and one more thing")
	before, _ := s.GetLoop(ctx, loop.ID)
	if !containsRole(before.StepAwaiting, "INVESTIGATION") {
		t.Fatalf("the step must be awaiting the investigator: %v", before.StepAwaiting)
	}

	member, err := s.RetireLoopMember(ctx, loop.ID, "investigation")
	if err != nil || member == nil || member.Status != LoopMemberDismissed {
		t.Fatalf("retire: %+v %v", member, err)
	}
	got, _ := s.GetLoop(ctx, loop.ID)
	if containsRole(got.StepAwaiting, "INVESTIGATION") {
		t.Fatalf("the retired role must leave the step's awaiting list: %v", got.StepAwaiting)
	}
	for _, id := range []string{held.ID, pending.ID} {
		m, _ := s.GetLoopMessage(ctx, loop.ID, id)
		if m.Status != MailCancelled {
			t.Fatalf("message %s to the retired member is %s, want cancelled", id, m.Status)
		}
	}
	if n := loopEventCount(t, s, loop.ID, EvMemberChanged); n == 0 {
		t.Fatal("retiring must be an event")
	}
	if missing, err := s.RetireLoopMember(ctx, loop.ID, "NOBODY"); err != nil || missing != nil {
		t.Fatalf("an unknown role is nil, nil: %+v %v", missing, err)
	}
}

// Dismissal is computed from the members being dismissed now, not from
// whoever was dismissed before.
func TestDismissLoopMembersActsOnTheMembersItDismisses(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "dismiss everyone")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]
	mustStartPlay(t, s, loop.ID, "recon")
	before, _ := s.GetLoop(ctx, loop.ID)
	if !containsRole(before.StepAwaiting, RoleOrchestrator) {
		t.Fatalf("the entry step must await the orchestrator: %v", before.StepAwaiting)
	}
	held := mustPost(t, s, orch, inv, "go")
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{}); err != nil {
		t.Fatal(err)
	}

	n, err := s.DismissLoopMembers(ctx, loop.ID)
	if err != nil || n == 0 {
		t.Fatalf("dismiss: %d %v", n, err)
	}
	after, _ := s.GetLoop(ctx, loop.ID)
	if len(after.StepAwaiting) != 0 {
		t.Fatalf("every dismissed role must leave the awaiting list: %v", after.StepAwaiting)
	}
	if m, _ := s.GetLoopMessage(ctx, loop.ID, held.ID); m.Status != MailCancelled {
		t.Fatalf("held mail of a dismissed member must be cancelled, is %s", m.Status)
	}
}

// A push claim takes only pending mail and counts nothing until the write is
// confirmed; confirming it counts once and clears a stranded mark.
func TestPushClaimCountsOnlyConfirmedDeliveries(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "push")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]
	msg := mustPost(t, s, orch, inv, "twelve characters")
	if _, err := s.MarkStranded(ctx, inv.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimInbox(ctx, inv, ClaimOptions{Push: true})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d %v", len(claimed), err)
	}
	m, _ := s.GetLoopMessage(ctx, loop.ID, msg.ID)
	member, _ := s.LoopMemberByID(ctx, inv.ID)
	if m.DeliveryCount != 0 || member.CharsIn != 0 {
		t.Fatalf("nothing may be counted before the write: count=%d chars=%d", m.DeliveryCount, member.CharsIn)
	}
	// Held mail is the member's to work on; a second push claim takes nothing.
	if again, _ := s.ClaimInbox(ctx, inv, ClaimOptions{Push: true, RedeliverAfter: new(time.Duration)}); len(again) != 0 {
		t.Fatalf("a push claim must never take held mail, got %d", len(again))
	}

	if err := s.MarkDelivered(ctx, inv.ID, []string{msg.ID}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	m, _ = s.GetLoopMessage(ctx, loop.ID, msg.ID)
	member, _ = s.LoopMemberByID(ctx, inv.ID)
	if m.DeliveryCount != 1 || member.CharsIn != int64(len("twelve characters")) {
		t.Fatalf("a confirmed write counts once: count=%d chars=%d", m.DeliveryCount, member.CharsIn)
	}
	if member.StrandedSince != 0 {
		t.Fatal("a member that was just handed its mail is not stranded")
	}
}

// A lease that keeps running out is escalated on the third expiry instead of
// the body being re-sent forever.
func TestTheThirdLeaseExpiryEscalatesInsteadOfRequeueing(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "never finished")
	orch, review := roles[RoleOrchestrator], roles["REVIEW"]
	msg := mustPost(t, s, orch, review, "review this")

	for i := 1; i <= MaxLeaseExpiries; i++ {
		if claimed, err := s.ClaimInbox(ctx, review, ClaimOptions{}); err != nil || len(claimed) != 1 {
			t.Fatalf("claim %d: %d %v", i, len(claimed), err)
		}
		n, err := s.RequeueExpiredMail(ctx, 0)
		if err != nil {
			t.Fatalf("requeue %d: %v", i, err)
		}
		m, _ := s.GetLoopMessage(ctx, loop.ID, msg.ID)
		if i < MaxLeaseExpiries {
			if n != 1 || m.Status != MailPending {
				t.Fatalf("expiry %d must requeue: n=%d status=%s", i, n, m.Status)
			}
			continue
		}
		if n != 0 || m.Status != MailCancelled {
			t.Fatalf("expiry %d must stop re-sending: n=%d status=%s", i, n, m.Status)
		}
	}
	if loopEventCount(t, s, loop.ID, EvMailUndeliverable) != 1 {
		t.Fatal("the escalation must be a loop event")
	}
	inbox, _ := s.ListLoopMessages(ctx, loop.ID, 100)
	told := false
	for _, m := range inbox {
		if m.SenderRole == RoleEngine && m.RecipientRole == RoleOrchestrator && strings.Contains(m.Subject, "Undeliverable") {
			told = true
		}
	}
	if !told {
		t.Fatal("the orchestrator must be told the work was never finished")
	}
}

func TestExtendMailLeaseIsCappedAtAnHour(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "long lease")
	msg := mustPost(t, s, roles[RoleOrchestrator], roles["REVIEW"], "take your time")
	if _, err := s.ClaimInbox(ctx, roles["REVIEW"], ClaimOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ExtendMailLease(ctx, roles["REVIEW"].ID, msg.ID, 7*24*3600); err != nil || !ok {
		t.Fatalf("extend: %v %v", ok, err)
	}
	m, _ := s.GetLoopMessage(ctx, loop.ID, msg.ID)
	if m.LeaseSeconds != int(MaxLeaseExtension.Seconds()) {
		t.Fatalf("a week-long extension must be capped at an hour, got %ds", m.LeaseSeconds)
	}
}

func TestMembersAwaitingDeliverySkipsEndedRuns(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "dead runs")
	orch, inv, review := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["REVIEW"]
	runFor(t, s, inv.ID, "run_dead", "crashed", 1, "boom")
	runFor(t, s, review.ID, "run_live", "running", 0, "")
	mustPost(t, s, orch, inv, "for the dead one")
	mustPost(t, s, orch, review, "for the live one")

	got, err := s.MembersAwaitingDelivery(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range got {
		ids[m.ID] = true
	}
	if ids[inv.ID] {
		t.Fatal("a member whose run crashed has no terminal to deliver into")
	}
	if !ids[review.ID] {
		t.Fatal("a member with a live run must be delivered to")
	}
}

// A play that refuses the crew refuses the create, and nothing is left
// behind: no loop, no members, no task.
func TestStartLoopWritesNothingWhenThePlayRefusesTheCrew(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop := &Loop{Task: "deep work with nobody to do it"}
	_, err := s.StartLoop(ctx, loop, RoleSpecs(RoleOrchestrator, "INVESTIGATION"), "deep", "do the deep work")
	if !errors.Is(err, ErrPlayInvalid) {
		t.Fatalf("deep without implementer and reviewer must be refused, got %v", err)
	}
	if got, _ := s.ListLoops(ctx, "", 0); len(got) != 0 {
		t.Fatalf("a refused create must leave no loop, found %d", len(got))
	}

	ok := &Loop{Task: "recon with an investigator"}
	tokens, err := s.StartLoop(ctx, ok, RoleSpecs(RoleOrchestrator, "INVESTIGATION"), "recon", "find the flaky test")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if tokens["INVESTIGATION"] == "" {
		t.Fatal("tokens must be issued")
	}
	got, _ := s.GetLoop(ctx, ok.ID)
	if got.PlayStatus != PlayRunning || got.StartedAt == 0 {
		t.Fatalf("the loop must be on its play with a start time: %+v", got)
	}
	msgs, _ := s.ListLoopMessages(ctx, ok.ID, 10)
	if len(msgs) < 2 || msgs[0].Body != "find the flaky test" || msgs[0].SenderRole != RoleEngineer {
		t.Fatalf("the task must be the orchestrator's first message: %+v", msgs)
	}
}

func TestContinueLoopWritesNothingWhenItFails(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	successor := &Loop{Task: "round two"}
	if _, err := s.ContinueLoop(ctx, "loop_missing", successor, RoleSpecs(RoleOrchestrator, "INVESTIGATION"),
		"recon", "round two", false); !errors.Is(err, ErrMailNotFound) {
		t.Fatalf("continuing a loop that does not exist must fail, got %v", err)
	}
	if got, _ := s.ListLoops(ctx, "", 0); len(got) != 0 {
		t.Fatalf("a failed continue must leave no successor, found %d", len(got))
	}
}

// An advisory reaches the orchestrator without being a post from anyone: the
// play does not move and nobody's brief is discharged.
func TestAnAdvisoryNeitherAdvancesThePlayNorDischarges(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "advise")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]
	mustStartPlay(t, s, loop.ID, "recon")
	send(t, s, loop.ID, orch, inv, "", "investigate")
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{}); err != nil {
		t.Fatal(err)
	}

	msg, err := s.PostAdvisory(ctx, loop.ID, RoleOrchestrator, "captured output", "the answer is 42", nil)
	if err != nil {
		t.Fatalf("advisory: %v", err)
	}
	if msg.SenderRole != RoleEngine || msg.RecipientRole != RoleOrchestrator {
		t.Fatalf("an advisory is from the engine to the orchestrator: %+v", msg)
	}
	got, _ := s.GetLoop(ctx, loop.ID)
	if got.StepID != "investigate" {
		t.Fatalf("an advisory must not move the play, it is on %s", got.StepID)
	}
	health, _ := s.LoopHealth(ctx, loop.ID)
	for _, h := range health {
		if h.MemberID == inv.ID && h.HeldBriefs == 0 {
			t.Fatal("an advisory must not discharge the member's brief")
		}
	}
	if loopEventCount(t, s, loop.ID, EvAutoForwarded) != 1 {
		t.Fatal("an advisory must be an event")
	}
}
