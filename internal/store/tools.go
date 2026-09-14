// tools.go — which code tool a member runs on and which model, validated at
// creation. The registry states what has actually been probed instead of
// guessing: an unprobed tool accepts a model it cannot check and says so,
// which is honest, where a made-up model list would be a silent lie.

package store

import (
	"errors"
	"fmt"
	"github.com/arthurobo/agentflow/internal/engine/opencode"
	"strings"
)

// ErrUnknownTool refuses a tool or a tool and model pairing we cannot honour.
var ErrUnknownTool = errors.New("store: unknown tool or model")

// Tool ids, the same vocabulary as engine.IDClaude / engine.IDOpenCode. This
// is deliberately not a second name for an engine.
const (
	ToolClaude   = "claude"
	ToolOpenCode = "opencode"
)

// ToolSpec is one entry in the registry.
type ToolSpec struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Models is the accepted set. Nil means the set is not known.
	Models []string `json:"models,omitempty"`
	// Probed says whether Models was established against the real tool. When
	// false, any non-empty model is accepted and NOTHING has verified it.
	Probed bool `json:"probed"`
	// DefaultModelAllowed permits an empty model, meaning the tool picks.
	DefaultModelAllowed bool `json:"defaultModelAllowed"`
	// Note carries what a caller should know before trusting the entry.
	Note string `json:"note,omitempty"`
}

var toolRegistry = []ToolSpec{
	{
		ID: ToolClaude, Title: "Claude Code",
		Models:              []string{"opus", "sonnet", "haiku", "fable"},
		Probed:              true,
		DefaultModelAllowed: true,
		Note:                "the CLI's own aliases; an empty model leaves the choice to the user's default for the cwd",
	},
	{
		ID: ToolOpenCode, Title: "OpenCode",
		// Still unprobed: which models exist is a property of THIS install,
		// so the registry cannot list them and will not guess. The catalogue
		// comes from opencode itself at the moment it is asked.
		Models: nil,
		Probed: false,
		// An empty model was refused as a registry policy, not a technical
		// limit: BuildArgs omits --model when empty and opencode then uses
		// its own configured default, which is a real and often correct
		// answer. Refusing it forced a guess to be typed.
		DefaultModelAllowed: true,
		Note:                "opencode's model list is a property of this machine, so it is asked for rather than listed here; leave the model empty to use opencode's own default",
	},
}

// Tools lists the registry.
func Tools() []ToolSpec {
	out := make([]ToolSpec, len(toolRegistry))
	copy(out, toolRegistry)
	return out
}

// LookupTool finds one entry.
func LookupTool(id string) (ToolSpec, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, t := range toolRegistry {
		if t.ID == id {
			return t, true
		}
	}
	return ToolSpec{}, false
}

// NormaliseTool maps empty to the default tool.
func NormaliseTool(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return ToolClaude
	}
	return id
}

// ValidateToolModel refuses a pairing the registry cannot honour.
func ValidateToolModel(tool, model string) error {
	tool = NormaliseTool(tool)
	model = strings.TrimSpace(model)
	spec, ok := LookupTool(tool)
	if !ok {
		ids := make([]string, 0, len(toolRegistry))
		for _, t := range toolRegistry {
			ids = append(ids, t.ID)
		}
		return fmt.Errorf("%w: no tool %q; the registry has %s",
			ErrUnknownTool, tool, strings.Join(ids, ", "))
	}
	if model == "" {
		if spec.DefaultModelAllowed {
			return nil
		}
		return fmt.Errorf("%w: %s has no known default model, so one must be named (%s)",
			ErrUnknownTool, tool, spec.Note)
	}
	// The shape floor. It applies whether or not the set is known, because a
	// bare id is not an error to opencode: it resolves to nothing and the
	// member silently runs on the default, which is how "minimax" reached a
	// spawn. This is the only check that can be made without asking the
	// machine, so it runs first and always.
	if tool == ToolOpenCode {
		if err := opencode.ValidateModelRef(model); err != nil {
			return fmt.Errorf("%w: %s", ErrUnknownTool, err)
		}
	}
	if !spec.Probed {
		return nil
	}
	for _, m := range spec.Models {
		if strings.EqualFold(m, model) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s does not run model %q; it runs %s",
		ErrUnknownTool, tool, model, strings.Join(spec.Models, ", "))
}
