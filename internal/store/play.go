// play.go — a play is a named directed graph of steps, validated at definition
// time. Every rule the sequence depends on is checked here rather than asked
// for in a brief, so a play that could route worker to worker, end anywhere
// but at the orchestrator, strand a step or leave a transition ambiguous
// cannot be defined at all.

package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrPlayInvalid is the sentinel behind every definition-time refusal.
var ErrPlayInvalid = errors.New("store: invalid play")

// Verdicts that select a guarded edge.
const (
	OutcomeClean   = "clean"
	OutcomeChanges = "changes"
)

// familySeparator marks a fan-out instance: INVESTIGATION#2 is one member of
// the INVESTIGATION family and is addressed by that full role.
const familySeparator = "#"

// PlayEdge is one legal transition, and therefore one legal message: from the
// source step's actor to the target step's actor.
type PlayEdge struct {
	To string `yaml:"to"`
	// When guards the edge on the sender's outcome. Empty is the default
	// edge, and a step may have at most one.
	When string `yaml:"when,omitempty"`
	// FanOut means the source actor must address every member of the
	// target's family before the edge is taken.
	FanOut bool `yaml:"fanOut,omitempty"`
	// Loop marks the edge that goes backwards. Taking it bumps the loop's
	// round, which is what makes "until review reports clean" a countable
	// thing rather than a hope.
	Loop bool `yaml:"loop,omitempty"`
}

// PlayStep is one node: the role holding the work, and where it may go next.
type PlayStep struct {
	ID    string `yaml:"id"`
	Actor string `yaml:"actor"`
	// FanIn means every member of the actor's family owes a message before
	// the step advances.
	FanIn bool `yaml:"fanIn,omitempty"`
	// Note is the short label for display.
	Note string `yaml:"note"`
	// Brief is the step-specific expectation the engine queues for whoever
	// holds this step. It is the reason a role appearing twice in one play
	// does not need a hedged prompt: the invariant lives in the prompt, the
	// job of THIS step lives here.
	Brief string     `yaml:"brief"`
	Next  []PlayEdge `yaml:"next,omitempty"`
}

// Terminal reports whether the step ends the play.
func (s *PlayStep) Terminal() bool { return len(s.Next) == 0 }

// Play is the graph.
type Play struct {
	Name  string `yaml:"name"`
	Title string `yaml:"title"`
	// Purpose is one sentence saying what this play is FOR. The step notes
	// describe the machinery; a picker showing seven role chains and no
	// purpose asks the engineer to infer the point from the mechanism.
	Purpose string `yaml:"purpose"`
	Entry   string `yaml:"entry"`
	// MaxRounds caps how many times a loop-back edge may be taken. Zero
	// means DefaultMaxRounds. Without it nothing stops implementation and
	// review trading forever.
	MaxRounds int        `yaml:"maxRounds,omitempty"`
	Steps     []PlayStep `yaml:"steps"`
}

// DefaultMaxRounds is the cap a cyclic play gets when it declares none.
const DefaultMaxRounds = 3

// EffectiveMaxRounds is the cap actually enforced.
func (p *Play) EffectiveMaxRounds() int {
	if p.MaxRounds > 0 {
		return p.MaxRounds
	}
	return DefaultMaxRounds
}

// HasCycle reports whether any edge loops back.
func (p *Play) HasCycle() bool {
	for i := range p.Steps {
		for _, e := range p.Steps[i].Next {
			if e.Loop {
				return true
			}
		}
	}
	return false
}

// Step returns one step by id (nil when absent).
func (p *Play) Step(id string) *PlayStep {
	for i := range p.Steps {
		if p.Steps[i].ID == id {
			return &p.Steps[i]
		}
	}
	return nil
}

// BaseRole strips the fan-out instance suffix: INVESTIGATION#2 is in the
// INVESTIGATION family. Routing and step matching both compare families, so a
// swept role is a worker exactly like the role it was cloned from.
func BaseRole(role string) string {
	role = strings.ToUpper(strings.TrimSpace(role))
	if i := strings.Index(role, familySeparator); i > 0 {
		return role[:i]
	}
	return role
}

// FamilyMembers returns the roster roles belonging to one family, sorted. An
// unrepresented family returns the family name itself so callers always have
// something to name in an error.
func FamilyMembers(roster []string, family string) []string {
	family = BaseRole(family)
	out := []string{}
	for _, r := range roster {
		if BaseRole(r) == family {
			out = append(out, strings.ToUpper(r))
		}
	}
	if len(out) == 0 {
		return []string{family}
	}
	sort.Strings(out)
	return out
}

// ErrInvalidRole refuses a role name that cannot exist.
var ErrInvalidRole = errors.New("store: invalid role")

// fannedOutReservedRole reports a fan-out suffix on a role there is exactly
// one of. Fan-out is a worker concept: one orchestrator, one engineer.
func fannedOutReservedRole(role string) bool {
	up := strings.ToUpper(strings.TrimSpace(role))
	base := BaseRole(up)
	return base != up && (base == RoleOrchestrator || base == RoleEngineer)
}

// ValidateRole refuses a member role that cannot exist.
func ValidateRole(role string) error {
	up := strings.ToUpper(strings.TrimSpace(role))
	if up == "" {
		return fmt.Errorf("%w: empty role", ErrInvalidRole)
	}
	// ENGINE is the sender of every message agentd writes itself (step
	// briefs, stranded notices). A member called ENGINE would be
	// indistinguishable from the engine on the board and in its own inbox.
	if BaseRole(up) == RoleEngine {
		return fmt.Errorf("%w: %s is reserved for messages agentd writes itself", ErrInvalidRole, RoleEngine)
	}
	if fannedOutReservedRole(role) {
		return fmt.Errorf("%w: %s cannot be fanned out, there is exactly one %s",
			ErrInvalidRole, up, BaseRole(up))
	}
	return nil
}

// isOrchestratorRole reports whether a role is the orchestrator, family-aware.
func isOrchestratorRole(role string) bool { return BaseRole(role) == RoleOrchestrator }

// isEngineerRole reports whether a role is the engineer, family-aware.
func isEngineerRole(role string) bool { return BaseRole(role) == RoleEngineer }

// Validate refuses a play whose graph could break a rule the sequence depends
// on. Structural only: see ValidateRoster for the check that needs members.
func (p *Play) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("%w: unnamed play", ErrPlayInvalid)
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("%w: %s has no steps", ErrPlayInvalid, p.Name)
	}
	seen := map[string]bool{}
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.ID == "" {
			return fmt.Errorf("%w: %s has a step with no id", ErrPlayInvalid, p.Name)
		}
		if seen[s.ID] {
			return fmt.Errorf("%w: %s has two steps called %s", ErrPlayInvalid, p.Name, s.ID)
		}
		seen[s.ID] = true
		if s.Actor == "" {
			return fmt.Errorf("%w: %s step %s has no actor", ErrPlayInvalid, p.Name, s.ID)
		}
		if fannedOutReservedRole(s.Actor) {
			return fmt.Errorf("%w: %s step %s is held by %s; %s cannot be fanned out, there is exactly one",
				ErrPlayInvalid, p.Name, s.ID, s.Actor, BaseRole(s.Actor))
		}
		if s.FanIn && (isOrchestratorRole(s.Actor) || isEngineerRole(s.Actor)) {
			return fmt.Errorf("%w: %s step %s fans in on %s; fan-out is a worker concept",
				ErrPlayInvalid, p.Name, s.ID, s.Actor)
		}
		if s.Brief == "" {
			return fmt.Errorf("%w: %s step %s has no brief; a step with nothing to say to whoever holds it is a gap",
				ErrPlayInvalid, p.Name, s.ID)
		}
	}
	if p.Step(p.Entry) == nil {
		return fmt.Errorf("%w: %s has no entry step %q", ErrPlayInvalid, p.Name, p.Entry)
	}
	if p.MaxRounds < 0 {
		return fmt.Errorf("%w: %s has a negative round cap", ErrPlayInvalid, p.Name)
	}

	for i := range p.Steps {
		s := &p.Steps[i]
		if s.Terminal() {
			if !isOrchestratorRole(s.Actor) {
				return fmt.Errorf("%w: %s ends at step %s held by %s; every path must terminate at %s",
					ErrPlayInvalid, p.Name, s.ID, s.Actor, RoleOrchestrator)
			}
			continue
		}
		defaults, guards := 0, map[string]bool{}
		for _, e := range s.Next {
			target := p.Step(e.To)
			if target == nil {
				return fmt.Errorf("%w: %s step %s points at unknown step %q",
					ErrPlayInvalid, p.Name, s.ID, e.To)
			}
			if target.ID == s.ID {
				return fmt.Errorf("%w: %s step %s points at itself", ErrPlayInvalid, p.Name, s.ID)
			}
			if !isOrchestratorRole(s.Actor) && !isOrchestratorRole(target.Actor) &&
				!isEngineerRole(s.Actor) && !isEngineerRole(target.Actor) {
				return fmt.Errorf("%w: %s edge %s -> %s is %s to %s; workers may only message %s",
					ErrPlayInvalid, p.Name, s.ID, e.To, s.Actor, target.Actor, RoleOrchestrator)
			}
			if e.When == "" {
				defaults++
			} else if guards[e.When] {
				return fmt.Errorf("%w: %s step %s has two edges guarded on %q",
					ErrPlayInvalid, p.Name, s.ID, e.When)
			} else {
				guards[e.When] = true
			}
			if e.FanOut {
				if !target.FanIn {
					return fmt.Errorf("%w: %s edge %s -> %s fans out to a step that is not fan-in",
						ErrPlayInvalid, p.Name, s.ID, e.To)
				}
				if len(s.Next) != 1 {
					return fmt.Errorf("%w: %s step %s fans out and branches; a fan-out step must have exactly one edge",
						ErrPlayInvalid, p.Name, s.ID)
				}
			}
		}
		if defaults > 1 {
			return fmt.Errorf("%w: %s step %s has %d unguarded edges; the transition is ambiguous",
				ErrPlayInvalid, p.Name, s.ID, defaults)
		}
		// A fan-in step is drained by several members at once, so there is
		// no single verdict to pick an edge with. Refusing this at
		// definition time beats silently honouring whichever member
		// happened to report last.
		if s.FanIn && len(guards) > 0 {
			return fmt.Errorf("%w: %s step %s is fan-in and guarded; no one member's outcome can decide for the rest",
				ErrPlayInvalid, p.Name, s.ID)
		}
	}

	reachable := p.reachableFrom(p.Entry)
	for i := range p.Steps {
		if !reachable[p.Steps[i].ID] {
			return fmt.Errorf("%w: %s step %s is unreachable from %s",
				ErrPlayInvalid, p.Name, p.Steps[i].ID, p.Entry)
		}
	}
	for i := range p.Steps {
		if !p.canReachTerminal(p.Steps[i].ID) {
			return fmt.Errorf("%w: %s step %s can never reach a terminal step",
				ErrPlayInvalid, p.Name, p.Steps[i].ID)
		}
	}
	return nil
}

// ValidateRoster refuses a play whose steps name a role the loop cannot staff.
func (p *Play) ValidateRoster(roster []string) error {
	have := map[string]bool{}
	for _, r := range roster {
		have[BaseRole(r)] = true
	}
	for i := range p.Steps {
		s := &p.Steps[i]
		if !have[BaseRole(s.Actor)] {
			return fmt.Errorf("%w: %s step %s needs a %s member and this loop has none",
				ErrPlayInvalid, p.Name, s.ID, BaseRole(s.Actor))
		}
	}
	return nil
}

func (p *Play) reachableFrom(id string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		s := p.Step(cur)
		if s == nil {
			continue
		}
		for _, e := range s.Next {
			stack = append(stack, e.To)
		}
	}
	return seen
}

func (p *Play) canReachTerminal(id string) bool {
	for step := range p.reachableFrom(id) {
		if s := p.Step(step); s != nil && s.Terminal() {
			return true
		}
	}
	return false
}

// stepObligations lists the parties a step is waiting for. The meaning follows
// the step's shape, which is what lets one drain rule serve fan-out, fan-in
// and the ordinary one-message hop.
func stepObligations(p *Play, s *PlayStep, roster []string) []string {
	if s.FanIn {
		return FamilyMembers(roster, s.Actor)
	}
	if len(s.Next) == 1 && s.Next[0].FanOut {
		if target := p.Step(s.Next[0].To); target != nil {
			return FamilyMembers(roster, target.Actor)
		}
	}
	return []string{strings.ToUpper(s.Actor)}
}

// The catalogue itself lives in defaults/plays.yml, embedded and parsed in
// defaults.go. It was a Go literal until briefs became editable: leaving it
// here while rules moved to yaml would have been two places to look for the
// same kind of prose.

// LookupPlay returns a shipped play by name.
func LookupPlay(name string) (*Play, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for i := range plays {
		if plays[i].Name == name {
			return &plays[i], true
		}
	}
	return nil, false
}

// PlayNames lists the shipped plays.
func PlayNames() []string {
	out := make([]string, 0, len(plays))
	for i := range plays {
		out = append(out, plays[i].Name)
	}
	return out
}
