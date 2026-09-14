package claudeconfig_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/claudeconfig"
)

// TestLoad_MergesSettingsJsonAndClaudeJson asserts the documented merge
// order: ~/.claude/settings.json wins over ~/.claude.json on every field,
// and both contribute to the AllowedTools + Hooks arrays.
func TestLoad_MergesSettingsJsonAndClaudeJson(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".claude.json"), `{
		"model": "claude-sonnet-4-5",
		"allowedTools": ["Read", "Bash"],
		"hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "/lower"}]}]}
	}`)
	mustWrite(t, filepath.Join(home, ".claude", "settings.json"), `{
		"model": "claude-haiku-4-5",
		"permissionMode": "acceptEdits",
		"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "/upper"}]}]}
	}`)

	got := claudeconfig.Load(home, "")
	if got.Settings.Model != "claude-haiku-4-5" {
		t.Fatalf("upper must win on model: %q", got.Settings.Model)
	}
	if got.Settings.PermissionMode != "acceptEdits" {
		t.Fatalf("permissionMode from upper: %q", got.Settings.PermissionMode)
	}
	if len(got.Settings.AllowedTools) != 2 {
		t.Fatalf("allowedTools must merge lower+upper: %v", got.Settings.AllowedTools)
	}
	if len(got.Settings.Hooks) != 2 {
		t.Fatalf("hooks merged count = %d, want 2", len(got.Settings.Hooks))
	}
}

// TestLoad_CwdScopedClaudeMd pins the cwd-scoped CLAUDE.md + parent walk
// precedence. ~/.claude/CLAUDE.md loads for every cwd; cwd-scoped files
// load for the matching one.
func TestLoad_CwdScopedClaudeMd(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	mustWrite(t, filepath.Join(home, ".claude", "CLAUDE.md"), "# global\n")
	mustWrite(t, filepath.Join(cwd, "CLAUDE.md"), "# cwd\n")
	mustWrite(t, filepath.Join(cwd, "CLAUDE.local.md"), "# cwd-local\n")

	sum := claudeconfig.Load(home, cwd)
	if len(sum.Files) < 2 {
		t.Fatalf("expected at least 2 files, got %d", len(sum.Files))
	}
	// both must report Loaded = true (the parent walk is capped at /
	// in real life; here we keep it shallow so the test stays portable).
	foundGlobal := false
	foundCwd := false
	for _, f := range sum.Files {
		if f.Origin != "CLAUDE.md" || !f.Loaded {
			continue
		}
		if filepath.Base(f.Path) == "CLAUDE.md" {
			if filepath.Dir(f.Path) == cwd {
				foundCwd = true
			} else if filepath.Dir(f.Path) == filepath.Join(home, ".claude") {
				foundGlobal = true
			}
		}
	}
	if !foundGlobal || !foundCwd {
		t.Fatalf("global=%v cwd=%v", foundGlobal, foundCwd)
	}
}

// TestLoad_SkillsAndAgents walks the .claude/skills and .claude/agents
// dirs and surfaces their leading description lines.
func TestLoad_SkillsAndAgents(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".claude", "skills", "deploy", "SKILL.md"), "# deploy\ndescription: Ship things\n")
	mustWrite(t, filepath.Join(home, ".claude", "agents", "reviewer.md"), "---\nmodel: claude-sonnet-4-5\n---\ndescription: review code\n")

	sum := claudeconfig.Load(home, "")
	if len(sum.Skills) != 1 || sum.Skills[0].Name != "deploy" || sum.Skills[0].Description != "Ship things" {
		t.Fatalf("skills: %+v", sum.Skills)
	}
	if len(sum.Agents) != 1 || sum.Agents[0].Name != "reviewer" || sum.Agents[0].Description != "review code" {
		t.Fatalf("agents: %+v", sum.Agents)
	}
	if sum.Agents[0].Model != "claude-sonnet-4-5" {
		t.Fatalf("agent model from front-matter: %q", sum.Agents[0].Model)
	}
}

// TestLoad_MissingHomeIsSafe pins the "no ~/.claude" case — a brand-new
// install never panics; the summary is empty but well-formed.
func TestLoad_MissingHomeIsSafe(t *testing.T) {
	home := filepath.Join(t.TempDir(), "does-not-exist")
	sum := claudeconfig.Load(home, "")
	if len(sum.Files) != 0 {
		t.Fatalf("files: %d, want 0", len(sum.Files))
	}
	// roundtrip JSON to confirm the shape is wire-safe
	if _, err := json.Marshal(sum); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// A nil Go slice marshals as `null`, not `[]`, and that null was what made
// the config page read "Failed to load config." on every machine without a
// ~/.claude/skills directory. The zero value of "a list of skills" is an
// empty list, and it has to survive the wire as one.
func TestLoadReturnsEmptyListsNeverNil(t *testing.T) {
	// A home with nothing in it at all: the case that produced the nulls.
	empty := t.TempDir()
	got := claudeconfig.Load(empty, t.TempDir())

	if got.Skills == nil {
		t.Fatal("Skills is nil and will marshal as null")
	}
	if got.Agents == nil {
		t.Fatal("Agents is nil and will marshal as null")
	}
	if got.Files == nil {
		t.Fatal("Files is nil and will marshal as null")
	}
	if got.Settings.Hooks == nil {
		t.Fatal("Settings.Hooks is nil and will marshal as null")
	}
	if got.Settings.AllowedTools == nil {
		t.Fatal("Settings.AllowedTools is nil and will marshal as null")
	}

	// The property that actually matters is what comes out of json.Marshal,
	// so assert on that rather than on the Go value alone.
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{
		`"skills":null`, `"agents":null`, `"files":null`, `"hooks":null`, `"allowedTools":null`,
	} {
		if strings.Contains(string(blob), forbidden) {
			t.Fatalf("payload contains %s, which every client has to special-case: %s", forbidden, blob)
		}
	}
	for _, want := range []string{`"skills":[]`, `"agents":[]`, `"files":[]`} {
		if !strings.Contains(string(blob), want) {
			t.Fatalf("payload is missing %s: %s", want, blob)
		}
	}
}
