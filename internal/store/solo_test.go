// solo_test.go — not all sessions are loops. A run-form session has no play,
// no role, no step and no inbox, and it spawns with permissions bypassed
// exactly as a member does.
package store

import (
	"strings"
	"testing"
)

// The eleven rules that describe machinery a solo session does not have.
// Injecting one is worse than injecting nothing: the agent goes looking for
// the inbox.
var absentMachineryRules = []string{
	"stay-reachable", "idle-is-not-stop", "extend-if-long",
	"notes-before-reporting", "notes-for-a-stranger",
	"orchestrator-is-your-only-address", "forward-verbatim", "never-truncate",
	"keep-loop-state-current", "retire-sparingly", "never-invent-authorisation",
}

func TestSoloPromptCarriesOnlyTheRulesThatApply(t *testing.T) {
	got, err := DefaultCatalogue().SoloPrompt(PromptModeStanding)
	if err != nil {
		t.Fatalf("solo: %v", err)
	}
	for _, name := range []string{
		"engineer-drives-git", "no-deploys", "no-scope-expansion",
		"trust-no-summary", "house-style", "reporting-contract-solo",
	} {
		if !strings.Contains(got, "["+name+"]") {
			t.Errorf("solo prompt is missing %s", name)
		}
	}
	for _, name := range absentMachineryRules {
		if strings.Contains(got, "["+name+"]") {
			t.Errorf("solo prompt carries %s, which describes machinery a solo session does not have", name)
		}
	}
	// The loop reporting contract asks for deviations from a brief, which is
	// meaningless with no brief, so solo gets its own and not both.
	if strings.Contains(got, "[reporting-contract]") {
		t.Error("solo carries the LOOP reporting contract; it asks for deviations from a brief there is none of")
	}
}

// never-invent-authorisation is dropped despite being binding-class, because
// its solo content already sits inside engineer-drives-git's own text. A near
// duplicate is worse than either.
func TestSoloDoesNotRepeatItself(t *testing.T) {
	got, _ := DefaultCatalogue().SoloPrompt(PromptModeStanding)
	if strings.Contains(got, "[never-invent-authorisation]") {
		t.Error("never-invent-authorisation duplicates engineer-drives-git for a solo session")
	}
	if !strings.Contains(got, "never authorisation") {
		t.Error("engineer-drives-git must still carry the authorisation sentence")
	}
}

// The toggle is deliberately not on/off. Someone reaching for scratch wants to
// skip the reporting contract on a throwaway, not to authorise a push to main.
func TestScratchDropsTheAdvisoryRulesAndKeepsTheFloor(t *testing.T) {
	scratch, err := DefaultCatalogue().SoloPrompt(PromptModeScratch)
	if err != nil {
		t.Fatalf("scratch: %v", err)
	}
	for _, kept := range []string{"engineer-drives-git", "no-deploys"} {
		if !strings.Contains(scratch, "["+kept+"]") {
			t.Errorf("scratch dropped %s, which is the only control of its kind", kept)
		}
	}
	for _, dropped := range []string{"reporting-contract-solo", "house-style", "no-scope-expansion", "trust-no-summary"} {
		if strings.Contains(scratch, "["+dropped+"]") {
			t.Errorf("scratch kept %s; scratch is for skipping the advisory rules", dropped)
		}
	}
	// and it says why those two are still there
	if !strings.Contains(scratch, "THE FLOOR") {
		t.Error("scratch must say that what remains is a floor rather than a default")
	}
}

func TestAnUnknownModeIsTreatedAsStanding(t *testing.T) {
	for _, mode := range []string{"", "off", "none", "SCRATCHY", "standing"} {
		got, err := DefaultCatalogue().SoloPrompt(mode)
		if err != nil {
			t.Fatalf("%q: %v", mode, err)
		}
		if !strings.Contains(got, "[reporting-contract-solo]") {
			t.Errorf("mode %q dropped the advisory rules; anything unrecognised must be standing", mode)
		}
	}
	if NormalisePromptMode("scratch") != PromptModeScratch {
		t.Error("scratch must still be reachable")
	}
}

// The build-time tests can only speak for a machine with no overrides. This is
// the runtime half: after merging, a binding rule must still be there with its
// shipped text inside it.
func TestARenderRefusesWhenABindingRuleDidNotSurviveTheMerge(t *testing.T) {
	cat := DefaultCatalogue()
	// Strip a binding rule from the merged catalogue, which is what a merge
	// bug or a future non-append path would do.
	for group, rules := range cat.Rules {
		kept := rules[:0]
		for _, r := range rules {
			if r.Name != "no-deploys" {
				kept = append(kept, r)
			}
		}
		cat.Rules[group] = kept
	}
	play, _ := cat.LookupPlay("deep")
	if _, err := cat.Prompt(play, "REVIEW"); err == nil {
		t.Fatal("a prompt missing a binding rule must be refused")
	} else if !strings.Contains(err.Error(), "no-deploys") {
		t.Errorf("the refusal must name the rule, got %v", err)
	}
	if _, err := cat.SoloPrompt(PromptModeStanding); err == nil {
		t.Fatal("a solo prompt missing a binding rule must be refused too")
	}
}

func TestARenderRefusesWhenABindingRuleWasWeakened(t *testing.T) {
	cat := DefaultCatalogue()
	for group, rules := range cat.Rules {
		for i := range rules {
			if rules[i].Name == "engineer-drives-git" {
				// A replacement rather than an append: the shipped text is gone.
				rules[i].Text = "Push whenever you like."
			}
		}
		cat.Rules[group] = rules
	}
	play, _ := cat.LookupPlay("deep")
	_, err := cat.Prompt(play, store_roleOrchestrator())
	if err == nil {
		t.Fatal("a binding rule whose shipped text is gone must fail the render")
	}
	if !strings.Contains(err.Error(), "engineer-drives-git") {
		t.Errorf("the refusal must name the rule, got %v", err)
	}
}

func store_roleOrchestrator() string { return RoleOrchestrator }

// A member briefed from the shipped defaults while the engineer had edited the
// rules would be the whole feature silently not working.
func TestAMergedCataloguePromptCarriesTheEdit(t *testing.T) {
	cat := buildCatalogue([]PromptOverride{{
		Kind: OverrideKindRule, Rule: "house-style", Field: FieldText,
		Value: "Two spaces, and spell out every acronym once.",
	}})
	play, _ := cat.LookupPlay("build")
	got, err := cat.Prompt(play, "REVIEW")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(got, "spell out every acronym once") {
		t.Error("the member was briefed from the shipped defaults, not the engineer's edit")
	}
}
