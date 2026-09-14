package agentapi

import (
	"context"
	"strings"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// soloSeparator marks where the standing text ends and the engineer's task
// begins, on the one engine that cannot keep them apart.
const soloSeparator = "\n\n--- the engineer's task follows ---\n\n"

// attachStandingPrompt gives a run-form session its standing text.
//
// Not all sessions are loops. A session with no play and no role still spawns
// with permissions bypassed, so the rules that describe the engineer apply to
// it exactly as they apply to a member, and it used to get none of them.
//
// DELIVERY DIFFERS BY ENGINE, and the difference is visible on the session row
// rather than buried:
//
//   - claude takes --append-system-prompt, which sits one rung above the
//     positional prompt and survives auto-compact. The standing text goes
//     there and the engineer's typed prompt stays the positional, UNMODIFIED.
//     managed_sessions.prompt persists that positional, so the stored birth
//     prompt reads back as his actual task rather than boilerplate with a
//     sentence buried in it.
//   - opencode has no system-prompt flag at all. Its whole set is --port
//     --hostname --session --agent --model --prompt --auto plus the
//     positional, so the standing text is prepended to --prompt behind a
//     separator. Worse in theory and better in practice than an extra
//     synchronous inference round trip in PostStart, but it does mean an
//     OpenCode session's stored prompt carries the rules inside it.
func attachStandingPrompt(ctx context.Context, st *store.Store, mode string, opts spawner.Options) (spawner.Options, string, error) {
	cat, err := st.PromptCatalogue(ctx)
	if err != nil {
		return opts, "", err
	}
	text, err := cat.SoloPrompt(mode)
	if err != nil {
		return opts, "", err
	}
	if strings.TrimSpace(text) == "" {
		return opts, "", nil
	}
	if opts.Engine == engine.IDOpenCode {
		opts.Prompt = text + soloSeparator + opts.Prompt
		return opts, "prompt", nil
	}
	opts.AppendSystemPrompt = text
	return opts, "system", nil
}
