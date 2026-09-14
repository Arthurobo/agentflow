package mailapi

import (
	"strings"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

// Two orchestration dead-ends, watched live. The orchestrator tried to WRITE
// loop state, got 405 from a path registered for GET only, and went looking
// for another way round. Then it asked the ENGINEER what the question was,
// holding a task it had already been given, while INVESTIGATION sat with an
// empty terminal because nobody had briefed it.
//
// Neither is fixable in code alone: the surface is right, the instructions
// were not. These pin the instructions.

func renderedOrchestratorPrompt(t *testing.T) string {
	t.Helper()
	h := newHarness(t, nil, Config{MaxWait: 2 * time.Second})
	cat := store.DefaultCatalogue()
	play, ok := cat.LookupPlay("audit")
	if !ok {
		t.Fatal("the default catalogue must have the audit play")
	}
	out, err := h.srv.RenderPromptWith(cat, h.base, play, store.RoleOrchestrator,
		h.tokens["ORCHESTRATOR"])
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

// The 405 was correct: there is no state to write. What was missing is the
// sentence that stops an orchestrator looking for the verb.
func TestTheOrchestratorIsToldStateIsReadOnlyAndHowToAdvance(t *testing.T) {
	p := renderedOrchestratorPrompt(t)

	if !strings.Contains(p, "READ ONLY") {
		t.Fatalf("the state line must say it is a read:\n%s", p)
	}
	if !strings.Contains(p, "no state-write endpoint") {
		t.Fatalf("the prompt must close the door the orchestrator walked into:\n%s", p)
	}
	// And name what to do instead, or closing the door just leaves it stuck.
	if !strings.Contains(p, "advance") && !strings.Contains(p, "advances") {
		t.Fatalf("the prompt must say how a loop advances:\n%s", p)
	}
	if !strings.Contains(p, "SEND") {
		t.Fatalf("the prompt must name sending as the advance mechanism:\n%s", p)
	}
}

// It held the task the whole time. Asking a member for it is how a loop
// stalls before it starts.
func TestTheOrchestratorIsToldToProceedWithTheTaskItHolds(t *testing.T) {
	p := renderedOrchestratorPrompt(t)

	for _, needed := range []string{
		"You already hold the task",
		"Do not ask a member what the task is",
		"CONCRETE brief",
	} {
		if !strings.Contains(p, needed) {
			t.Fatalf("the orchestrator prompt is missing %q:\n%s", needed, p)
		}
	}
	// whoami is where it looks the task up, so it has to be named nearby.
	if !strings.Contains(p, "whoami") {
		t.Fatalf("the prompt must name where the task can be read:\n%s", p)
	}
}

// Nobody is told to poll any more, in any role. This is the property the
// whole delivery change exists to create, and it has to hold for the rendered
// prompt as a whole rather than only for the block that used to carry it.
func TestNoRoleIsToldToPoll(t *testing.T) {
	h := newHarness(t, nil, Config{MaxWait: 2 * time.Second})
	cat := store.DefaultCatalogue()
	play, ok := cat.LookupPlay("audit")
	if !ok {
		t.Fatal("the default catalogue must have the audit play")
	}
	for _, role := range store.PlayRoles(play) {
		out, err := h.srv.RenderPromptWith(cat, h.base, play, role, "tok")
		if err != nil {
			t.Fatalf("render %s: %v", role, err)
		}
		for _, banned := range []string{"while true", "FOREGROUND", "Do not add &"} {
			if strings.Contains(out, banned) {
				t.Fatalf("%s is still told to block (%q):\n%s", role, banned, out)
			}
		}
		if !strings.Contains(out, "DELIVERED") {
			t.Fatalf("%s is not told that mail arrives on its own:\n%s", role, out)
		}
	}
}

// The standing rule changed with the design. A rule that still said "wait in
// the foreground" would contradict the brief it is printed beside.
func TestTheStandingRuleTellsMembersToStayReachable(t *testing.T) {
	p := renderedOrchestratorPrompt(t)
	if !strings.Contains(p, "[stay-reachable]") {
		t.Fatalf("the standing rule must be present by name:\n%s", p)
	}
	if strings.Contains(p, "wait-in-foreground") {
		t.Fatalf("the superseded rule is still shipping:\n%s", p)
	}
	if !strings.Contains(p, "Never block waiting for mail") {
		t.Fatalf("the rule must say the thing it exists to say:\n%s", p)
	}
}

// A member's report reached the orchestrator only if the member chose to send
// it. The capture in agentd is the safety net; the brief is what should make
// it rare, so the obligation has to be unmistakable and it has to be the LAST
// thing a member reads about finishing.
func TestTheBriefMakesReportingMandatoryForWorkers(t *testing.T) {
	h := newHarness(t, nil, Config{MaxWait: 2 * time.Second})
	cat := store.DefaultCatalogue()
	play, ok := cat.LookupPlay("audit")
	if !ok {
		t.Fatal("the default catalogue must have the audit play")
	}
	out, err := h.srv.RenderPromptWith(cat, h.base, play, "INVESTIGATION", "tok")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, needed := range []string{
		"[report-or-you-are-not-done]",
		"NOT done until you have POSTed",
		"Reporting is your LAST ACT",
	} {
		if !strings.Contains(out, needed) {
			t.Fatalf("the worker brief is missing %q:\n%s", needed, out)
		}
	}
	// It must say WHY, or a model treats it as boilerplate: writing into its
	// own terminal reaches nobody.
	if !strings.Contains(out, "reaches nobody") {
		t.Fatalf("the brief must say why output alone is not a report:\n%s", out)
	}
}

// The orchestrator reports to the human, not to itself, so it must not carry
// a rule telling it to post to ORCHESTRATOR.
func TestTheOrchestratorIsExemptFromTheReportingRule(t *testing.T) {
	p := renderedOrchestratorPrompt(t)
	if strings.Contains(p, "[report-or-you-are-not-done]") {
		t.Fatalf("the orchestrator must not be told to report to itself:\n%s", p)
	}
	if strings.Contains(p, "Reporting is your LAST ACT") {
		t.Fatalf("the last-act clause is a worker obligation:\n%s", p)
	}
}
