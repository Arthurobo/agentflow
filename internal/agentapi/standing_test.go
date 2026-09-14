// standing_test.go — how a run-form session's standing text is delivered, and
// why it differs by engine.
package agentapi

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

func standingStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "standing.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Claude has --append-system-prompt, which sits above the positional and
// survives auto-compact. The engineer's typed task stays the positional,
// UNMODIFIED, because managed_sessions.prompt persists it and the stored birth
// prompt should read back as his task rather than boilerplate with a sentence
// buried in it.
func TestClaudeGetsTheRulesInTheSystemPromptAndKeepsTheTaskIntact(t *testing.T) {
	st := standingStore(t)
	task := "Fix the flaky guest export"
	opts, carrier, err := attachStandingPrompt(context.Background(), st, "", spawner.Options{
		Engine: engine.IDClaude, Prompt: task,
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if carrier != "system" {
		t.Errorf("claude carries the rules in the system prompt, got %q", carrier)
	}
	if opts.Prompt != task {
		t.Errorf("the engineer's task must be untouched, got %q", opts.Prompt)
	}
	if !strings.Contains(opts.AppendSystemPrompt, "[engineer-drives-git]") {
		t.Error("the standing rules did not reach the system prompt")
	}
	if strings.Contains(opts.Prompt, "engineer-drives-git") {
		t.Error("the rules must not be concatenated into the stored birth prompt")
	}
}

// OpenCode has no system-prompt flag at all: --port --hostname --session
// --agent --model --prompt --auto and the positional. The rules go into
// --prompt behind a separator, and that difference is visible rather than
// silent, because "why did my rules not stick on the OpenCode session" is
// otherwise unanswerable.
func TestOpenCodeGetsTheRulesPrependedBecauseItHasNowhereElse(t *testing.T) {
	st := standingStore(t)
	task := "Fix the flaky guest export"
	opts, carrier, err := attachStandingPrompt(context.Background(), st, "", spawner.Options{
		Engine: engine.IDOpenCode, Prompt: task,
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if carrier != "prompt" {
		t.Errorf("opencode has nowhere else to put them, got %q", carrier)
	}
	if opts.AppendSystemPrompt != "" {
		t.Error("opencode has no system-prompt flag; setting one would silently drop the rules")
	}
	if !strings.Contains(opts.Prompt, "[engineer-drives-git]") {
		t.Error("the rules did not reach the prompt")
	}
	if !strings.Contains(opts.Prompt, soloSeparator) {
		t.Error("the engineer's task must be separated from the rules, not run together")
	}
	if !strings.HasSuffix(opts.Prompt, task) {
		t.Errorf("the task must be last so it reads as the instruction, got %q", opts.Prompt)
	}
}

// Scratch is not off.
func TestScratchStillCarriesTheFloorOnASoloSpawn(t *testing.T) {
	st := standingStore(t)
	opts, _, err := attachStandingPrompt(context.Background(), st, "scratch", spawner.Options{
		Engine: engine.IDClaude, Prompt: "throwaway",
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	for _, kept := range []string{"engineer-drives-git", "no-deploys"} {
		if !strings.Contains(opts.AppendSystemPrompt, "["+kept+"]") {
			t.Errorf("scratch dropped %s", kept)
		}
	}
	if strings.Contains(opts.AppendSystemPrompt, "[reporting-contract-solo]") {
		t.Error("scratch should have dropped the reporting contract")
	}
}

// The engineer's edits reach a solo session too, not just loop members.
func TestASoloSessionCarriesTheEngineersEdits(t *testing.T) {
	st := standingStore(t)
	ctx := context.Background()
	if _, err := st.PutPromptOverride(ctx, store.PromptOverride{
		Kind: store.OverrideKindRule, Rule: "house-style", Field: store.FieldText,
		Value: "British spelling throughout.",
	}); err != nil {
		t.Fatalf("override: %v", err)
	}
	opts, _, err := attachStandingPrompt(ctx, st, "", spawner.Options{
		Engine: engine.IDClaude, Prompt: "task",
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !strings.Contains(opts.AppendSystemPrompt, "British spelling throughout.") {
		t.Error("a solo session was briefed from the shipped defaults, not the engineer's edit")
	}
}
