// play_test.go — definition-time contracts. A play that could route worker to
// worker, end anywhere but at the orchestrator, strand a step, dead-end, or
// leave a transition ambiguous must be impossible to define, so these tests
// build broken graphs on purpose and require the refusal.
package store

import (
	"errors"
	"strings"
	"testing"
)

// step builds a step with the fields these tests are not about already
// filled, so each case isolates the one rule it is testing.
func step(id, actor string, next ...PlayEdge) PlayStep {
	return PlayStep{ID: id, Actor: actor, Note: "note", Brief: "brief", Next: next}
}

// The shipped catalogue is only safe because LookupPlay never returns an
// unvalidated graph. This is the test that makes that true.
func TestEveryShippedPlayValidates(t *testing.T) {
	want := []string{"recon", "audit", "patch", "build", "deep", "sweep", "second-opinion"}
	if got := PlayNames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("shipped plays = %v, want %v", got, want)
	}
	for _, name := range want {
		p, ok := LookupPlay(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("shipped play %s does not validate: %v", name, err)
		}
		if p.Title == "" {
			t.Fatalf("%s has no title", name)
		}
		for i := range p.Steps {
			if p.Steps[i].Note == "" {
				t.Fatalf("%s step %s has no note", name, p.Steps[i].ID)
			}
			// Asserted directly as well as in Validate: a shipped step with
			// no brief is a silent gap in the prompt layer, and it must be
			// caught even if the validation rule is the thing that broke.
			if p.Steps[i].Brief == "" {
				t.Fatalf("%s step %s ships with no brief; whoever holds it would be told nothing", name, p.Steps[i].ID)
			}
		}
	}
	if _, ok := LookupPlay("nonsense"); ok {
		t.Fatal("an unknown play must not resolve")
	}
}

// The worker order each play actually walks, which is the shape the brief
// names. The orchestrator hops between them are the routing rule showing
// through, not decoration.
func TestShippedPlaysWalkTheIntendedRoles(t *testing.T) {
	want := map[string][]string{
		"recon":          {"ORCHESTRATOR", "INVESTIGATION", "ORCHESTRATOR"},
		"audit":          {"ORCHESTRATOR", "INVESTIGATION", "ORCHESTRATOR", "REVIEW", "ORCHESTRATOR"},
		"patch":          {"ORCHESTRATOR", "IMPLEMENTATION", "ORCHESTRATOR"},
		"build":          {"ORCHESTRATOR", "IMPLEMENTATION", "ORCHESTRATOR", "REVIEW", "ORCHESTRATOR"},
		"sweep":          {"ORCHESTRATOR", "INVESTIGATION", "ORCHESTRATOR", "REVIEW", "ORCHESTRATOR"},
		"second-opinion": {"ORCHESTRATOR", "REVIEW", "ORCHESTRATOR"},
	}
	for name, roles := range want {
		p, _ := LookupPlay(name)
		got := []string{}
		for id := p.Entry; id != ""; {
			s := p.Step(id)
			got = append(got, s.Actor)
			if s.Terminal() {
				break
			}
			id = s.Next[0].To
		}
		if strings.Join(got, ",") != strings.Join(roles, ",") {
			t.Fatalf("%s walks %v, want %v", name, got, roles)
		}
	}
}

// Deep's whole value is the cycle: implementation goes back for another round
// until review reports clean. A flat list cannot say that.
func TestDeepExpressesACycleWithAnExplicitExit(t *testing.T) {
	p, _ := LookupPlay("deep")
	review := p.Step("review_work")
	if review == nil || len(review.Next) != 2 {
		t.Fatalf("deep must branch after the work review, got %+v", review)
	}
	var exit, back *PlayEdge
	for i := range review.Next {
		switch review.Next[i].When {
		case OutcomeClean:
			exit = &review.Next[i]
		case OutcomeChanges:
			back = &review.Next[i]
		}
	}
	if exit == nil || back == nil {
		t.Fatal("the cycle needs both an exit guarded on clean and a way back guarded on changes")
	}
	if !p.Step(exit.To).Terminal() {
		t.Fatalf("the clean edge must end the play, it goes to %s", exit.To)
	}
	rework := p.Step(back.To)
	if rework == nil || !isOrchestratorRole(rework.Actor) {
		t.Fatal("the way back must pass through the orchestrator")
	}
	if len(rework.Next) != 1 || rework.Next[0].To != "implement" {
		t.Fatalf("rework must lead back to implement, got %+v", rework.Next)
	}
	if !rework.Next[0].Loop {
		t.Fatal("the loop-back edge must be marked Loop so a round can be counted")
	}
	// implement is reachable from the review, which is what makes it a cycle
	// rather than a fork.
	if !p.reachableFrom("review_work")["implement"] {
		t.Fatal("implement must be reachable from review_work")
	}
}

func TestPlayMustTerminateAtTheOrchestrator(t *testing.T) {
	bad := &Play{Name: "ends-at-review", Entry: "brief", Steps: []PlayStep{
		step("brief", RoleOrchestrator, PlayEdge{To: "review"}),
		step("review", "REVIEW"),
	}}
	err := bad.Validate()
	if err == nil {
		t.Fatal("a play that ends at a worker must be refused")
	}
	if !errors.Is(err, ErrPlayInvalid) || !strings.Contains(err.Error(), "terminate at ORCHESTRATOR") {
		t.Fatalf("refusal must say why: %v", err)
	}
}

func TestPlayCannotContainAWorkerToWorkerEdge(t *testing.T) {
	bad := &Play{Name: "worker-to-worker", Entry: "brief", Steps: []PlayStep{
		step("brief", RoleOrchestrator, PlayEdge{To: "investigate"}),
		step("investigate", "INVESTIGATION", PlayEdge{To: "review"}),
		step("review", "REVIEW", PlayEdge{To: "conclude"}),
		step("conclude", RoleOrchestrator),
	}}
	err := bad.Validate()
	if err == nil {
		t.Fatal("INVESTIGATION -> REVIEW must be refused at definition time")
	}
	if !errors.Is(err, ErrPlayInvalid) || !strings.Contains(err.Error(), "workers may only message") {
		t.Fatalf("refusal must name the rule: %v", err)
	}
	// The same play routed through the orchestrator is fine, which is the
	// point: the rule shapes the graph rather than banning the intent.
	good := &Play{Name: "routed", Entry: "brief", Steps: []PlayStep{
		step("brief", RoleOrchestrator, PlayEdge{To: "investigate"}),
		step("investigate", "INVESTIGATION", PlayEdge{To: "route"}),
		step("route", RoleOrchestrator, PlayEdge{To: "review"}),
		step("review", "REVIEW", PlayEdge{To: "conclude"}),
		step("conclude", RoleOrchestrator),
	}}
	if err := good.Validate(); err != nil {
		t.Fatalf("the routed equivalent must validate: %v", err)
	}
}

func TestPlayCannotStrandOrDeadEndAStep(t *testing.T) {
	unreachable := &Play{Name: "stranded", Entry: "brief", Steps: []PlayStep{
		step("brief", RoleOrchestrator, PlayEdge{To: "conclude"}),
		step("conclude", RoleOrchestrator),
		step("orphan", "INVESTIGATION", PlayEdge{To: "conclude"}),
	}}
	err := unreachable.Validate()
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("an unreachable step must be refused: %v", err)
	}

	deadEnd := &Play{Name: "dead-end", Entry: "brief", Steps: []PlayStep{
		step("brief", RoleOrchestrator, PlayEdge{To: "spin"}),
		step("spin", "INVESTIGATION", PlayEdge{To: "spin_back"}),
		step("spin_back", RoleOrchestrator, PlayEdge{To: "spin"}),
	}}
	err = deadEnd.Validate()
	if err == nil || !strings.Contains(err.Error(), "never reach a terminal step") {
		t.Fatalf("a cycle with no exit must be refused: %v", err)
	}
}

func TestPlayTransitionsMustBeUnambiguous(t *testing.T) {
	cases := []struct {
		name string
		play *Play
		want string
	}{
		{"two defaults", &Play{Name: "x", Entry: "a", Steps: []PlayStep{
			step("a", RoleOrchestrator, PlayEdge{To: "b"}, PlayEdge{To: "c"}),
			step("b", "REVIEW", PlayEdge{To: "c"}),
			step("c", RoleOrchestrator),
		}}, "ambiguous"},
		{"duplicate guards", &Play{Name: "x", Entry: "a", Steps: []PlayStep{
			step("a", RoleOrchestrator, PlayEdge{To: "b", When: OutcomeClean}, PlayEdge{To: "c", When: OutcomeClean}),
			step("b", "REVIEW", PlayEdge{To: "c"}),
			step("c", RoleOrchestrator),
		}}, "guarded on"},
		{"self edge", &Play{Name: "x", Entry: "a", Steps: []PlayStep{
			step("a", RoleOrchestrator, PlayEdge{To: "a"}),
		}}, "points at itself"},
		{"unknown target", &Play{Name: "x", Entry: "a", Steps: []PlayStep{
			step("a", RoleOrchestrator, PlayEdge{To: "nowhere"}),
		}}, "unknown step"},
		{"missing entry", &Play{Name: "x", Entry: "ghost", Steps: []PlayStep{
			step("a", RoleOrchestrator),
		}}, "entry step"},
		{"fan-out to a step that is not fan-in", &Play{Name: "x", Entry: "a", Steps: []PlayStep{
			step("a", RoleOrchestrator, PlayEdge{To: "b", FanOut: true}),
			step("b", "INVESTIGATION", PlayEdge{To: "c"}),
			step("c", RoleOrchestrator),
		}}, "not fan-in"},
		{"fan-out that also branches", &Play{Name: "x", Entry: "a", Steps: []PlayStep{
			{ID: "a", Actor: RoleOrchestrator, Note: "note", Brief: "brief", Next: []PlayEdge{{To: "b", FanOut: true}, {To: "c", When: OutcomeClean}}},
			{ID: "b", Actor: "INVESTIGATION", FanIn: true, Note: "note", Brief: "brief", Next: []PlayEdge{{To: "c"}}},
			step("c", RoleOrchestrator),
		}}, "fans out and branches"},
		{"guarded fan-in", &Play{Name: "x", Entry: "a", Steps: []PlayStep{
			{ID: "a", Actor: RoleOrchestrator, Note: "note", Brief: "brief", Next: []PlayEdge{{To: "b", FanOut: true}}},
			{ID: "b", Actor: "INVESTIGATION", FanIn: true, Note: "note", Brief: "brief", Next: []PlayEdge{{To: "c", When: OutcomeClean}}},
			step("c", RoleOrchestrator),
		}}, "no one member's outcome can decide"},
	}
	for _, tc := range cases {
		err := tc.play.Validate()
		if err == nil {
			t.Fatalf("%s must be refused", tc.name)
		}
		if !errors.Is(err, ErrPlayInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s refused with the wrong reason: %v", tc.name, err)
		}
	}
}

func TestPlayNeedsAMemberForEveryRole(t *testing.T) {
	deep, _ := LookupPlay("deep")
	short := []string{RoleOrchestrator, RoleEngineer, "INVESTIGATION", "REVIEW"}
	err := deep.ValidateRoster(short)
	if err == nil || !strings.Contains(err.Error(), "IMPLEMENTATION") {
		t.Fatalf("deep without an implementer must be refused by name: %v", err)
	}
	full := append(append([]string{}, short...), "IMPLEMENTATION")
	if err := deep.ValidateRoster(full); err != nil {
		t.Fatalf("a full roster must be accepted: %v", err)
	}
	// A fan-out family is staffed by its instances, not by the bare name.
	sweep, _ := LookupPlay("sweep")
	if err := sweep.ValidateRoster([]string{RoleOrchestrator, "INVESTIGATION#1", "INVESTIGATION#2", "REVIEW"}); err != nil {
		t.Fatalf("suffixed members must staff their family: %v", err)
	}
	if err := sweep.ValidateRoster([]string{RoleOrchestrator, "REVIEW"}); err == nil {
		t.Fatal("a sweep with no investigators must be refused")
	}
}

func TestBaseRoleAndFamilyMembers(t *testing.T) {
	for role, want := range map[string]string{
		"INVESTIGATION#2": "INVESTIGATION",
		"investigation#2": "INVESTIGATION",
		"REVIEW":          "REVIEW",
		"  review  ":      "REVIEW",
		"#leading":        "#LEADING",
	} {
		if got := BaseRole(role); got != want {
			t.Fatalf("BaseRole(%q) = %q, want %q", role, got, want)
		}
	}
	roster := []string{"ORCHESTRATOR", "INVESTIGATION#2", "REVIEW", "INVESTIGATION#1"}
	got := FamilyMembers(roster, "INVESTIGATION")
	if strings.Join(got, ",") != "INVESTIGATION#1,INVESTIGATION#2" {
		t.Fatalf("family = %v, want the two investigators sorted", got)
	}
	if got := FamilyMembers(roster, "IMPLEMENTATION"); strings.Join(got, ",") != "IMPLEMENTATION" {
		t.Fatalf("an unstaffed family must still name itself, got %v", got)
	}
}

// A step with nothing to say to whoever holds it is a gap in the prompt
// layer, so it is refused at definition time as well as asserted on the
// shipped catalogue.
func TestPlayCannotDefineAStepWithNoBrief(t *testing.T) {
	bad := &Play{Name: "briefless", Entry: "a", Steps: []PlayStep{
		{ID: "a", Actor: RoleOrchestrator, Note: "note", Next: []PlayEdge{{To: "b"}}},
		step("b", "REVIEW", PlayEdge{To: "c"}),
		step("c", RoleOrchestrator),
	}}
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "has no brief") {
		t.Fatalf("a step with no brief must be refused: %v", err)
	}
	if !errors.Is(err, ErrPlayInvalid) {
		t.Fatalf("refusal must be a play error: %v", err)
	}
	bad.Steps[0].Brief = "say something"
	if err := bad.Validate(); err != nil {
		t.Fatalf("with a brief it must validate: %v", err)
	}
}
