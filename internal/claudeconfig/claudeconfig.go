package claudeconfig

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// File is one effective rule source for the running engine, ready for the
// "what rules are actually in effect" page.
type File struct {
	Path   string `json:"path"`
	Origin string `json:"origin"` // "settings.json" | "CLAUDE.md" | "agent" | "skill" | "hook"
	Loaded bool   `json:"loaded"`
	Bytes  int    `json:"bytes"`
	// Excerpt is the first ~512 chars so the UI can preview without dragging
	// the full file across the wire.
	Excerpt string `json:"excerpt,omitempty"`
}

// Hook is one entry from settings.json's hooks block (PreToolUse etc.).
type Hook struct {
	Event   string `json:"event"`
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
	// FireCount is filled from the agentd-side hook events the corpus
	// tailer / hooksrv already see; 0 here means "never observed this
	// session".
	FireCount int64 `json:"fireCount"`
}

// Skill is one .claude/skills/* entry.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path"`
}

// Agent is one .claude/agents/* entry.
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path"`
	// Model is optional — only present when the agent markdown front-matter
	// declares it.
	Model string `json:"model,omitempty"`
}

// Settings is ~/.claude.json + ~/.claude/settings.json merged (settings.json
// wins per Anthropic's documented merge order).
type Settings struct {
	Model          string   `json:"model,omitempty"`
	PermissionMode string   `json:"permissionMode,omitempty"`
	AllowedTools   []string `json:"allowedTools,omitempty"`
	Hooks          []Hook   `json:"hooks,omitempty"`
}

// Summary is the engine-agnostic "what rules are in effect" payload.
type Summary struct {
	// Files are the CLAUDE.md + settings.json + .claude.json file paths
	// that apply to cwd, in precedence order.
	Files []File `json:"files"`
	// Settings is the merged effective settings for cwd.
	Settings Settings `json:"settings"`
	// Skills is the global .claude/skills list (not cwd-scoped).
	Skills []Skill `json:"skills"`
	// Agents is the global .claude/agents list.
	Agents []Agent `json:"agents"`
}

// SettingsJSON is a minimal decode of ~/.claude/settings.json — only the
// fields the config surface cares about. Unknown fields are ignored.
type SettingsJSON struct {
	Model          string                 `json:"model,omitempty"`
	PermissionMode string                 `json:"permissionMode,omitempty"`
	AllowedTools   []string               `json:"allowedTools,omitempty"`
	Hooks          map[string][]HookEntry `json:"hooks,omitempty"`
}

// ClaudeJSON is ~/.claude.json (machine-scoped settings, same shape as
// settings.json). Per Anthropic docs, settings.json wins on conflict.
type ClaudeJSON = SettingsJSON

// HookEntry is one {matcher, hooks} pair (the hooks field is an array of
// {type, command}).
type HookEntry struct {
	Matcher string       `json:"matcher,omitempty"`
	Hooks   []HookInline `json:"hooks"`
}

// HookInline is one inner hook spec (type + command).
type HookInline struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// Load reads the engine-agnostic Claude config surface from home + cwd and
// returns the merged summary. cwd may be empty (global-only view).
func Load(home, cwd string) Summary {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	out := Summary{
		Files:    []File{},
		Skills:   []Skill{},
		Agents:   []Agent{},
		Settings: Settings{},
	}
	// 1. ~/.claude.json (machine-wide defaults; lowest precedence)
	var claudeJSON SettingsJSON
	if path := filepath.Join(home, ".claude.json"); exists(path) {
		out.Files = append(out.Files, readFileMeta(path, "settings.json"))
		_ = readJSON(path, &claudeJSON)
	}
	// 2. ~/.claude/settings.json (per-user, overrides .claude.json)
	var userJSON SettingsJSON
	if path := filepath.Join(home, ".claude", "settings.json"); exists(path) {
		out.Files = append(out.Files, readFileMeta(path, "settings.json"))
		_ = readJSON(path, &userJSON)
	}
	// 3. ~/.claude/CLAUDE.md (per-user memory, loaded by every session)
	for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md"} {
		path := filepath.Join(home, ".claude", name)
		if exists(path) {
			out.Files = append(out.Files, readFileMeta(path, "CLAUDE.md"))
		}
	}
	// 4. cwd-scoped CLAUDE.md / CLAUDE.local.md (project-scoped, overrides user)
	if cwd != "" {
		for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md"} {
			path := filepath.Join(cwd, name)
			if exists(path) {
				out.Files = append(out.Files, readFileMeta(path, "CLAUDE.md"))
			}
		}
	}
	// 5. .claude/CLAUDE.md (parent-directory walk — gitignore-style precedence)
	if cwd != "" {
		for dir := cwd; dir != "" && dir != "/"; dir = filepath.Dir(dir) {
			for _, name := range []string{".claude/CLAUDE.md", ".claude/CLAUDE.local.md"} {
				path := filepath.Join(dir, name)
				if exists(path) {
					out.Files = append(out.Files, readFileMeta(path, "CLAUDE.md"))
				}
			}
		}
	}
	// merge settings: user wins over claudeJSON
	out.Settings = mergeSettings(claudeJSON, userJSON)
	// skills + agents (global, not cwd-scoped)
	// loadSkills/loadAgents return nil when the directory is absent, and a
	// nil Go slice marshals as `null` rather than `[]`. That null is what
	// broke the config page for every machine without ~/.claude/skills. The
	// zero value of "a list of skills" is an empty list; say so.
	out.Skills = nonNil(loadSkills(filepath.Join(home, ".claude", "skills")))
	out.Agents = nonNil(loadAgents(filepath.Join(home, ".claude", "agents")))
	if out.Files == nil {
		out.Files = []File{}
	}
	if out.Settings.Hooks == nil {
		out.Settings.Hooks = []Hook{}
	}
	if out.Settings.AllowedTools == nil {
		out.Settings.AllowedTools = []string{}
	}
	return out
}

// LoadForSession returns the cwd-scoped summary for a managed session's
// stored CWD. Falls back to the global view if the row is missing or
// unreadable.
func LoadForSession(cwd string) Summary {
	return Load("", cwd)
}

func readFileMeta(path, origin string) File {
	out := File{Path: path, Origin: origin, Loaded: false}
	data, err := os.ReadFile(path) //nolint:gosec // path is built from the user's home
	if err != nil {
		return out
	}
	out.Loaded = true
	out.Bytes = len(data)
	if len(data) > 512 {
		out.Excerpt = string(data[:512]) + "\n…[truncated]"
	} else {
		out.Excerpt = string(data)
	}
	return out
}

func readJSON(path string, into any) error {
	data, err := os.ReadFile(path) //nolint:gosec // path is built from the user's home
	if err != nil {
		return err
	}
	return json.Unmarshal(data, into)
}

func mergeSettings(lower, upper SettingsJSON) Settings {
	out := Settings{}
	// upper (settings.json) wins on every field
	if upper.Model != "" {
		out.Model = upper.Model
	} else {
		out.Model = lower.Model
	}
	if upper.PermissionMode != "" {
		out.PermissionMode = upper.PermissionMode
	} else {
		out.PermissionMode = lower.PermissionMode
	}
	out.AllowedTools = append([]string(nil), lower.AllowedTools...)
	out.AllowedTools = append(out.AllowedTools, upper.AllowedTools...)
	for event, entries := range lower.Hooks {
		for _, e := range entries {
			for _, h := range e.Hooks {
				out.Hooks = append(out.Hooks, Hook{
					Event:   event,
					Matcher: e.Matcher,
					Command: h.Command,
				})
			}
		}
	}
	for event, entries := range upper.Hooks {
		for _, e := range entries {
			for _, h := range e.Hooks {
				out.Hooks = append(out.Hooks, Hook{
					Event:   event,
					Matcher: e.Matcher,
					Command: h.Command,
				})
			}
		}
	}
	// stable order so the UI renders the same each poll
	sort.SliceStable(out.Hooks, func(i, j int) bool {
		if out.Hooks[i].Event != out.Hooks[j].Event {
			return out.Hooks[i].Event < out.Hooks[j].Event
		}
		if out.Hooks[i].Matcher != out.Hooks[j].Matcher {
			return out.Hooks[i].Matcher < out.Hooks[j].Matcher
		}
		return out.Hooks[i].Command < out.Hooks[j].Command
	})
	return out
}

// SkillsIn lists the .claude/skills entries under dir (one directory per
// skill, each holding a SKILL.md). Exported so other packages can scan a
// project-scoped skills directory with the same reader rather than growing a
// second parser for the same file format.
func SkillsIn(dir string) []Skill { return loadSkills(dir) }

// nonNil turns a nil slice into an empty one so it marshals as [] and not
// null. Every list this package hands out is a list, even when it is empty.
func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

func loadSkills(dir string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(dir, e.Name())
		// SKILL.md is the conventional filename (Anthropic docs).
		skillPath := filepath.Join(skillDir, "SKILL.md")
		if !exists(skillPath) {
			continue
		}
		out = append(out, Skill{
			Name:        e.Name(),
			Description: headAfter(skillPath, "description:"),
			Path:        skillPath,
		})
	}
	return out
}

func loadAgents(dir string) []Agent {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Agent
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body := readFile(path)
		name := strings.TrimSuffix(e.Name(), ".md")
		out = append(out, Agent{
			Name:        name,
			Description: headAfter(path, "description:"),
			Path:        path,
			Model:       FrontMatterField(body, "model"),
		})
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readFile(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // path is built from the user's home
	if err != nil {
		return ""
	}
	return string(data)
}

// headAfter finds the FIRST occurrence of `key:` in the file's first 80
// lines and returns the trimmed value after it. Used for the description
// line that conventional SKILL.md / agent.md files lead with.
func headAfter(path, key string) string {
	f, err := os.Open(path) //nolint:gosec // path is built from the user's home
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewReader(f)
	for i := 0; i < 80; i++ {
		line, err := scanner.ReadString('\n')
		if err != nil {
			break
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key) {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, key))
			val = strings.Trim(val, "\"'`")
			return val
		}
	}
	return ""
}

// FrontMatterField reads YAML front-matter `key: value` from a markdown
// file. Returns a for absent / malformed front-matter.
func FrontMatterField(body, key string) string {
	if !strings.HasPrefix(body, "---") {
		return ""
	}
	end := strings.Index(body[3:], "\n---")
	if end < 0 {
		return ""
	}
	front := body[3 : 3+end]
	for _, line := range strings.Split(front, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key+":") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), key+":"))
		}
	}
	return ""
}
