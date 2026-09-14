// waitcmd_test.go — what an idle member is told to do, executed here rather
// than merely inspected.
//
// The history is two failures, not one. First two agents BACKGROUNDED the
// blocking poll and their messages went into jobs nobody read, so the command
// was pinned to the foreground. Then a foreground poll made a member DEAF: a
// live loop froze with the engineer's typed prompt queued behind a curl that
// had minutes left to run.
//
// Mail is delivered into the terminal now, so an idle member waits at its
// prompt and hears mail and a human alike. What these tests pin is that the
// brief no longer tells anyone to block, and that the fallback left for
// members agentd did not spawn is bounded and returns on its own.
package mailapi

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

func waitShell(t *testing.T, h *harness, token string) string {
	t.Helper()
	base, err := promptBase(h.base)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	return h.srv.waitBlock(base, token)
}

// waitCommand returns the one fallback fetch in the block. There is exactly
// one command line and it is a bare curl; anything else is a regression.
func waitCommand(t *testing.T, h *harness, token string) string {
	t.Helper()
	block := waitShell(t, h, token)
	found := ""
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "curl ") {
			if found != "" {
				t.Fatalf("the block offers more than one command:\n%s", block)
			}
			found = trimmed
		}
	}
	if found == "" {
		t.Fatalf("no fallback command found in:\n%s", block)
	}
	return found
}

// run executes the command and reports whether it exited on its own.
func run(t *testing.T, cmdText string, budget time.Duration) (exited bool, out string) {
	t.Helper()
	for _, bin := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is required to prove the wait command actually behaves: %v", bin, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", cmdText)
	raw, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return false, string(raw)
	}
	if err != nil {
		t.Fatalf("wait command failed: %v\n%s", err, raw)
	}
	return true, string(raw)
}

// The whole point of the change: nothing in the brief tells a member to loop.
func TestTheBriefNoLongerTellsAnyoneToBlockOnAPoll(t *testing.T) {
	h := newHarness(t, nil, Config{MaxWait: 2 * time.Second})
	block := waitShell(t, h, h.tokens["INVESTIGATION"])

	// A polling loop by any of its names. "while true" is the exact shape
	// that shipped and froze a loop.
	for _, banned := range []string{"while true", "while :", "for ;;", "until ", "done"} {
		if strings.Contains(block, banned) {
			t.Fatalf("the brief still contains a polling loop (%q):\n%s", banned, block)
		}
	}
	// And it must not tell anyone to keep the command in the foreground,
	// because there is no longer a command worth blocking on.
	if strings.Contains(block, "FOREGROUND") || strings.Contains(block, "Do not add &") {
		t.Fatalf("the brief still pins a command to the foreground:\n%s", block)
	}
	// It has to say what actually happens instead, or a model will invent a
	// poll to fill the gap.
	for _, needed := range []string{"DELIVERED", "prompt", "Never wrap it in a loop"} {
		if !strings.Contains(block, needed) {
			t.Fatalf("the brief does not say %q:\n%s", needed, block)
		}
	}
}

// The fallback exists for a member running where agentd cannot reach it, and
// it must still be un-backgroundable by copy and paste.
func TestTheFallbackIsOneBoundedFetchAndIsMarkedOptional(t *testing.T) {
	h := newHarness(t, nil, Config{MaxWait: 2 * time.Second})
	block := waitShell(t, h, h.tokens["INVESTIGATION"])
	cmd := waitCommand(t, h, h.tokens["INVESTIGATION"])

	for _, banned := range []string{"&", "nohup", "disown", "setsid", "screen ", "tmux "} {
		if strings.Contains(cmd, banned) {
			t.Fatalf("the fallback contains %q; it must never be backgroundable by copy and paste", banned)
		}
	}
	if !strings.Contains(strings.ToLower(block), "fallback") ||
		!strings.Contains(block, "only if") {
		t.Fatalf("the fallback must be marked as one, or it becomes the default again:\n%s", block)
	}
	if !strings.Contains(cmd, "wait=") || !strings.Contains(cmd, "--max-time") {
		t.Fatalf("the fallback must be bounded on both ends: %s", cmd)
	}
}

// However long MaxWait is configured to be, the brief never hands out an
// unbounded block: a member parked for five minutes is the old bug.
func TestTheFallbackIsCappedEvenWhenTheServerAllowsALongWait(t *testing.T) {
	h := newHarness(t, nil, Config{MaxWait: 10 * time.Minute})
	cmd := waitCommand(t, h, h.tokens["INVESTIGATION"])
	want := fmt.Sprintf("wait=%d", fallbackWaitSeconds)
	if !strings.Contains(cmd, want) {
		t.Fatalf("a 10 minute MaxWait must still render %s, got: %s", want, cmd)
	}
}

// Executed, not inspected: it has to come back on its own with an empty
// inbox, which is what makes running it once safe.
func TestTheFallbackReturnsOnItsOwnWhenNothingArrives(t *testing.T) {
	h := newHarness(t, nil, Config{MaxWait: 2 * time.Second})
	cmd := waitCommand(t, h, h.tokens["INVESTIGATION"])

	exited, out := run(t, cmd, 20*time.Second)
	if !exited {
		t.Fatalf("the fallback must return on its own; it did not:\n%s", out)
	}
	if !strings.Contains(out, `"messages":[]`) {
		t.Fatalf("an empty inbox must come back as an empty inbox, got: %s", out)
	}
}

// And it returns the message when there is one.
func TestTheFallbackReturnsARealMessage(t *testing.T) {
	h := newHarness(t, nil, Config{MaxWait: 3 * time.Second})
	cmd := waitCommand(t, h, h.tokens["INVESTIGATION"])

	go func() {
		time.Sleep(300 * time.Millisecond)
		h.post(h.tokens["ORCHESTRATOR"], "/messages", map[string]any{
			"to": "INVESTIGATION", "subject": "brief", "body": "look at the second table",
		}, nil)
	}()
	exited, out := run(t, cmd, 20*time.Second)
	if !exited {
		t.Fatalf("the fallback did not return:\n%s", out)
	}
	if !strings.Contains(out, "look at the second table") {
		t.Fatalf("the fallback must carry the message: %s", out)
	}
}

func TestRenderedPromptCarriesTheRulesAndTheCommands(t *testing.T) {
	h := newHarness(t, nil, Config{})
	play, _ := store.LookupPlay("deep")
	prompt, err := h.srv.RenderPromptWith(store.DefaultCatalogue(), h.base, play, "REVIEW", h.tokens["REVIEW"])
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	pure, err := store.DefaultCatalogue().Prompt(play, "REVIEW")
	if err != nil {
		t.Fatalf("pure: %v", err)
	}
	if !strings.HasPrefix(prompt, pure) {
		t.Fatal("the rendered prompt must be the pure prompt plus commands, never a rewrite of it")
	}
	// "WHEN YOU HAVE NOTHING TO DO" replaced "WAIT": there is no command by
	// that name any more, because there is nothing an idle member runs.
	for _, want := range []string{"HOW TO REACH THE LOOP", "/whoami", "/inbox", "/messages",
		"WHEN YOU HAVE NOTHING TO DO", h.tokens["REVIEW"]} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the rendered prompt is missing %q", want)
		}
	}
	if strings.Contains(prompt, "/archive") {
		t.Fatal("archive and forward belong to the orchestrator, not a worker")
	}
	orch, err := h.srv.RenderPromptWith(store.DefaultCatalogue(), h.base, play, store.RoleOrchestrator, h.tokens[store.RoleOrchestrator])
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"/archive", "forward", "bodies=false"} {
		if !strings.Contains(orch, want) {
			t.Fatalf("the orchestrator prompt is missing %q", want)
		}
	}
	// A deployment with no base URL would render a prompt naming no endpoint,
	// which is worse than no prompt.
	if _, err := h.srv.RenderPromptWith(store.DefaultCatalogue(), "", play, "REVIEW", "tok"); err == nil {
		t.Fatal("rendering without a base URL must be refused")
	}
}

// The report IS the re-entry, so the rendered send line has to carry the wait.
// An agent copying these lines must not have to remember a second command at
// the exact moment it feels finished, which is the moment it stops following
// instructions.
func TestTheSendLineCarriesTheWait(t *testing.T) {
	h := newHarness(t, nil, Config{MaxWait: 300 * time.Second})
	prompt, err := h.srv.RenderPromptWith(store.DefaultCatalogue(), h.base, deepPlay(t), "INVESTIGATION", h.tokens["INVESTIGATION"])
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	sendLine := ""
	for _, line := range strings.Split(prompt, "\n") {
		if strings.Contains(line, "send ") && strings.Contains(line, "/messages") {
			sendLine = line
		}
	}
	if sendLine == "" {
		t.Fatalf("no send line in the prompt:\n%s", prompt)
	}
	if !strings.Contains(sendLine, "/messages?wait=") {
		t.Fatalf("the send line must post and wait in one call: %q", sendLine)
	}
}

// Every rule deleted is a rule that cannot be forgotten, and ack-when-done is
// the one that was: neither live investigator ever acked.
func TestTheAckRuleIsGoneAndTheLeaseRuleReplacedIt(t *testing.T) {
	h := newHarness(t, nil, Config{})
	prompt, err := h.srv.RenderPromptWith(store.DefaultCatalogue(), h.base, deepPlay(t), "INVESTIGATION", h.tokens["INVESTIGATION"])
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(prompt, "Ack a brief only when the work is genuinely done") {
		t.Fatalf("the ack rule must be gone:\n%s", prompt)
	}
	if !strings.Contains(prompt, "extend its lease") {
		t.Fatalf("the lease rule must have replaced it:\n%s", prompt)
	}
	if !strings.Contains(prompt, "reporting is what marks the brief done") {
		t.Fatalf("the prompt must say what replaced the ack:\n%s", prompt)
	}
}

func deepPlay(t *testing.T) *store.Play {
	t.Helper()
	p, ok := store.LookupPlay("deep")
	if !ok {
		t.Fatal("the deep play must be in the shipped catalogue")
	}
	return p
}
