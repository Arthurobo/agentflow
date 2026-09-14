// prompt.go — a prompt is a function of the play and the role, and nothing
// else. It names no task and no loop id on purpose: members are adopted into
// successor loops with their tokens intact, so a prompt with the task baked in
// goes stale the moment that happens, and it would force the engineer to
// decide the task before he could even see a prompt. The agent reads its loop,
// its task and its step at runtime.

package store

import (
	"errors"
	"fmt"
	"strings"
)

// ErrRoleNotInPlay refuses a prompt for a role the play never gives work to.
var ErrRoleNotInPlay = errors.New("store: role holds no step in this play")

// ErrBindingRuleMissing refuses to render a prompt whose binding rules did not
// survive the merge. Two of the seventeen rules are the only control of their
// kind: members and solo sessions both spawn with permissions bypassed, so
// there is no allowlist, no hook and no approval gate behind those sentences.
// A merge that lost one is a prompt that authorises a push.
var ErrBindingRuleMissing = errors.New("store: a binding rule did not survive the merge")

// Prompt modes. The toggle is deliberately not on/off: someone reaching for
// scratch wants to skip the reporting contract on a throwaway, not to
// authorise a push to main, so scratch drops the advisory rules and KEEPS the
// binding ones. That is what makes the floor a floor rather than a default.
const (
	PromptModeStanding = "standing"
	PromptModeScratch  = "scratch"
)

// NormalisePromptMode maps anything unrecognised onto the safe mode.
func NormalisePromptMode(mode string) string {
	if strings.ToLower(strings.TrimSpace(mode)) == PromptModeScratch {
		return PromptModeScratch
	}
	return PromptModeStanding
}

// roleJobs is the one-line statement of what a role is for. It lives in
// defaults/rules.yml now; see defaults.go.

// PlayRoles lists the distinct roles a play gives work to, in step order.
func PlayRoles(p *Play) []string {
	seen := map[string]bool{}
	out := []string{}
	for i := range p.Steps {
		r := strings.ToUpper(p.Steps[i].Actor)
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// StepsHeldBy lists the steps a role holds in a play, family-aware.
func StepsHeldBy(p *Play, role string) []*PlayStep {
	out := []*PlayStep{}
	for i := range p.Steps {
		if BaseRole(p.Steps[i].Actor) == BaseRole(role) {
			out = append(out, &p.Steps[i])
		}
	}
	return out
}

// Prompt renders against THIS catalogue, so a member is briefed with the
// engineer's edits rather than the shipped text.
func (c *PromptCatalogue) Prompt(p *Play, role string) (string, error) {
	if p == nil {
		return "", fmt.Errorf("%w: no play", ErrPlayInvalid)
	}
	if err := ValidateRole(role); err != nil {
		return "", err
	}
	role = strings.ToUpper(strings.TrimSpace(role))
	held := StepsHeldBy(p, role)
	if len(held) == 0 {
		return "", fmt.Errorf("%w: %s in %s", ErrRoleNotInPlay, role, p.Name)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are %s in the %s play.\n\n", role, p.Title)

	fmt.Fprintf(&b, "WHAT YOU ARE FOR\n%s\n\n", c.Roles[BaseRole(role)])

	b.WriteString("HOW THIS PLAY RUNS\n")
	for i := range p.Steps {
		s := &p.Steps[i]
		marker := "  "
		if BaseRole(s.Actor) == BaseRole(role) {
			marker = "> "
		}
		shape := ""
		if s.FanIn {
			shape = " (every member of the family reports before this step advances)"
		}
		fmt.Fprintf(&b, "%s%-16s %-16s %s%s\n", marker, s.ID, s.Actor, s.Note, shape)
	}
	b.WriteString("\n")

	if len(held) == 1 {
		fmt.Fprintf(&b, "You hold one step, %s. It carries its own brief; read it when the step arrives.\n\n", held[0].ID)
	} else {
		ids := make([]string, 0, len(held))
		for _, s := range held {
			ids = append(ids, s.ID)
		}
		fmt.Fprintf(&b, "You hold %d steps: %s. They are DIFFERENT jobs and each carries its own brief. "+
			"Read the brief when the step arrives and do that job, not the one you did last time.\n\n",
			len(held), strings.Join(ids, ", "))
	}

	b.WriteString("HOW YOU WORK\n")
	b.WriteString("This prompt names no task and no loop, on purpose: you outlive both. " +
		"When a loop ends you are parked, not stopped, and the next loop adopts you with this same prompt " +
		"and a different task. So read your loop, its task, its state and your current step at runtime, " +
		"every time you wake. Never assume the task is the one you saw last.\n")
	b.WriteString("Wait for a brief. Work it. Report with send. The reply to your report is your next brief " +
		"when there is one. There is no separate step to remember when you " +
		"finish. The engine owns the sequence: a message that does not belong to the current step is refused " +
		"and tells you which step you were on, so a refusal is information, not a failure.\n")
	if !isOrchestratorRole(role) {
		b.WriteString("Reporting is your LAST ACT, not an optional one. When the work is done, POST it to " +
			"the ORCHESTRATOR with send or verdict, and only then stop. Output you write into your own " +
			"terminal reaches nobody: the orchestrator sees your messages and nothing else, so a finished " +
			"task you did not post is a task nobody knows you did and a loop that cannot move.\n")
	}
	b.WriteString("Mail is DELIVERED to you: a message arrives in your terminal as a prompt, exactly like " +
		"the engineer typing to you. So when you have nothing to do, stop at your prompt. Do not poll, do " +
		"not run a command to wait, and never wrap anything in a loop. A command that blocks while you wait " +
		"for mail is the one thing that makes you deaf to BOTH the mail and the human.\n")
	if isOrchestratorRole(role) {
		b.WriteString("The engineer is a participant, not a side channel: progress you narrate is a message " +
			"addressed to ENGINEER, and the engineer typing reaches you the same way mail does.\n")
		b.WriteString("You already hold the task. It is on your loop, whoami returns it, and it was sent to " +
			"you as your first message. Proceed with it. Do not ask a member what the task is and do not ask " +
			"the engineer to restate it: asking for work you were already given is how a loop stalls before " +
			"it starts. Your first act is to turn that task into a CONCRETE brief and send it to the member " +
			"the current step names, saying what you want, against what, and what to report back.\n")
		b.WriteString("You advance the loop by SENDING. There is no state to write and no endpoint that " +
			"writes it; state is a read. If a step is waiting on somebody, the way to move it is to give " +
			"that somebody their brief.\n")
	}
	b.WriteString("\n")

	rules := c.RulesFor(role)
	if err := assertBindingRulesIntact(rules); err != nil {
		return "", err
	}
	b.WriteString("THE STANDING RULES\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "[%s] %s\n", r.Name, r.Text)
	}
	return b.String(), nil
}

// assertBindingRulesIntact is the runtime half of the binding guarantee. The
// build-time tests can only speak for a machine with no overrides; this speaks
// for the machine the prompt is actually being rendered on. Append semantics
// makes it a substring check: a binding rule's shipped text must still be
// inside whatever the merge produced.
//
// A failure fails THIS render and refuses to spawn THIS member. It does not
// take down the daemon and it does not fail the loop: the other members are
// unaffected and the engineer gets a message naming the rule.
func assertBindingRulesIntact(rules []Rule) error {
	byName := map[string]string{}
	for _, r := range rules {
		byName[r.Name] = r.Text
	}
	for _, def := range RuleNames() {
		shipped, ok := LookupRule(def)
		if !ok || !shipped.Binding {
			continue
		}
		text, present := byName[def]
		if !present {
			// A binding rule that applies to this role and is not in the
			// rendered set at all.
			if ruleAppliesToRenderedSet(def, rules) {
				return fmt.Errorf("%w: %s is missing", ErrBindingRuleMissing, def)
			}
			continue
		}
		if !strings.Contains(text, shipped.Text) {
			return fmt.Errorf("%w: %s no longer contains its shipped text", ErrBindingRuleMissing, def)
		}
	}
	return nil
}

// ruleAppliesToRenderedSet reports whether a binding rule belongs in this
// particular render. Every binding rule today is shared, so it belongs in all
// of them; this exists so a future binding rule in a narrower group does not
// make every other render fail.
func ruleAppliesToRenderedSet(name string, _ []Rule) bool {
	for _, r := range RuleGroup(RulesShared) {
		if r.Name == name {
			return true
		}
	}
	return false
}

// SoloPrompt renders the standing text for a session with NO play and no role.
// A run-form session is still an agent working for the engineer, and the rules
// that matter are the ones that describe the engineer rather than the loop.
//
// It references rules BY NAME from the same dictionary rather than copying
// text, so a rule edited once is edited everywhere.
func (c *PromptCatalogue) SoloPrompt(mode string) (string, error) {
	mode = NormalisePromptMode(mode)
	var b strings.Builder
	if intro := strings.TrimSpace(c.Solo.Intro); intro != "" && mode == PromptModeStanding {
		b.WriteString(intro)
		b.WriteString("\n\n")
	}
	chosen := []Rule{}
	for _, name := range c.Solo.Rules {
		r, ok := c.LookupRule(name)
		if !ok {
			return "", fmt.Errorf("%w: solo block names %q, which is in no group", ErrPlayInvalid, name)
		}
		if mode == PromptModeScratch && !r.Binding {
			continue
		}
		chosen = append(chosen, r)
	}
	if err := assertBindingRulesIntact(chosen); err != nil {
		return "", err
	}
	if mode == PromptModeScratch {
		b.WriteString("THE FLOOR\nThese hold on every session, including this one.\n")
	} else {
		b.WriteString("THE STANDING RULES\n")
	}
	for _, r := range chosen {
		fmt.Fprintf(&b, "[%s] %s\n", r.Name, r.Text)
	}
	return b.String(), nil
}
