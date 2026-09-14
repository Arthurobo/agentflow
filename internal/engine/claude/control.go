package claude

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/claudeconfig"
	"github.com/arthurobo/agentflow/internal/engine"
)

// Control is the PTY-backed Claude implementation of engine.Control. Claude
// Code has no side-channel API, but everything the control sheet offers is
// reachable by typing: a model change is "/model <alias>", a slash command is
// its own name, an abort is Ctrl-C. So the control layer types.
//
// The injector this file once claimed did not exist, and the methods were
// switched off rather than built. The writer below is that injector: the
// host binds the run's live PTY (BindPTY) before the first call, and every
// write-method answers ErrUnsupported while nothing is bound, which is the
// truth for a run whose process is gone.
type Control struct {
	mu  sync.Mutex
	pty engine.PTYWriter
	cwd string
}

// NewControl returns a fresh Control. It is inert until BindPTY.
func NewControl() *Control { return &Control{} }

// BindPTY implements engine.PTYBinder.
func (c *Control) BindPTY(w engine.PTYWriter, cwd string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pty, c.cwd = w, cwd
}

func (c *Control) bound() (engine.PTYWriter, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pty, c.cwd
}

// submit types one line into the TUI and submits it.
func (c *Control) submit(text string) error {
	w, _ := c.bound()
	if w == nil {
		return engine.ErrUnsupported
	}
	return w.Submit(text)
}

// Capabilities implements engine.Control.
func (c *Control) Capabilities() engine.Caps {
	// Everything true here is served by keystroke injection, which is the
	// only channel Claude Code has and a real one. ListAgents/SetAgent stay
	// false: the Agent row is a live switcher and there is no "/agent x"
	// that changes the running session's agent.
	return engine.Caps{
		ListModels:      true,
		SetModel:        true,
		ListAgents:      false,
		SetAgent:        false,
		ListCommands:    true,
		RunCommand:      true,
		ListSkills:      true,
		AttachFiles:     false, // no file part; the host names the paths instead
		ListProviders:   false,
		SetProvider:     false,
		ListMCPServers:  false,
		Prompt:          true,
		Abort:           true,
		ReplyPermission: false, // handled by the existing approval hook
		ReplyQuestion:   false,
		Events:          true, // claude events flow through the existing hook + corpus tailer path
	}
}

// Aliases are the model names the CLI itself accepts. They are a property of
// the CLI rather than of the machine, so unlike opencode's they can be listed
// here without asking anything.
var Aliases = []engine.Model{
	{ID: "opus", DisplayName: "Opus", Provider: "claude", Status: "active"},
	{ID: "sonnet", DisplayName: "Sonnet", Provider: "claude", Status: "active"},
	{ID: "haiku", DisplayName: "Haiku", Provider: "claude", Status: "active"},
	{ID: "fable", DisplayName: "Fable", Provider: "claude", Status: "active"},
}

// Models implements engine.Control with the CLI's own alias set. Full model
// ids are accepted by SetModel too; the aliases are what a picker can name
// without asking the machine.
func (c *Control) Models(_ context.Context) ([]engine.Model, error) {
	return append([]engine.Model(nil), Aliases...), nil
}

// builtinCommands is Claude Code's own slash-command set. It is VERSION
// DEPENDENT: the CLI ships commands with releases and nothing enumerates
// them, so this table is a snapshot (sanity-checked against 2.1.263) and the
// file-scanned project/user entries below override any name it gets wrong.
var builtinCommands = []engine.Command{
	{Name: "add-dir", Description: "Allow tool access to another directory", ArgsHint: "path"},
	{Name: "agents", Description: "Manage agents"},
	{Name: "artifacts", Description: "List published artifacts"},
	{Name: "clear", Description: "Clear the conversation"},
	{Name: "compact", Description: "Compact the conversation", ArgsHint: "optional instructions"},
	{Name: "config", Description: "Open the config panel"},
	{Name: "context", Description: "Show context usage"},
	{Name: "cost", Description: "Show session cost"},
	{Name: "doctor", Description: "Diagnose the installation"},
	{Name: "exit", Description: "End the session"},
	{Name: "export", Description: "Export the conversation"},
	{Name: "fast", Description: "Toggle fast mode"},
	{Name: "help", Description: "List commands"},
	{Name: "hooks", Description: "Manage hooks"},
	{Name: "init", Description: "Write a CLAUDE.md for this repo"},
	{Name: "install-github-app", Description: "Install the GitHub app"},
	{Name: "login", Description: "Sign in"},
	{Name: "logout", Description: "Sign out"},
	{Name: "mcp", Description: "Manage MCP servers"},
	{Name: "memory", Description: "Edit memory files"},
	{Name: "model", Description: "Switch model", ArgsHint: "alias"},
	{Name: "permissions", Description: "Manage tool permissions"},
	{Name: "plan", Description: "Enter plan mode"},
	{Name: "pr-comments", Description: "Read PR comments"},
	{Name: "release-notes", Description: "Show release notes"},
	{Name: "remember", Description: "Save something to memory"},
	{Name: "rename", Description: "Rename this session", ArgsHint: "name"},
	{Name: "resume", Description: "Resume a session", ArgsHint: "optional id"},
	{Name: "review", Description: "Review code", ArgsHint: "optional target"},
	{Name: "rewind", Description: "Rewind the conversation"},
	{Name: "skill-doctor", Description: "Diagnose skills"},
	{Name: "stats", Description: "Show usage stats"},
	{Name: "status", Description: "Show session status"},
	{Name: "statusline", Description: "Configure the status line"},
	{Name: "tasks", Description: "Show background tasks"},
	{Name: "terminal-setup", Description: "Configure the terminal"},
	{Name: "todos", Description: "Show the todo list"},
	{Name: "usage", Description: "Show plan usage"},
	{Name: "vim", Description: "Toggle vim mode"},
}

// Commands implements engine.Control: the builtin table plus everything the
// filesystem defines for this run — project commands, user commands, and
// skills (a skill is invoked as /<name> like any other). Precedence on a
// name clash is project > user > builtin, and a project skill outranks a
// user one, so a repo can shadow a global command with its own.
func (c *Control) Commands(_ context.Context) ([]engine.Command, error) {
	_, cwd := c.bound()
	home, _ := os.UserHomeDir()

	var ordered []engine.Command
	if cwd != "" {
		ordered = append(ordered, commandsIn(filepath.Join(cwd, ".claude", "commands"), "project")...)
	}
	if home != "" {
		ordered = append(ordered, commandsIn(filepath.Join(home, ".claude", "commands"), "user")...)
	}
	if cwd != "" {
		ordered = append(ordered, skillCommands(filepath.Join(cwd, ".claude", "skills"))...)
	}
	if home != "" {
		ordered = append(ordered, skillCommands(filepath.Join(home, ".claude", "skills"))...)
	}
	for _, b := range builtinCommands {
		b.Source = "builtin"
		ordered = append(ordered, b)
	}

	seen := map[string]bool{}
	out := make([]engine.Command, 0, len(ordered))
	for _, cmd := range ordered {
		if cmd.Name == "" || seen[cmd.Name] {
			continue
		}
		seen[cmd.Name] = true
		out = append(out, cmd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// commandsIn reads one .claude/commands directory. Name is the filename
// without .md; description and argument hint come from the front matter when
// the file has any, and from the first non-empty line when it does not.
func commandsIn(dir, source string) []engine.Command {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []engine.Command
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // path is the user's own commands dir
		if err != nil {
			continue
		}
		text := string(body)
		desc := claudeconfig.FrontMatterField(text, "description")
		if desc == "" {
			desc = firstProseLine(text)
		}
		out = append(out, engine.Command{
			Name:        strings.TrimSuffix(e.Name(), ".md"),
			Description: strings.Trim(desc, "\"'`"),
			ArgsHint:    strings.Trim(claudeconfig.FrontMatterField(text, "argument-hint"), "\"'`"),
			Source:      source,
		})
	}
	return out
}

// skillCommands lists a skills directory as invocable commands (/<skill>).
func skillCommands(dir string) []engine.Command {
	skills := claudeconfig.SkillsIn(dir)
	out := make([]engine.Command, 0, len(skills))
	for _, s := range skills {
		out = append(out, engine.Command{
			Name: s.Name, Description: s.Description, Source: "skill",
		})
	}
	return out
}

// firstProseLine is the fallback description: the first heading or first
// non-empty line outside the front matter.
func firstProseLine(body string) string {
	rest := body
	if strings.HasPrefix(rest, "---") {
		if end := strings.Index(rest[3:], "\n---"); end >= 0 {
			rest = rest[3+end+4:]
		}
	}
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line != "" {
			return line
		}
	}
	return ""
}

// Agents implements engine.Control. Always returns engine.ErrUnsupported:
// the agent is a spawn-time directive, not something a live session switches.
func (c *Control) Agents(_ context.Context) ([]engine.Agent, error) {
	return nil, engine.ErrUnsupported
}

// Skills implements engine.Control: the project's and the user's skills,
// read-only. Project entries shadow user ones of the same name.
func (c *Control) Skills(_ context.Context) ([]engine.Skill, error) {
	_, cwd := c.bound()
	home, _ := os.UserHomeDir()
	var found []claudeconfig.Skill
	if cwd != "" {
		found = append(found, claudeconfig.SkillsIn(filepath.Join(cwd, ".claude", "skills"))...)
	}
	if home != "" {
		found = append(found, claudeconfig.SkillsIn(filepath.Join(home, ".claude", "skills"))...)
	}
	seen := map[string]bool{}
	out := make([]engine.Skill, 0, len(found))
	for _, s := range found {
		if seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		out = append(out, engine.Skill{Name: s.Name, Description: s.Description, Path: s.Path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// MCPServers implements engine.Control. Always returns engine.ErrUnsupported.
func (c *Control) MCPServers(_ context.Context) ([]engine.MCPServer, error) {
	return nil, engine.ErrUnsupported
}

// Providers implements engine.Control. Always returns engine.ErrUnsupported.
func (c *Control) Providers(_ context.Context) ([]engine.Provider, error) {
	return nil, engine.ErrUnsupported
}

// Permissions implements engine.Control. Claude's permission flow runs
// through the existing approval hook, so this Control returns
// ErrUnsupported; the host dispatches via approvals.Service instead.
func (c *Control) Permissions(_ context.Context) ([]engine.PermissionRequest, error) {
	return nil, engine.ErrUnsupported
}

// ReplyPermission implements engine.Control. Always ErrUnsupported (claude
// uses the hook path).
func (c *Control) ReplyPermission(_ context.Context, _ string, _ bool, _ string) error {
	return engine.ErrUnsupported
}

// Questions implements engine.Control. Always returns the empty list; claude
// TUI questions are answered via the menu key sequence, not via a separate
// HTTP API.
func (c *Control) Questions(_ context.Context) ([]engine.Question, error) {
	return nil, engine.ErrUnsupported
}

// ReplyQuestion implements engine.Control. Always ErrUnsupported.
func (c *Control) ReplyQuestion(_ context.Context, _, _ string) error {
	return engine.ErrUnsupported
}

// State implements engine.Control. Always returns an empty SessionState —
// the TUI cannot report its live model, so the sheet reads back the value
// persisted on the run row when a switch is applied.
func (c *Control) State(_ context.Context) (engine.SessionState, error) {
	return engine.SessionState{}, nil
}

// SetModel implements engine.Control by typing "/model <model>". Aliases and
// full model ids both work; the argument is never omitted, because a bare
// /model opens the interactive menu and leaves the TUI waiting on a choice
// nobody is there to make. The change applies from the next turn.
func (c *Control) SetModel(_ context.Context, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return engine.ErrUnsupported
	}
	return c.submit("/model " + model)
}

// SetAgent implements engine.Control. Always returns engine.ErrUnsupported.
func (c *Control) SetAgent(_ context.Context, _ string) error {
	return engine.ErrUnsupported
}

// SetProvider implements engine.Control. Always returns engine.ErrUnsupported.
func (c *Control) SetProvider(_ context.Context, _ string) error {
	return engine.ErrUnsupported
}

// RunCommand implements engine.Control by typing the slash command.
func (c *Control) RunCommand(_ context.Context, name, args string) error {
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "/"))
	if name == "" {
		return engine.ErrUnsupported
	}
	line := "/" + name
	if args = strings.TrimSpace(args); args != "" {
		line += " " + args
	}
	return c.submit(line)
}

// Prompt implements engine.Control by typing the message and submitting it.
func (c *Control) Prompt(_ context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return engine.ErrUnsupported
	}
	return c.submit(text)
}

// PromptWithFiles implements engine.Control by declining, on purpose.
//
// Claude Code has no message API and no file part: the TUI is the only
// channel, and typing base64 into it is not a delivery mechanism. What it
// DOES have is a Read tool that opens image files, so the host names the
// paths in the turn instead (see the control API's prompt endpoint). That
// is a real delivery, which is why Caps.AttachFiles is false rather than
// the feature being unavailable.
func (c *Control) PromptWithFiles(_ context.Context, _ string, _ []engine.Attachment) error {
	return engine.ErrUnsupported
}

// Abort implements engine.Control with Ctrl-C, which is what interrupts a
// turn in the TUI without terminating the process.
func (c *Control) Abort(_ context.Context) error {
	w, _ := c.bound()
	if w == nil {
		return engine.ErrUnsupported
	}
	return w.Write([]byte("\x03"))
}

// WaitReady implements engine.Control. No-op for Claude — the spawner
// already waits for system/init via the corpus tailer.
func (c *Control) WaitReady(_ context.Context, _ time.Duration) error { return nil }
