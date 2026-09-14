// prompt.go — the rendering layer. store.Prompt stays pure, a function of play
// and role and nothing else; this appends the concrete endpoints and what an
// idle member should do, which is nothing.

package mailapi

import (
	"fmt"
	"strings"

	"github.com/arthurobo/agentflow/internal/store"
)

// RenderPromptWith renders against a specific catalogue: the merged one on a
// machine with overrides, the shipped one otherwise. A member briefed from the
// defaults while the engineer had edited the rules would be the whole feature
// silently not working.
func (s *Server) RenderPromptWith(cat *store.PromptCatalogue, baseURL string, play *store.Play, role, token string) (string, error) {
	base, err := promptBase(baseURL)
	if err != nil {
		return "", err
	}
	body, err := cat.Prompt(play, role)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n")
	b.WriteString(s.commandsBlock(base, token, store.BaseRole(role) == store.RoleOrchestrator))
	b.WriteString("\n")
	b.WriteString(s.waitBlock(base, token))
	return b.String(), nil
}

func promptBase(baseURL string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return "", fmt.Errorf("mailapi: no base URL, so a prompt would name no endpoint")
	}
	return base + Prefix, nil
}

func (s *Server) commandsBlock(base, token string, orchestrator bool) string {
	wait := int(s.cfg.MaxWait.Seconds())
	if wait <= 0 {
		wait = int(DefaultMaxWait.Seconds())
	}
	h := fmt.Sprintf(`-H "X-Agent-Token: %s"`, token)
	jsonH := h + ` -H 'Content-Type: application/json'`
	lines := []string{
		"HOW TO REACH THE LOOP",
		fmt.Sprintf(`  whoami   curl -s "%s/whoami" %s`, base, h) +
			"   # your role, your loop, its task, and the brief for the step you are on",
		fmt.Sprintf(`  state    curl -s "%s/state" %s`, base, h) +
			"   # READ ONLY: where the play stands and who is on the roster.\n" +
			"           # There is no state-write endpoint and there is nothing to write.\n" +
			"           # A loop advances when you SEND: the step engine moves on messages,\n" +
			"           # so delegating a brief IS advancing the loop.",
		fmt.Sprintf(`  catchup  curl -s "%s/notes" %s`, base, h) +
			"   # what your role already established, across sessions",
		fmt.Sprintf(`  history  curl -s "%s/thread?limit=20" %s`, base, h) +
			"   # your own past messages",
		fmt.Sprintf(`  note     curl -s -X POST "%s/notes" %s -d '{"body":"what you just established"}'`, base, jsonH),
		fmt.Sprintf(`  send     curl -s -X POST "%s/messages?wait=%d" %s -d '{"to":"ORCHESTRATOR","subject":"...","body":"..."}'`, base, wait, jsonH) +
			"\n           # posts AND waits: the reply is your next brief. Nothing to remember afterwards.",
		fmt.Sprintf(`  verdict  curl -s -X POST "%s/messages" %s -d '{"to":"ORCHESTRATOR","body":"...","outcome":"clean"}'`, base, jsonH) +
			`   # a guarded step decides on your outcome: "clean" or "changes"`,
		fmt.Sprintf(`  ack      curl -s -X POST "%s/messages/<MESSAGE_ID>/ack" %s`, base, h) +
			"   # rarely needed: reporting already marks your brief done",
		fmt.Sprintf(`  extend   curl -s -X POST "%s/messages/<MESSAGE_ID>/extend" %s -d '{"seconds":3600}'`, base, jsonH),
		fmt.Sprintf(`  read     curl -s "%s/messages/<MESSAGE_ID>" %s`, base, h) +
			"   # the whole body of any message in this loop's lineage",
	}
	if orchestrator {
		lines = append(lines,
			fmt.Sprintf(`  forward  curl -s -X POST "%s/messages" %s -d '{"to":"REVIEW","subject":"...","forward":"<MESSAGE_ID>"}'`, base, jsonH)+
				"   # byte-identical copy, never retype a report",
			fmt.Sprintf(`  peek     curl -s "%s/inbox?wait=0&bodies=false" %s`, base, h)+
				"   # ids, senders and sizes with no bodies, so you can route without reading",
			fmt.Sprintf(`  archive  curl -s "%s/archive" %s`, base, h)+
				"   # every message in this loop and the loops it grew from, ids and sizes, no bodies",
		)
	}
	return strings.Join(lines, "\n") + "\n"
}

// fallbackWaitSeconds caps the optional fetch. Long enough to be worth one
// call, short enough that a member using it is responsive between calls
// rather than parked.
const fallbackWaitSeconds = 30

// waitBlock used to be the one command an idle agent ran: a blocking
// foreground poll it was told never to background. Both halves of that were
// wrong, and the comment that used to live here admitted half of it — a
// backgrounded wait swallows messages into a job nobody reads, so it was
// pinned to the foreground instead.
//
// The cost of the foreground was worse than the bug it avoided. A member
// blocked in that poll is DEAF: it cannot see the engineer typing into its
// own terminal. A live loop froze exactly that way, with a typed prompt
// sitting queued behind a curl that had minutes left to run, and the loop
// looking dead to everyone watching it.
//
// Neither is necessary now. agentd delivers mail INTO the member's terminal
// (see the courier), so the correct thing for an idle member to do is sit at
// its prompt and be reachable — by mail and by a human at the same time,
// which is the one property the old design could not have.
//
// The command survives as a FALLBACK, bounded and explicitly optional,
// because a member running somewhere agentd did not spawn it has no courier
// and must still be able to fetch. It is bounded so that even that member
// spends most of its time responsive rather than blocked.
func (s *Server) waitBlock(base, token string) string {
	wait := int(s.cfg.MaxWait.Seconds())
	if wait <= 0 {
		wait = int(DefaultMaxWait.Seconds())
	}
	if wait > fallbackWaitSeconds {
		wait = fallbackWaitSeconds
	}
	cmd := fmt.Sprintf(
		`curl -s --max-time %d "%s/inbox?wait=%d" -H "X-Agent-Token: %s"`,
		wait+10, base, wait, token)
	return "WHEN YOU HAVE NOTHING TO DO\n" +
		"Stop and wait at your prompt. Do not poll, and do not write a loop around anything.\n" +
		"Mail is DELIVERED to you: when a message arrives it appears here as a prompt, the same " +
		"way the engineer typing to you does. Blocking on a command to wait for mail is the one " +
		"thing that stops both from reaching you, which is why there is no polling loop in this " +
		"brief any more.\n\n" +
		"  fallback, only if you were told your mail is not being delivered:\n" +
		"  " + cmd + "\n" +
		"  It comes back within " + fmt.Sprint(wait) + " seconds whether or not anything arrived. " +
		"Run it once, act on what it gives you, and go back to your prompt. Never wrap it in a loop.\n"
}
