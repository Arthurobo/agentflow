// Package claude is the Claude Code engine implementation: it produces the
// argv and per-spawn env for a `claude` TTY, and exposes a thin Control
// backed by PTY injection. Every control action that OpenCode serves
// natively, Claude serves "approximately" — slash command in, best-effort
// read-back — and is so labelled in the UI via the Capabilities mask.
package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
)

// ID is engine.IDClaude.
const ID = engine.IDClaude

// Engine implements engine.Engine for Claude Code. The argv assembly mirrors
// spawner.buildArgs verbatim for the TTY path so behavior does not drift
// between today's spawner and the engine seam ( acceptance: every
// existing claude argv still passes the spawner tests).
type Engine struct {
	// Path is the claude binary override (defaults to PATH lookup or
	// AF_CLAUDE). The spawner also reads this — it just consults Engine
	// when going through the engine seam.
	Path string
}

// New builds the claude engine.
func New(path string) *Engine { return &Engine{Path: path} }

// ID implements engine.Engine.
func (e *Engine) ID() string { return ID }

// BuildArgs assembles the claude argv for a TTY spawn (engine.Options ->
// []string). Ordering matches the existing spawner.buildArgs TTY branch:
// optional --name for the title, then the common flags, then the positional
// first prompt as the FINAL argument so claude processes it as the first
// turn at boot.
func (e *Engine) BuildArgs(opts engine.Options) ([]string, error) {
	var args []string
	if opts.Title != "" {
		args = append(args, "--name", opts.Title)
	}
	args = append(args, commonArgs(opts)...)
	if opts.Prompt != "" {
		args = append(args, opts.Prompt)
	}
	return args, nil
}

// commonArgs mirrors spawner.commonArgs with the TTY-relevant subset.
func commonArgs(opts engine.Options) []string {
	var args []string
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	// Claude Code does not have a single --agent flag; the agent name is
	// referenced via the TUI's slash menu or as a subagent in
	// .claude/agents. We forward the chosen agent via the system prompt
	// pre-amble so a future PTY injection of `/agent <name>` has a stable
	// resting state.
	if opts.Agent != "" {
		args = append(args, "--append-system-prompt",
			fmt.Sprintf("Active agent: %s", opts.Agent))
	}
	// Product default for managed TTY runs: full autonomy. The TUI's prompt
	// menus never answer themselves.
	args = append(args, "--dangerously-skip-permissions")
	return args
}

// SpawnEnv returns the per-spawn env entries for a claude child. Claude Code
// does not need any overrides — the spawner's standard scrubbing strips the
// leak variables. nil = inherit.
func (e *Engine) SpawnEnv(_ engine.Options) []string { return nil }

// NeedsControlPort reports whether claude serves a side-channel API. It does
// not; the only "control" surface is the PTY itself, so the spawner MUST NOT
// allocate a port for claude sessions.
func (e *Engine) NeedsControlPort() bool { return false }

// LookupBinary returns the resolved claude path. Honors the engine-supplied
// override first, then falls back to the spawner's path resolution.
func (e *Engine) LookupBinary() (string, error) {
	if e.Path != "" {
		if p, err := lookupPath(e.Path); err == nil {
			return p, nil
		}
	}
	return lookupPath("claude")
}

// LookupVersion returns the output of `claude --version`, or "" on any error.
func (e *Engine) LookupVersion(ctx context.Context) string {
	bin, err := e.LookupBinary()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output() //nolint:gosec // G204: the engine binary resolved from PATH or AF_CLAUDE
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Control returns the thin claude control implementation. baseURL is
// ignored — Claude has no HTTP API. The implementation is a singleton (no
// state per-run); the per-run distinctions live in the spawner's PTY handle
// (which it does not touch).
func (e *Engine) Control(_ string) engine.Control { return NewControl() }

func lookupPath(name string) (string, error) {
	if strings.Contains(name, "/") {
		if _, err := os.Stat(name); err != nil {
			return "", fmt.Errorf("claude binary %q: %w", name, err)
		}
		return name, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("claude binary %q: not found in PATH", name)
}
