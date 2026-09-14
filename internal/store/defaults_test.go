// defaults_test.go — the shipped yaml. These are the tests that used to speak
// for a Go literal; they now speak for the embedded documents, and they
// additionally guarantee that nobody dropped a rule or reshaped a graph in a
// way Validate happens to accept.
package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden shape. Validate cannot catch a careless edit that moves a step
// from REVIEW to IMPLEMENTATION or renames conclude: both still validate. This
// names every play, its step ids in order, and who holds each one.
var goldenPlays = []struct {
	name   string
	steps  []string
	actors []string
}{
	{"recon", []string{"brief", "investigate", "conclude"},
		[]string{"ORCHESTRATOR", "INVESTIGATION", "ORCHESTRATOR"}},
	{"audit", []string{"brief", "investigate", "route", "review", "conclude"},
		[]string{"ORCHESTRATOR", "INVESTIGATION", "ORCHESTRATOR", "REVIEW", "ORCHESTRATOR"}},
	{"patch", []string{"brief", "implement", "conclude"},
		[]string{"ORCHESTRATOR", "IMPLEMENTATION", "ORCHESTRATOR"}},
	{"build", []string{"brief", "implement", "route", "review", "conclude"},
		[]string{"ORCHESTRATOR", "IMPLEMENTATION", "ORCHESTRATOR", "REVIEW", "ORCHESTRATOR"}},
	{"deep", []string{"brief", "investigate", "route_findings", "review_findings", "plan",
		"implement", "route_work", "review_work", "rework", "conclude"},
		[]string{"ORCHESTRATOR", "INVESTIGATION", "ORCHESTRATOR", "REVIEW", "ORCHESTRATOR",
			"IMPLEMENTATION", "ORCHESTRATOR", "REVIEW", "ORCHESTRATOR", "ORCHESTRATOR"}},
	{"sweep", []string{"brief", "sweep", "collate", "review", "conclude"},
		[]string{"ORCHESTRATOR", "INVESTIGATION", "ORCHESTRATOR", "REVIEW", "ORCHESTRATOR"}},
	{"second-opinion", []string{"brief", "review", "conclude"},
		[]string{"ORCHESTRATOR", "REVIEW", "ORCHESTRATOR"}},
}

func TestEmbeddedPlaysMatchTheirGoldenShape(t *testing.T) {
	if len(PlayNames()) != len(goldenPlays) {
		t.Fatalf("catalogue has %d plays, golden has %d: %v", len(PlayNames()), len(goldenPlays), PlayNames())
	}
	for _, want := range goldenPlays {
		p, ok := LookupPlay(want.name)
		if !ok {
			t.Errorf("play %q is gone from the shipped yaml", want.name)
			continue
		}
		if len(p.Steps) != len(want.steps) {
			t.Errorf("%s has %d steps, golden has %d", want.name, len(p.Steps), len(want.steps))
			continue
		}
		for i, s := range p.Steps {
			if s.ID != want.steps[i] {
				t.Errorf("%s step %d is %q, golden says %q", want.name, i, s.ID, want.steps[i])
			}
			if s.Actor != want.actors[i] {
				t.Errorf("%s step %s is held by %q, golden says %q", want.name, s.ID, s.Actor, want.actors[i])
			}
		}
		if strings.TrimSpace(p.Purpose) == "" {
			t.Errorf("%s ships with no purpose", want.name)
		}
	}
}

// Every shipped play must validate, and this now covers the yaml rather than a
// literal. A failure here is a build bug: init() panics on it, so this test is
// the thing that tells you why before you ship the binary.
func TestEveryEmbeddedPlayValidates(t *testing.T) {
	for _, name := range PlayNames() {
		p, ok := LookupPlay(name)
		if !ok {
			t.Fatalf("%s vanished between listing and lookup", name)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("shipped play %s does not validate: %v", name, err)
		}
	}
}

// The two binding rules are the only control of their kind. This asserts the
// yaml still marks them, because a rule that quietly stops being binding stops
// being append-only and can then be weakened from a phone.
func TestTheBindingRulesAreMarkedInTheShippedYaml(t *testing.T) {
	want := map[string]bool{"engineer-drives-git": true, "no-deploys": true}
	got := map[string]bool{}
	for _, name := range RuleNames() {
		r, _ := LookupRule(name)
		if r.Binding {
			got[name] = true
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s is no longer binding; it is the only control of its kind", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s became binding without a decision; binding means append-only forever", name)
		}
	}
	// no-scope-expansion is deliberately NOT binding: it is a quality rule as
	// much as a safety one and the one most reasonably reworded per project.
	if r, _ := LookupRule("no-scope-expansion"); r.Binding {
		t.Error("no-scope-expansion must stay overridable")
	}
}

// Every rule in the dictionary has text, and no two share a name.
func TestTheShippedDictionaryIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range RuleNames() {
		if seen[name] {
			t.Errorf("rule %q appears twice; a prompt would carry it twice", name)
		}
		seen[name] = true
		r, ok := LookupRule(name)
		if !ok || strings.TrimSpace(r.Text) == "" {
			t.Errorf("rule %q has no text", name)
		}
	}
	// roleJobs moved out of prompt.go and must have arrived intact.
	for _, role := range []string{"INVESTIGATION", "IMPLEMENTATION", "REVIEW", RoleOrchestrator} {
		if strings.TrimSpace(DefaultCatalogue().Roles[role]) == "" {
			t.Errorf("role %q has no statement of what it is for", role)
		}
	}
}

// The data-directory copy is output, not input: it says so, and nothing reads
// it back.
func TestTheDefaultsCopyIsMarkedGenerated(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDefaultsCopy(dir); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, name := range DefaultsFileNames {
		raw, err := os.ReadFile(filepath.Join(dir, "defaults", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.HasPrefix(string(raw), "# GENERATED - DO NOT EDIT.") {
			t.Errorf("%s does not say it is generated; a file that looks editable and is not is worse than none", name)
		}
		if !strings.Contains(string(raw), "override surface") {
			t.Errorf("%s does not say where edits belong", name)
		}
	}
	// Rewritten every boot, so a stale copy cannot survive.
	if err := os.WriteFile(filepath.Join(dir, "defaults", "plays.yml"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteDefaultsCopy(dir); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "defaults", "plays.yml"))
	if strings.Contains(string(raw), "tampered") {
		t.Error("the copy was not rewritten, so it can drift from what the binary uses")
	}
}

// The embedded documents are compiled in, so bad content there is a build bug
// rather than a machine problem, and the daemon must refuse to start on it.
// These hand the loader the bytes directly, because a check with no way to
// reach it is a check nothing holds us to.
func TestBadEmbeddedContentIsRefusedRatherThanRun(t *testing.T) {
	good := func(name string) []byte {
		raw, err := defaultsFS.ReadFile("defaults/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	plays, rules := good("plays.yml"), good("rules.yml")

	// Restore the real catalogue whatever these attempts do to it.
	t.Cleanup(func() {
		if err := applyDefaults(plays, rules); err != nil {
			t.Fatalf("could not restore the real defaults: %v", err)
		}
	})

	for _, tc := range []struct {
		name         string
		plays, rules []byte
		want         string
	}{
		{"a play that does not validate",
			[]byte("plays:\n  - name: broken\n    title: Broken\n    entry: nowhere\n    steps:\n      - id: a\n        actor: ORCHESTRATOR\n        note: n\n        brief: b\n"),
			rules, "entry step"},
		{"a step with no brief",
			[]byte("plays:\n  - name: broken\n    title: Broken\n    entry: a\n    steps:\n      - id: a\n        actor: ORCHESTRATOR\n        note: n\n"),
			rules, "no brief"},
		{"no plays at all", []byte("plays: []\n"), rules, "ships no plays"},
		{"unparseable yaml", []byte("plays: [oh no\n"), rules, "parse plays.yml"},
		{"no shared rules", plays, []byte("shared: []\n"), "shared rules"},
		{"a solo block naming a rule that is in no group", plays,
			[]byte("shared:\n    - name: engineer-drives-git\n      text: t\n" +
				"solo:\n    intro: hi\n    rules:\n        - a-rule-nobody-wrote\n"),
			"in no group"},
	} {
		err := applyDefaults(tc.plays, tc.rules)
		if err == nil {
			t.Errorf("%s: must be refused, this binary would ship briefs nobody wrote", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error mentioning %q, got %v", tc.name, tc.want, err)
		}
	}
}
