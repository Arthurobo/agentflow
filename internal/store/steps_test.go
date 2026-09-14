// steps_test.go — the play engine in motion. The sequence is enforced in the
// delivery layer, so these tests send real messages and require the refusals:
// out of turn, wrong recipient, no verdict where a verdict decides, and a
// fan-out step that will not advance while one member is still out.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func mustLoopWithRoles(t *testing.T, s *Store, task string, roles []string) (*Loop, map[string]*LoopMember) {
	t.Helper()
	ctx := context.Background()
	// Some rosters here are larger than the default member limit, so the
	// limit is raised for them explicitly.
	l := &Loop{Task: task, CWD: "/tmp/target", MaxMembers: 64}
	if _, err := s.CreateLoop(ctx, l, roles); err != nil {
		t.Fatalf("create loop: %v", err)
	}
	members, err := s.ListLoopMembers(ctx, l.ID)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	byRole := map[string]*LoopMember{}
	for _, m := range members {
		byRole[m.Role] = m
	}
	return l, byRole
}

func mustStartPlay(t *testing.T, s *Store, loopID, play string) *Loop {
	t.Helper()
	l, err := s.StartPlay(context.Background(), loopID, play)
	if err != nil {
		t.Fatalf("StartPlay %s: %v", play, err)
	}
	return l
}

// send posts and asserts the play position afterwards.
func send(t *testing.T, s *Store, loopID string, from, to *LoopMember, outcome, wantStep string) *LoopMessage {
	t.Helper()
	m, err := s.PostMail(context.Background(), from, to,
		MailPost{Body: from.Role + " to " + to.Role, Outcome: outcome})
	if err != nil {
		t.Fatalf("%s -> %s: %v", from.Role, to.Role, err)
	}
	l, err := s.GetLoop(context.Background(), loopID)
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if l.StepID != wantStep {
		t.Fatalf("after %s -> %s the loop is on %s, want %s", from.Role, to.Role, l.StepID, wantStep)
	}
	return m
}

func violation(t *testing.T, err error) *StepViolation {
	t.Helper()
	if err == nil {
		t.Fatal("expected a step violation, got no error")
	}
	if !errors.Is(err, ErrMailStepViolation) {
		t.Fatalf("error must match ErrMailStepViolation: %v", err)
	}
	var v *StepViolation
	if !errors.As(err, &v) {
		t.Fatalf("error must carry a *StepViolation a transport can render: %v", err)
	}
	return v
}

// The engine advances on the message that satisfies the step, and on nothing
// else. Nobody declares a step done.
func TestPlayAdvancesOnlyOnTheMessageThatSatisfiesTheStep(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "recon")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	started := mustStartPlay(t, s, loop.ID, "recon")
	if started.StepID != "brief" || started.PlayStatus != PlayRunning {
		t.Fatalf("a started play stands on its entry step: %+v", started)
	}
	if strings.Join(started.StepAwaiting, ",") != RoleOrchestrator {
		t.Fatalf("entry step awaits the orchestrator, got %v", started.StepAwaiting)
	}

	brief := send(t, s, loop.ID, orch, inv, "", "investigate")
	if brief.StepID != "brief" {
		t.Fatalf("a message records the step that produced it, got %q", brief.StepID)
	}

	// The investigator is now the only party the step is waiting for, and
	// the orchestrator writing again does not move anything.
	if _, err := s.PostMail(ctx, orch, inv, MailPost{Body: "again"}); err == nil {
		t.Fatal("the orchestrator must not be able to re-send its way past a step")
	}

	send(t, s, loop.ID, inv, orch, "", "conclude")
	done, _ := s.GetLoop(ctx, loop.ID)
	if done.PlayStatus != PlayDone {
		t.Fatalf("reaching the terminal step completes the play, got %q", done.PlayStatus)
	}
	if len(done.StepAwaiting) != 0 {
		t.Fatalf("a completed play waits for nobody, got %v", done.StepAwaiting)
	}
}

// The refusal names the step it violated so a transport can render it rather
// than swallowing it.
func TestOutOfStepMessageIsRefusedWithTheStepItViolated(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "audit")
	orch, inv, impl, review := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["IMPLEMENTATION"], roles["REVIEW"]
	mustStartPlay(t, s, loop.ID, "audit")

	// Wrong recipient: audit briefs the investigator first, not the
	// implementer.
	v := violation(t, mustErr(s.PostMail(ctx, orch, impl, MailPost{Body: "skip ahead"})))
	if v.Play != "audit" || v.StepID != "brief" || v.StepActor != RoleOrchestrator {
		t.Fatalf("violation must name the play and step: %+v", v)
	}
	if v.FromRole != RoleOrchestrator || v.ToRole != "IMPLEMENTATION" {
		t.Fatalf("violation must name both parties: %+v", v)
	}
	if strings.Join(v.Allowed, ",") != "INVESTIGATION" {
		t.Fatalf("violation must say what the step would accept, got %v", v.Allowed)
	}
	if !strings.Contains(v.Error(), "audit") || !strings.Contains(v.Error(), "brief") {
		t.Fatalf("the rendered message must carry the step: %q", v.Error())
	}

	// Out of turn: a worker reporting before it was briefed.
	send(t, s, loop.ID, orch, inv, "", "investigate")
	v = violation(t, mustErr(s.PostMail(ctx, review, orch, MailPost{Body: "unasked"})))
	if v.StepID != "investigate" || !strings.Contains(v.Reason, "not this member's turn") {
		t.Fatalf("an unasked worker must be refused by turn: %+v", v)
	}
	if strings.Join(v.Awaiting, ",") != "INVESTIGATION" {
		t.Fatalf("violation must say who the step is waiting for, got %v", v.Awaiting)
	}
}

// Deep cycles between implementation and review until review reports clean,
// and every trip round the loop is counted.
func TestDeepCyclesUntilReviewReportsClean(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "deep")
	orch, inv, impl, review := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["IMPLEMENTATION"], roles["REVIEW"]
	mustStartPlay(t, s, loop.ID, "deep")

	send(t, s, loop.ID, orch, inv, "", "investigate")
	send(t, s, loop.ID, inv, orch, "", "route_findings")
	send(t, s, loop.ID, orch, review, "", "review_findings")
	send(t, s, loop.ID, review, orch, "", "plan")
	send(t, s, loop.ID, orch, impl, "", "implement")

	for round := 1; round <= 2; round++ {
		send(t, s, loop.ID, impl, orch, "", "route_work")
		send(t, s, loop.ID, orch, review, "", "review_work")

		// A review that states no verdict cannot pick an edge, and is told
		// which verdicts this step decides on.
		v := violation(t, mustErr(s.PostMail(ctx, review, orch, MailPost{Body: "it depends"})))
		if !strings.Contains(v.Reason, OutcomeClean) || !strings.Contains(v.Reason, OutcomeChanges) {
			t.Fatalf("the refusal must name the verdicts the step decides on: %+v", v)
		}

		send(t, s, loop.ID, review, orch, OutcomeChanges, "rework")
		send(t, s, loop.ID, orch, impl, "", "implement")

		l, _ := s.GetLoop(ctx, loop.ID)
		if l.Round != round {
			t.Fatalf("after %d trip(s) round the cycle the round is %d, want %d", round, l.Round, round)
		}
	}

	send(t, s, loop.ID, impl, orch, "", "route_work")
	send(t, s, loop.ID, orch, review, "", "review_work")
	send(t, s, loop.ID, review, orch, OutcomeClean, "conclude")

	done, _ := s.GetLoop(ctx, loop.ID)
	if done.PlayStatus != PlayDone {
		t.Fatalf("clean must end the play, got %q", done.PlayStatus)
	}
	if done.Round != 2 {
		t.Fatalf("the exit must not count as another round, got %d", done.Round)
	}
	verdicts, err := s.ListLoopMessages(ctx, loop.ID, 0)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	clean := 0
	for _, m := range verdicts {
		if m.Outcome == OutcomeClean && m.StepID == "review_work" {
			clean++
		}
	}
	if clean != 1 {
		t.Fatalf("the transcript must record exactly one clean verdict at review_work, got %d", clean)
	}
}

// Sweep: one role, several members, and a step that physically cannot advance
// while any of them is still out.
func TestSweepFansOutAndWaitsForEveryMember(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoopWithRoles(t, s, "sweep", []string{
		RoleOrchestrator, RoleEngineer, "INVESTIGATION#1", "INVESTIGATION#2", "INVESTIGATION#3", "REVIEW",
	})
	orch, review := roles[RoleOrchestrator], roles["REVIEW"]
	sweepers := []*LoopMember{roles["INVESTIGATION#1"], roles["INVESTIGATION#2"], roles["INVESTIGATION#3"]}

	started := mustStartPlay(t, s, loop.ID, "sweep")
	if strings.Join(started.StepAwaiting, ",") != "INVESTIGATION#1,INVESTIGATION#2,INVESTIGATION#3" {
		t.Fatalf("the fan-out step must await every member, got %v", started.StepAwaiting)
	}

	// Briefing two of three does not advance, and the third cannot be
	// skipped by briefing one twice.
	send(t, s, loop.ID, orch, sweepers[0], "", "brief")
	send(t, s, loop.ID, orch, sweepers[1], "", "brief")
	v := violation(t, mustErr(s.PostMail(ctx, orch, sweepers[0], MailPost{Body: "again"})))
	if !strings.Contains(v.Reason, "exactly once") {
		t.Fatalf("briefing the same member twice must be refused: %+v", v)
	}
	if _, err := s.PostMail(ctx, orch, review, MailPost{Body: "early"}); err == nil {
		t.Fatal("the orchestrator must not leave the fan-out early")
	}
	send(t, s, loop.ID, orch, sweepers[2], "", "sweep")

	// Now the reports. Two of three still does not advance.
	send(t, s, loop.ID, sweepers[0], orch, "", "sweep")
	send(t, s, loop.ID, sweepers[2], orch, "", "sweep")
	mid, _ := s.GetLoop(ctx, loop.ID)
	if strings.Join(mid.StepAwaiting, ",") != "INVESTIGATION#2" {
		t.Fatalf("the step must still be waiting for the missing member, got %v", mid.StepAwaiting)
	}
	v = violation(t, mustErr(s.PostMail(ctx, sweepers[0], orch, MailPost{Body: "twice"})))
	if !strings.Contains(v.Reason, "not this member's turn") {
		t.Fatalf("a member that already reported must be refused: %+v", v)
	}
	send(t, s, loop.ID, sweepers[1], orch, "", "collate")

	send(t, s, loop.ID, orch, review, "", "review")
	send(t, s, loop.ID, review, orch, "", "conclude")
}

// Concurrent reports into a fan-in step: every arrival is recorded and the
// step advances exactly once.
func TestConcurrentFanInReportsLoseNoArrival(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	const n = 6
	roles := []string{RoleOrchestrator, RoleEngineer, "REVIEW"}
	for i := 1; i <= n; i++ {
		roles = append(roles, fmt.Sprintf("INVESTIGATION#%d", i))
	}
	loop, members := mustLoopWithRoles(t, s, "concurrent sweep", roles)
	orch := members[RoleOrchestrator]
	mustStartPlay(t, s, loop.ID, "sweep")

	sweepers := make([]*LoopMember, 0, n)
	for i := 1; i <= n; i++ {
		sweepers = append(sweepers, members[fmt.Sprintf("INVESTIGATION#%d", i)])
	}
	for _, m := range sweepers {
		if _, err := s.PostMail(ctx, orch, m, MailPost{Body: "your slice"}); err != nil {
			t.Fatalf("brief: %v", err)
		}
	}
	at, _ := s.GetLoop(ctx, loop.ID)
	if at.StepID != "sweep" {
		t.Fatalf("briefing everyone must advance to the sweep, got %s", at.StepID)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i, m := range sweepers {
		wg.Add(1)
		go func(i int, m *LoopMember) {
			defer wg.Done()
			<-start
			_, errs[i] = s.PostMail(ctx, m, orch, MailPost{Body: "slice report"})
		}(i, m)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("sweeper %d was refused: %v", i+1, err)
		}
	}

	after, _ := s.GetLoop(ctx, loop.ID)
	if after.StepID != "collate" {
		t.Fatalf("every report landed, so the step must have advanced, got %s awaiting %v",
			after.StepID, after.StepAwaiting)
	}
	if strings.Join(after.StepAwaiting, ",") != RoleOrchestrator {
		t.Fatalf("collate awaits the orchestrator, got %v", after.StepAwaiting)
	}
	reports, err := s.ListLoopMessages(ctx, loop.ID, 0)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	seen := map[string]int{}
	engineBriefs := 0
	for _, m := range reports {
		if m.StepID != "sweep" {
			continue
		}
		// The engine queues the step's brief to every member of the family
		// when the step opens; those are deliveries, not reports.
		if m.SenderRole == RoleEngine {
			engineBriefs++
			continue
		}
		seen[m.SenderRole]++
	}
	if engineBriefs != n {
		t.Fatalf("the sweep step must brief all %d members, got %d", n, engineBriefs)
	}
	if len(seen) != n {
		t.Fatalf("the sweep step recorded %d reporters, want %d", len(seen), n)
	}
	for role, c := range seen {
		if c != 1 {
			t.Fatalf("%s reported %d times", role, c)
		}
	}
}

// The engineer is a participant, not a step. Narrating to the engineer, and
// the engineer typing back, must never be blocked by a play and must never
// move it.
func TestEngineerIsExemptFromStepEnforcement(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "engineer")
	orch, inv, engineer := roles[RoleOrchestrator], roles["INVESTIGATION"], roles[RoleEngineer]
	mustStartPlay(t, s, loop.ID, "recon")
	send(t, s, loop.ID, orch, inv, "", "investigate")

	before, _ := s.GetLoop(ctx, loop.ID)
	if _, err := s.PostMail(ctx, orch, engineer, MailPost{Body: "two sentences of progress"}); err != nil {
		t.Fatalf("the orchestrator must always be able to narrate to the engineer: %v", err)
	}
	if _, err := s.PostMail(ctx, engineer, orch, MailPost{Body: "change of plan"}); err != nil {
		t.Fatalf("the engineer must always be able to type: %v", err)
	}
	if _, err := s.PostMail(ctx, engineer, inv, MailPost{Body: "straight to you"}); err != nil {
		t.Fatalf("the engineer may address anyone: %v", err)
	}
	after, _ := s.GetLoop(ctx, loop.ID)
	if after.StepID != before.StepID || strings.Join(after.StepAwaiting, ",") != strings.Join(before.StepAwaiting, ",") {
		t.Fatalf("engineer traffic must not move the play: %s%v -> %s%v",
			before.StepID, before.StepAwaiting, after.StepID, after.StepAwaiting)
	}
	// A worker narrating to the engineer is also exempt from the step, and
	// also does not move the play.
	if _, err := s.PostMail(ctx, inv, engineer, MailPost{Body: "still digging"}); err != nil {
		t.Fatalf("a worker must be able to narrate to the engineer: %v", err)
	}
	stillThere, _ := s.GetLoop(ctx, loop.ID)
	if stillThere.StepID != before.StepID {
		t.Fatal("narration must not move the play")
	}
}

// A finished play routes nothing more between members. Letting mail through
// unenforced once the play was over is how a loop kept trading messages, and
// spending, after it had said it was done. The orchestrator can still report
// to the engineer, and a loop that never started a play stays unenforced.
func TestRouteRefusedAfterPlayDone(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "after")
	orch, inv, review, engineer := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["REVIEW"], roles[RoleEngineer]
	mustStartPlay(t, s, loop.ID, "recon")
	send(t, s, loop.ID, orch, inv, "", "investigate")
	send(t, s, loop.ID, inv, orch, "", "conclude")

	done, _ := s.GetLoop(ctx, loop.ID)
	if done.PlayStatus != PlayDone || done.PlayEndedAt == 0 {
		t.Fatalf("the play must be done and say when: %+v", done)
	}
	_, err := s.PostMail(ctx, orch, review, MailPost{Body: "one more thing"})
	var violation *StepViolation
	if !errors.As(err, &violation) {
		t.Fatalf("orchestrator to a worker after the play is done must be refused, got %v", err)
	}
	if _, err := s.PostMail(ctx, review, orch, MailPost{Body: "any time"}); !errors.As(err, &violation) {
		t.Fatalf("a worker to the orchestrator after the play is done must be refused, got %v", err)
	}
	if _, err := s.PostMail(ctx, orch, engineer, MailPost{Body: "here is what we found"}); err != nil {
		t.Fatalf("the orchestrator must still be able to report to the engineer: %v", err)
	}
	// And a loop that never started a play is unenforced from the start.
	_, plainRoles := mustLoop(t, s, "no play")
	if _, err := s.PostMail(ctx, plainRoles[RoleOrchestrator], plainRoles["REVIEW"], MailPost{Body: "free"}); err != nil {
		t.Fatalf("a loop with no play must be unenforced: %v", err)
	}
}

// A play the loop cannot staff is refused when it is started, not discovered
// three steps in.
func TestStartPlayRefusesAPlayTheLoopCannotStaff(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, _ := mustLoopWithRoles(t, s, "thin", []string{RoleOrchestrator, RoleEngineer, "INVESTIGATION"})

	if _, err := s.StartPlay(ctx, loop.ID, "deep"); err == nil {
		t.Fatal("deep without an implementer or reviewer must be refused at start")
	} else if !errors.Is(err, ErrPlayInvalid) {
		t.Fatalf("refusal must be a play error: %v", err)
	}
	if _, err := s.StartPlay(ctx, loop.ID, "recon"); err != nil {
		t.Fatalf("recon only needs an investigator: %v", err)
	}
	if _, err := s.StartPlay(ctx, loop.ID, "nonsense"); err == nil {
		t.Fatal("an unknown play must be refused")
	}

	// A dismissed member does not staff a role.
	if _, err := s.SetLoopMemberStatus(ctx, loop.ID, "INVESTIGATION", LoopMemberDismissed); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if _, err := s.StartPlay(ctx, loop.ID, "recon"); err == nil {
		t.Fatal("a dismissed member must not staff a play")
	}

	// An ended loop takes no play at all.
	ended, _ := mustLoop(t, s, "ended")
	if _, err := s.EndLoop(ctx, ended.ID, LoopComplete, "done", false); err != nil {
		t.Fatalf("end: %v", err)
	}
	if _, err := s.StartPlay(ctx, ended.ID, "recon"); err == nil {
		t.Fatal("an ended loop must not start a play")
	}
}

// Forwarding is a step transition like any other: it carries the verdict and
// records the step.
func TestForwardingSatisfiesAStep(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "forward in a play")
	orch, inv, review := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["REVIEW"]
	mustStartPlay(t, s, loop.ID, "audit")

	send(t, s, loop.ID, orch, inv, "", "investigate")
	report := "F1 finding\n" + strings.Repeat("evidence\n", 200)
	if _, err := s.PostMail(ctx, inv, orch, MailPost{Body: report}); err != nil {
		t.Fatalf("report: %v", err)
	}
	forwarded, err := s.ForwardMail(ctx, orch, review, MailForward{OriginalID: mustFirstFrom(t, s, loop.ID, "INVESTIGATION")})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if forwarded.Body != report {
		t.Fatal("the forward must still be byte identical inside a play")
	}
	if forwarded.StepID != "route" {
		t.Fatalf("the forward must record the step it satisfied, got %q", forwarded.StepID)
	}
	at, _ := s.GetLoop(ctx, loop.ID)
	if at.StepID != "review" {
		t.Fatalf("the forward must advance the play, got %s", at.StepID)
	}
}

func mustErr(_ *LoopMessage, err error) error { return err }

func mustFirstFrom(t *testing.T, s *Store, loopID, role string) string {
	t.Helper()
	msgs, err := s.ListLoopMessages(context.Background(), loopID, 0)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	for _, m := range msgs {
		if m.SenderRole == role {
			return m.ID
		}
	}
	t.Fatalf("no message from %s", role)
	return ""
}
