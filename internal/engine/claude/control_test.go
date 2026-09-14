package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/engine"
)

// fakePTY records what the control types into the terminal. Submit is one
// composed line; Write is raw bytes (Ctrl-C).
type fakePTY struct {
	submitted []string
	written   []string
}

func (f *fakePTY) Write(b []byte) error  { f.written = append(f.written, string(b)); return nil }
func (f *fakePTY) Submit(t string) error { f.submitted = append(f.submitted, t); return nil }
func (f *fakePTY) last() string {
	if len(f.submitted) == 0 {
		return ""
	}
	return f.submitted[len(f.submitted)-1]
}

// An unbound control is a control for a run whose process is gone. Every
// write-method must say so rather than reporting a success it cannot have
// had — the previous version returned ErrUnsupported unconditionally, and
// the fix is to keep that answer for exactly this case.
func TestUnboundControlRefusesEveryWrite(t *testing.T) {
	c := NewControl()
	ctx := context.Background()
	for name, err := range map[string]error{
		"SetModel":   c.SetModel(ctx, "opus"),
		"RunCommand": c.RunCommand(ctx, "context", ""),
		"Prompt":     c.Prompt(ctx, "hello"),
		"Abort":      c.Abort(ctx),
	} {
		if !errors.Is(err, engine.ErrUnsupported) {
			t.Fatalf("%s on an unbound control = %v, want ErrUnsupported", name, err)
		}
	}
}

func TestBoundControlTypesTheRightBytes(t *testing.T) {
	c := NewControl()
	pty := &fakePTY{}
	c.BindPTY(pty, t.TempDir())
	ctx := context.Background()

	// /model always carries its argument: a bare /model opens the
	// interactive menu and leaves the TUI waiting on a choice nobody makes.
	if err := c.SetModel(ctx, "opus"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if pty.last() != "/model opus" {
		t.Fatalf("SetModel typed %q", pty.last())
	}
	if err := c.SetModel(ctx, "claude-opus-4-1-20250805"); err != nil {
		t.Fatalf("SetModel(full id): %v", err)
	}
	if pty.last() != "/model claude-opus-4-1-20250805" {
		t.Fatalf("a full model id must pass through, got %q", pty.last())
	}
	if err := c.SetModel(ctx, "  "); !errors.Is(err, engine.ErrUnsupported) {
		t.Fatalf("an empty model must be refused, got %v", err)
	}

	if err := c.RunCommand(ctx, "compact", "keep the repro"); err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if pty.last() != "/compact keep the repro" {
		t.Fatalf("RunCommand typed %q", pty.last())
	}
	if err := c.RunCommand(ctx, "/context", ""); err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if pty.last() != "/context" {
		t.Fatalf("an already-slashed name must not double up, got %q", pty.last())
	}

	if err := c.Prompt(ctx, "ship it"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if pty.last() != "ship it" {
		t.Fatalf("Prompt typed %q", pty.last())
	}

	if err := c.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if len(pty.written) != 1 || pty.written[0] != "\x03" {
		t.Fatalf("Abort must send Ctrl-C, got %q", pty.written)
	}
}

func TestCapabilitiesReportWhatInjectionCanActuallyDo(t *testing.T) {
	caps := NewControl().Capabilities()
	for name, got := range map[string]bool{
		"SetModel": caps.SetModel, "ListModels": caps.ListModels,
		"ListCommands": caps.ListCommands, "RunCommand": caps.RunCommand,
		"ListSkills": caps.ListSkills, "Prompt": caps.Prompt, "Abort": caps.Abort,
	} {
		if !got {
			t.Fatalf("caps.%s = false; injection serves it", name)
		}
	}
	// The agent row is a live switcher and there is no slash command that
	// changes a running session's agent.
	if caps.ListAgents || caps.SetAgent {
		t.Fatal("claude cannot switch agents live; the row must stay off")
	}
}

// The catalog is builtins plus whatever the filesystem defines for this run,
// and a name defined in more than one place resolves project > user >
// skill > builtin.
func TestCommandsMergeFilesWithBuiltinsAndRespectPrecedence(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)

	writeFile(t, filepath.Join(cwd, ".claude", "commands", "deploy.md"),
		"---\ndescription: Ship the current branch\nargument-hint: env\n---\n\nDo the thing.\n")
	// Same name in both scopes: the project's must win.
	writeFile(t, filepath.Join(cwd, ".claude", "commands", "audit.md"),
		"---\ndescription: Project audit\n---\n")
	writeFile(t, filepath.Join(home, ".claude", "commands", "audit.md"),
		"---\ndescription: User audit\n---\n")
	// No front matter at all: the first prose line is the description.
	writeFile(t, filepath.Join(home, ".claude", "commands", "notes.md"),
		"# Scratch notes\n\nbody\n")
	// A builtin name defined as a project command is shadowed by the file.
	writeFile(t, filepath.Join(cwd, ".claude", "commands", "review.md"),
		"---\ndescription: Our own review\n---\n")
	writeFile(t, filepath.Join(cwd, ".claude", "skills", "dataviz", "SKILL.md"),
		"---\nname: dataviz\ndescription: Draw the chart\n---\n")

	c := NewControl()
	c.BindPTY(&fakePTY{}, cwd)
	cmds, err := c.Commands(context.Background())
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	by := map[string]engine.Command{}
	for _, cmd := range cmds {
		if _, dup := by[cmd.Name]; dup {
			t.Fatalf("%q listed twice", cmd.Name)
		}
		by[cmd.Name] = cmd
	}

	if got := by["deploy"]; got.Description != "Ship the current branch" ||
		got.ArgsHint != "env" || got.Source != "project" {
		t.Fatalf("project command read back as %+v", got)
	}
	if got := by["audit"]; got.Description != "Project audit" || got.Source != "project" {
		t.Fatalf("the project entry must shadow the user one, got %+v", got)
	}
	if got := by["notes"]; got.Description != "Scratch notes" || got.Source != "user" {
		t.Fatalf("front-matter-less command read back as %+v", got)
	}
	if got := by["review"]; got.Source != "project" {
		t.Fatalf("a project file must shadow the builtin of the same name, got %+v", got)
	}
	if got := by["dataviz"]; got.Source != "skill" || got.Description != "Draw the chart" {
		t.Fatalf("a skill must be invocable as a command, got %+v", got)
	}
	// The builtins are still there, with their argument hints.
	if got := by["model"]; got.Source != "builtin" || got.ArgsHint != "alias" {
		t.Fatalf("builtin /model read back as %+v", got)
	}
	if _, ok := by["context"]; !ok {
		t.Fatalf("builtins missing from the catalog: %d entries", len(cmds))
	}
	// A builtin that takes nothing must not ask for an argument.
	if by["context"].ArgsHint != "" {
		t.Fatalf("/context must not claim an argument, got %q", by["context"].ArgsHint)
	}
	for i := 1; i < len(cmds); i++ {
		if cmds[i-1].Name > cmds[i].Name {
			t.Fatalf("catalog is not sorted: %q before %q", cmds[i-1].Name, cmds[i].Name)
		}
	}
}

func TestSkillsListProjectAndUserWithProjectWinning(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(cwd, ".claude", "skills", "shared", "SKILL.md"),
		"---\ndescription: Project copy\n---\n")
	writeFile(t, filepath.Join(home, ".claude", "skills", "shared", "SKILL.md"),
		"---\ndescription: User copy\n---\n")
	writeFile(t, filepath.Join(home, ".claude", "skills", "global-only", "SKILL.md"),
		"---\ndescription: Only at home\n---\n")

	c := NewControl()
	c.BindPTY(&fakePTY{}, cwd)
	skills, err := c.Skills(context.Background())
	if err != nil {
		t.Fatalf("Skills: %v", err)
	}
	by := map[string]engine.Skill{}
	for _, s := range skills {
		by[s.Name] = s
	}
	if len(skills) != 2 {
		t.Fatalf("want the two distinct skills, got %+v", skills)
	}
	if by["shared"].Description != "Project copy" {
		t.Fatalf("the project skill must win, got %q", by["shared"].Description)
	}
	if by["global-only"].Path == "" {
		t.Fatal("a skill must carry its path")
	}
}

// An unbound control has no cwd, so it can still name the builtins and the
// user's own commands — it just cannot see a project's.
func TestCommandsWithoutACwdStillListBuiltins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmds, err := NewControl().Commands(context.Background())
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	if len(cmds) < len(builtinCommands) {
		t.Fatalf("want at least the builtins, got %d", len(cmds))
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// Claude has no file part, so PromptWithFiles declines and the host names
// the paths instead. Declining is the whole contract: a Control that tried
// to type base64 into a TUI would "succeed" and deliver nothing.
func TestPromptWithFilesIsUnsupportedEvenWhenBound(t *testing.T) {
	c := NewControl()
	pty := &fakePTY{}
	c.BindPTY(pty, t.TempDir())

	err := c.PromptWithFiles(context.Background(), "look at this",
		[]engine.Attachment{{ID: "a", Path: "/tmp/shot.png", Mime: "image/png"}})
	if !errors.Is(err, engine.ErrUnsupported) {
		t.Fatalf("PromptWithFiles = %v, want ErrUnsupported", err)
	}
	if len(pty.submitted) != 0 || len(pty.written) != 0 {
		t.Fatalf("declining must type NOTHING, got %v %v", pty.submitted, pty.written)
	}
	if NewControl().Capabilities().AttachFiles {
		t.Fatal("claude has no native file part; caps.attachFiles must be false")
	}
}

// The host's fallback turn goes through the same paced submit as everything
// else. This is the exact text the TUI receives, asserted through the writer
// the control is bound to.
func TestTheAttachmentTurnReachesThePtyVerbatim(t *testing.T) {
	c := NewControl()
	pty := &fakePTY{}
	c.BindPTY(pty, t.TempDir())

	turn := "Attached image: /home/a/.agentflow/uploads/webapp/s1/20260906T110512Z-shot.png\n" +
		"\n" +
		"why is this misaligned?"
	if err := c.Prompt(context.Background(), turn); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(pty.submitted) != 1 {
		t.Fatalf("want exactly one submit, got %v", pty.submitted)
	}
	if pty.submitted[0] != turn {
		t.Fatalf("the TUI got:\n%q\nwant:\n%q", pty.submitted[0], turn)
	}
	// Multi-line is the point: without bracketed paste the first newline
	// would submit the path and orphan the question, which is why the hub
	// tracks the TUI's DECSET 2004 rather than guessing.
	if !strings.Contains(pty.submitted[0], "\n") {
		t.Fatal("the turn should be multi-line")
	}
}
