// rules.go — the standing rules as a dictionary, not prose embedded in a
// template. Every rule has a stable name and one text, and prompts are
// assembled from names. A rule can then be changed in one place, and a rule
// that goes missing is a red test naming it rather than a loop that quietly
// stops honouring it.

package store

// Rule is one standing rule. Name is stable and is what tests and prompts
// refer to; Text is what the agent reads.
type Rule struct {
	Name string `json:"name" yaml:"name"`
	Text string `json:"text" yaml:"text"`
	// Binding marks a rule that is the ONLY control of its kind. Members and
	// solo sessions both spawn with permissions bypassed, so there is no
	// allowlist, no hook and no approval gate behind these sentences: the
	// text IS the control. An override may only APPEND to a binding rule, so
	// the engineer can tighten it or name a project script from his phone and
	// cannot weaken it.
	Binding bool `json:"binding,omitempty" yaml:"binding,omitempty"`
}

// Rule groups.
const (
	RulesShared       = "shared"
	RulesWorker       = "worker"
	RulesOrchestrator = "orchestrator"
	// RulesSolo are the rules a session with no play and no role uses. Kept
	// out of shared so a loop prompt never carries two reporting contracts.
	RulesSolo = "solo"
)

// The dictionary itself lives in defaults/rules.yml, embedded and parsed in
// defaults.go.

// RuleGroup returns one named group of the dictionary.
func RuleGroup(group string) []Rule {
	switch group {
	case RulesShared:
		return append([]Rule{}, sharedRules...)
	case RulesWorker:
		return append([]Rule{}, workerRules...)
	case RulesOrchestrator:
		return append([]Rule{}, orchestratorRules...)
	case RulesSolo:
		return append([]Rule{}, soloRules...)
	}
	return nil
}

// LookupRule finds one rule by name anywhere in the dictionary.
func LookupRule(name string) (Rule, bool) {
	for _, group := range [][]Rule{sharedRules, workerRules, orchestratorRules, soloRules} {
		for _, r := range group {
			if r.Name == name {
				return r, true
			}
		}
	}
	return Rule{}, false
}

// RuleNames lists every rule in the dictionary, by group order.
func RuleNames() []string {
	out := []string{}
	for _, group := range [][]Rule{sharedRules, workerRules, orchestratorRules, soloRules} {
		for _, r := range group {
			out = append(out, r.Name)
		}
	}
	return out
}
