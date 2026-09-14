// prompt_test.go — the regression gate for the prompt layer. The required rule
// names below are written out HERE, deliberately not derived from the
// dictionary: a test that reads its expectations from the thing under test
// goes green when a rule is deleted, which is the exact loss this round exists
// to make impossible.
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var requiredSharedRules = []string{
	"stay-reachable",
	"idle-is-not-stop",
	"notes-before-reporting",
	"extend-if-long",
	"engineer-drives-git",
	"trust-no-summary",
	"no-scope-expansion",
	"no-deploys",
	"house-style",
	"reporting-contract",
}

var requiredWorkerRules = []string{
	"report-or-you-are-not-done",
	"orchestrator-is-your-only-address",
	"notes-for-a-stranger",
}

// A session with no loop uses these and only these. Eleven of the seventeen
// describe machinery a solo session does not have, and a rule about an inbox
// that is not there sends the agent looking for one.
var requiredSoloRules = []string{
	"reporting-contract-solo",
}

var requiredOrchestratorRules = []string{
	"forward-verbatim",
	"never-truncate",
	"keep-loop-state-current",
	"retire-sparingly",
	"never-invent-authorisation",
}

// Every play crossed with every role in it. Every applicable rule must be
// present by name AND in full, and no role-specific rule may leak across.
func TestPromptMatrixCarriesEveryApplicableRule(t *testing.T) {
	checked := 0
	for _, name := range PlayNames() {
		play, ok := LookupPlay(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		for _, role := range PlayRoles(play) {
			prompt, err := DefaultCatalogue().Prompt(play, role)
			if err != nil {
				t.Fatalf("%s/%s: %v", name, role, err)
			}
			checked++

			for _, want := range requiredSharedRules {
				requireRule(t, prompt, name, role, want)
			}
			if isOrchestratorRole(role) {
				for _, want := range requiredOrchestratorRules {
					requireRule(t, prompt, name, role, want)
				}
				for _, leaked := range requiredWorkerRules {
					refuseRule(t, prompt, name, role, leaked)
				}
			} else {
				for _, want := range requiredWorkerRules {
					requireRule(t, prompt, name, role, want)
				}
				for _, leaked := range requiredOrchestratorRules {
					refuseRule(t, prompt, name, role, leaked)
				}
			}
		}
	}
	if checked < 14 {
		t.Fatalf("the matrix only checked %d play/role pairs, which is too few to be the matrix", checked)
	}
}

func requireRule(t *testing.T, prompt, play, role, name string) {
	t.Helper()
	rule, ok := LookupRule(name)
	if !ok {
		t.Fatalf("rule %q is gone from the dictionary, so the %s prompt for %s silently lost it", name, play, role)
	}
	if !strings.Contains(prompt, "["+rule.Name+"]") {
		t.Fatalf("%s/%s prompt does not name rule %q", play, role, name)
	}
	if !strings.Contains(prompt, rule.Text) {
		t.Fatalf("%s/%s prompt names rule %q but does not carry its full text", play, role, name)
	}
}

func refuseRule(t *testing.T, prompt, play, role, name string) {
	t.Helper()
	rule, ok := LookupRule(name)
	if !ok {
		t.Fatalf("rule %q is gone from the dictionary", name)
	}
	if strings.Contains(prompt, rule.Text) {
		t.Fatalf("%s/%s prompt carries %q, which belongs to the other side", play, role, name)
	}
}

// The gate above only holds if its list and the dictionary agree. A rule added
// without being added here would never be checked by anything.
func TestRuleDictionaryMatchesTheRequiredSet(t *testing.T) {
	want := map[string]bool{}
	for _, group := range [][]string{
		requiredSharedRules, requiredWorkerRules, requiredOrchestratorRules, requiredSoloRules,
	} {
		for _, n := range group {
			want[n] = true
		}
	}
	have := map[string]bool{}
	for _, n := range RuleNames() {
		have[n] = true
	}
	for n := range want {
		if !have[n] {
			t.Fatalf("rule %q is required by the prompt gate but missing from the dictionary", n)
		}
	}
	for n := range have {
		if !want[n] {
			t.Fatalf("rule %q is in the dictionary but no prompt gate requires it; add it to the gate or delete it", n)
		}
	}
	for _, group := range []struct {
		name  string
		got   []Rule
		wants []string
	}{
		{RulesShared, RuleGroup(RulesShared), requiredSharedRules},
		{RulesWorker, RuleGroup(RulesWorker), requiredWorkerRules},
		{RulesOrchestrator, RuleGroup(RulesOrchestrator), requiredOrchestratorRules},
	} {
		if len(group.got) != len(group.wants) {
			t.Fatalf("group %s has %d rules, the gate requires %d", group.name, len(group.got), len(group.wants))
		}
		for _, r := range group.got {
			if r.Text == "" {
				t.Fatalf("rule %q has no text", r.Name)
			}
			if strings.Contains(r.Text, "—") {
				t.Fatalf("rule %q contains an em-dash", r.Name)
			}
		}
	}
}

// A prompt is a function of play and role and nothing else. This is the bug it
// prevents: a member adopted into a successor loop with a different task keeps
// its prompt, so a prompt naming a task would be wrong from that moment on.
func TestPromptSurvivesAdoptionIntoADifferentLoop(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	first := &Loop{Task: "Fix the flaky guest export"}
	tokens, err := s.CreateLoop(ctx, first, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	play, _ := LookupPlay("deep")
	before, err := DefaultCatalogue().Prompt(play, "INVESTIGATION")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}

	if _, err := s.EndLoop(ctx, first.ID, LoopComplete, "round one done", false); err != nil {
		t.Fatalf("end: %v", err)
	}
	second := &Loop{Task: "Rewrite the announcement sender", ParentLoopID: first.ID}
	if _, err := s.CreateLoop(ctx, second, nil); err != nil {
		t.Fatalf("create successor: %v", err)
	}
	if _, err := s.AdoptLoopMembers(ctx, first.ID, second.ID); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	member, loop, err := s.LoopMemberByToken(ctx, tokens["INVESTIGATION"])
	if err != nil || member == nil || loop.ID != second.ID {
		t.Fatalf("adoption: %v %+v", err, member)
	}

	after, err := DefaultCatalogue().Prompt(play, member.Role)
	if err != nil {
		t.Fatalf("prompt after adoption: %v", err)
	}
	if after != before {
		t.Fatal("the prompt changed across adoption; it is supposed to be a function of play and role alone")
	}
	for _, forbidden := range []string{
		first.ID, second.ID,
		"Fix the flaky guest export", "Rewrite the announcement sender",
	} {
		if strings.Contains(after, forbidden) {
			t.Fatalf("the prompt names %q; a prompt that names a task or a loop goes stale on adoption", forbidden)
		}
	}
	if !strings.Contains(after, "read your loop, its task") {
		t.Fatal("the prompt must tell the agent to read its loop and task at runtime instead")
	}
}

// No prompt in the catalogue may name a task or a loop id, and none may be
// generated for a role the play gives no work to.
func TestPromptNamesNothingItCannotKnow(t *testing.T) {
	for _, name := range PlayNames() {
		play, _ := LookupPlay(name)
		for _, role := range PlayRoles(play) {
			prompt, err := DefaultCatalogue().Prompt(play, role)
			if err != nil {
				t.Fatalf("%s/%s: %v", name, role, err)
			}
			for _, forbidden := range []string{"loop_", "msg_", "task:", "Task:"} {
				if strings.Contains(prompt, forbidden) {
					t.Fatalf("%s/%s prompt contains %q", name, role, forbidden)
				}
			}
		}
		if _, err := DefaultCatalogue().Prompt(play, RoleEngineer); !errors.Is(err, ErrRoleNotInPlay) {
			t.Fatalf("%s: the engineer holds no step and must get no play prompt: %v", name, err)
		}
		if _, err := DefaultCatalogue().Prompt(play, "NOBODY"); !errors.Is(err, ErrRoleNotInPlay) {
			t.Fatalf("%s: an unknown role must be refused: %v", name, err)
		}
	}
	if _, err := DefaultCatalogue().Prompt(nil, "REVIEW"); err == nil {
		t.Fatal("a nil play must be refused")
	}
}

// Deep is the hard case: REVIEW holds two steps doing genuinely different
// jobs. The invariant lives in the prompt, the job of each step lives in that
// step's brief, so neither has to hedge.
func TestDeepReviewGetsOneUnhedgedPromptAndTwoDifferentBriefs(t *testing.T) {
	play, _ := LookupPlay("deep")
	held := StepsHeldBy(play, "REVIEW")
	if len(held) != 2 {
		t.Fatalf("REVIEW holds %d steps in deep, want 2", len(held))
	}
	findings, work := held[0], held[1]
	if findings.ID != "review_findings" || work.ID != "review_work" {
		t.Fatalf("unexpected steps: %s, %s", findings.ID, work.ID)
	}
	if findings.Brief == work.Brief {
		t.Fatal("the two REVIEW steps must not share a brief; that is the hedge this is designed to avoid")
	}
	if !strings.Contains(findings.Brief, "INVESTIGATION") || !strings.Contains(findings.Brief, "do not review code") {
		t.Fatalf("the findings review brief must say it is not a diff: %q", findings.Brief)
	}
	if !strings.Contains(work.Brief, "DIFF") || !strings.Contains(work.Brief, "verdict is required") {
		t.Fatalf("the work review brief must demand a verdict on a diff: %q", work.Brief)
	}

	prompt, err := DefaultCatalogue().Prompt(play, "REVIEW")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	// The prompt tells the reviewer it holds two different jobs and to read
	// each step's brief, and carries NEITHER brief itself.
	if !strings.Contains(prompt, "review_findings") || !strings.Contains(prompt, "review_work") {
		t.Fatal("the prompt must name both steps the role holds")
	}
	if !strings.Contains(prompt, "DIFFERENT jobs") {
		t.Fatal("the prompt must say the two steps are different jobs")
	}
	if strings.Contains(prompt, findings.Brief) || strings.Contains(prompt, work.Brief) {
		t.Fatal("a step brief must not be baked into the prompt; that is the hedging we are avoiding")
	}
	// A role holding one step is told so, without the two-step warning.
	single, err := DefaultCatalogue().Prompt(play, "IMPLEMENTATION")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if strings.Contains(single, "DIFFERENT jobs") {
		t.Fatal("a role holding one step must not be warned about two")
	}
}

// The brief follows the step, which is what makes the prompt able to stay
// silent about it.
func TestCurrentStepBriefFollowsTheStep(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "briefs")
	orch, inv, review := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["REVIEW"]
	play, _ := LookupPlay("deep")
	mustStartPlay(t, s, loop.ID, "deep")

	seen := map[string]bool{}
	for _, hop := range []struct {
		from, to *LoopMember
		step     string
	}{
		{orch, inv, "investigate"},
		{inv, orch, "route_findings"},
		{orch, review, "review_findings"},
		{review, orch, "plan"},
	} {
		at, _ := s.GetLoop(ctx, loop.ID)
		brief, ok := CurrentStepBrief(at)
		if !ok || brief == "" {
			t.Fatalf("step %s has no brief", at.StepID)
		}
		if brief != play.Step(at.StepID).Brief {
			t.Fatalf("step %s brief does not match the play data", at.StepID)
		}
		seen[brief] = true
		send(t, s, loop.ID, hop.from, hop.to, "", hop.step)
	}
	if len(seen) != 4 {
		t.Fatalf("four steps produced %d distinct briefs", len(seen))
	}
	if _, ok := CurrentStepBrief(&Loop{}); ok {
		t.Fatal("a loop on no play has no step brief")
	}
}

// The cycle is bounded. When the cap is reached the loop-back is refused and
// the state says capped, instead of implement and review trading forever.
func TestRoundCapStopsTheCycle(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "capped")
	orch, inv, impl, review := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["IMPLEMENTATION"], roles["REVIEW"]
	play, _ := LookupPlay("deep")
	capped := play.EffectiveMaxRounds()
	if capped != 3 || !play.HasCycle() {
		t.Fatalf("deep must declare a cap of 3 and have a cycle, got %d %v", capped, play.HasCycle())
	}
	mustStartPlay(t, s, loop.ID, "deep")

	send(t, s, loop.ID, orch, inv, "", "investigate")
	send(t, s, loop.ID, inv, orch, "", "route_findings")
	send(t, s, loop.ID, orch, review, "", "review_findings")
	send(t, s, loop.ID, review, orch, "", "plan")
	send(t, s, loop.ID, orch, impl, "", "implement")

	for round := 1; round <= capped; round++ {
		send(t, s, loop.ID, impl, orch, "", "route_work")
		send(t, s, loop.ID, orch, review, "", "review_work")
		send(t, s, loop.ID, review, orch, OutcomeChanges, "rework")
		send(t, s, loop.ID, orch, impl, "", "implement")
		at, _ := s.GetLoop(ctx, loop.ID)
		if at.Round != round {
			t.Fatalf("round %d: counter is %d", round, at.Round)
		}
	}

	// One more round is refused at the loop-back.
	send(t, s, loop.ID, impl, orch, "", "route_work")
	send(t, s, loop.ID, orch, review, "", "review_work")
	send(t, s, loop.ID, review, orch, OutcomeChanges, "rework")

	before, _ := s.CountMailByStatus(ctx, loop.ID)
	_, err := s.PostMail(ctx, orch, impl, MailPost{Body: "one more round"})
	if !errors.Is(err, ErrRoundCapReached) {
		t.Fatalf("the loop-back past the cap must be refused: %v", err)
	}
	var capErr *RoundCapReached
	if !errors.As(err, &capErr) {
		t.Fatalf("the refusal must be renderable: %v", err)
	}
	if capErr.Round != capped || capErr.MaxRounds != capped || capErr.Play != "deep" {
		t.Fatalf("the refusal must carry the counts: %+v", capErr)
	}
	if !strings.Contains(capErr.Error(), "capped") {
		t.Fatalf("the rendered refusal must say the play is capped: %q", capErr.Error())
	}

	after, _ := s.GetLoop(ctx, loop.ID)
	if after.PlayStatus != PlayCapped {
		t.Fatalf("the loop must be left saying capped, got %q", after.PlayStatus)
	}
	if after.Round != capped {
		t.Fatalf("a refused round must not be counted, got %d", after.Round)
	}
	if after.StepID != "rework" {
		t.Fatalf("the loop must stand where it stopped, got %s", after.StepID)
	}
	afterCounts, _ := s.CountMailByStatus(ctx, loop.ID)
	if afterCounts[MailPending] != before[MailPending] {
		t.Fatal("the refused brief must not have been delivered to anyone")
	}
	// Capped stops enforcement so the orchestrator can escalate or end it.
	if after.PlayInProgress() {
		t.Fatal("a capped play is no longer in progress")
	}
	if _, err := s.PostMail(ctx, orch, roles[RoleEngineer], MailPost{Body: "we are capped"}); err != nil {
		t.Fatalf("a capped loop must still let the orchestrator escalate: %v", err)
	}
}

// There is exactly one orchestrator and one engineer, so neither can be
// fanned out. Closed at the play, at loop creation and at staffing.
func TestReservedRolesCannotBeFannedOut(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	for _, bad := range []string{"ORCHESTRATOR#2", "orchestrator#1", "ENGINEER#2"} {
		if err := ValidateRole(bad); !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("%s must be refused: %v", bad, err)
		}
	}
	for _, good := range []string{"INVESTIGATION#2", "REVIEW", RoleOrchestrator, RoleEngineer} {
		if err := ValidateRole(good); err != nil {
			t.Fatalf("%s must be allowed: %v", good, err)
		}
	}
	if err := ValidateRole("  "); err == nil {
		t.Fatal("an empty role must be refused")
	}

	if _, err := s.CreateLoop(ctx, &Loop{Task: "bad"}, []string{RoleOrchestrator, "ORCHESTRATOR#2"}); err == nil {
		t.Fatal("a loop must not be created with a second orchestrator")
	}
	loop, _ := mustLoop(t, s, "staffing")
	if _, _, err := s.AddLoopMember(ctx, loop.ID, MemberSpec{Role: "ORCHESTRATOR#2"}); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("staffing a second orchestrator must be refused: %v", err)
	}
	if _, _, err := s.AddLoopMember(ctx, loop.ID, MemberSpec{Role: "INVESTIGATION#2"}); err != nil {
		t.Fatalf("staffing another investigator must be allowed: %v", err)
	}

	bad := &Play{Name: "two-orchestrators", Entry: "a", Steps: []PlayStep{
		step("a", "ORCHESTRATOR#1", PlayEdge{To: "b"}),
		step("b", "REVIEW", PlayEdge{To: "c"}),
		step("c", RoleOrchestrator),
	}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("a play held by a suffixed orchestrator must be refused: %v", err)
	}
	fanned := &Play{Name: "fan-in-orchestrator", Entry: "a", Steps: []PlayStep{
		{ID: "a", Actor: RoleOrchestrator, Note: "n", Brief: "b", Next: []PlayEdge{{To: "c", FanOut: true}}},
		{ID: "c", Actor: RoleOrchestrator, FanIn: true, Note: "n", Brief: "b"},
	}}
	if err := fanned.Validate(); err == nil || !strings.Contains(err.Error(), "worker concept") {
		t.Fatalf("the orchestrator must not be a fan-in actor: %v", err)
	}
}
