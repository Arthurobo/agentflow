// Package engine defines the abstraction that lets agentd run both a Claude
// Code session and an OpenCode session through one lifecycle. One cockpit,
// two engines: a
// claude session and an OpenCode session are both real TUIs in a PTY, driven
// from mobile web, wrapped in a native agentflow control layer.
//
// Two interfaces sit on top of one another:
//
// - Engine decides HOW a session is launched: argv for the PTY spawn, the
// port the child will serve its control API on, and the per-run Control
// handle that owns the side channel (HTTP for OpenCode, PTY injection
// for Claude).
// - Control is the native control layer's backend: it lists/reads/sets
// models, agents, sessions, providers, MCP servers, skills and commands;
// it answers permission and question prompts; and it streams live
// events for ingest.
//
// Both engines are KindTTY at the spawner level — the terminal is the
// canvas. Only the argv builder and the Control implementation differ; the
// rest of agentd (lifecycle, persistence, ingest) is shared.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ID names an engine implementation. Stable strings — they go into the DB
// (managed_sessions.engine) and across the relay, so DO NOT rename without
// a migration.
const (
	IDClaude   = "claude"
	IDOpenCode = "opencode"
)

// ErrUnsupported is returned by Control methods an engine cannot satisfy
// natively (a Claude SetAgent on a Claude session can only inject a slash
// command; Agents() returns the empty list because the TUI doesn't enumerate
// agents). The control layer surfaces it as a greyed-out picker.
var ErrUnsupported = errors.New("engine: operation unsupported")

// Options is the engine-agnostic view of a spawn. It mirrors the relevant
// fields of spawner.Options without coupling either engine to that struct —
// a future engine (a third-party TUI, an experimental binary) only needs to
// satisfy this shape.
type Options struct {
	// Cwd scopes the process + permission workspace. Required.
	Cwd string
	// Project is a display label (defaults from Cwd).
	Project string
	// Model is the engine's model id (e.g. "claude-sonnet-4-5"; OpenCode uses
	// "provider/model"). "" = engine default.
	Model string
	// Agent selects the engine's named agent (Claude subagents, OpenCode
	// agents). "" = default.
	Agent string
	// Title names the session up front (TTY runs). Empty for "let the engine
	// decide".
	Title string
	// Prompt is the initial prompt — the TUI's first turn. Verified live on
	// both engines: claude takes it as the trailing positional, opencode as
	// --prompt. Neither needs the session API to receive its brief.
	Prompt string
	// ResumeSessionID continues an existing engine session.
	ResumeSessionID string
	// CreatedBy identifies the caller for auditing ("local" | "device:<id>").
	CreatedBy string
	// Cols/Rows (TTY only): the client terminal's real geometry so the PTY
	// is born at the size the viewer will actually render.
	Cols uint16
	Rows uint16
	// ControlPort is the port the host ALREADY allocated and recorded for
	// this run, for engines that serve a control API. BuildArgs must put
	// this exact port in the argv.
	//
	// It exists because the alternative is what happened: BuildArgs had no
	// way to learn the allocated port, so it allocated its OWN. The child
	// then listened on one port while the host polled another, the session
	// never bound, and every OpenCode run sat in "starting" forever. Zero
	// means "nothing was pre-allocated" and the engine may allocate.
	ControlPort int
}

// SpawnResult is what an engine hands back after launching its TUI process.
// The spawner wires it into its existing Proc model — TTY runs are universal.
type SpawnResult struct {
	// Cmd is the executable to spawn (absolute path preferred; Engine
	// implementations resolve via LookupClaude or their own lookup).
	Cmd string
	// Args is the argv (does NOT include the binary as args[0]).
	Args []string
	// Env is the environment to set; nil means "inherit, with the spawner's
	// standard scrubbing".
	Env []string
	// Cols/Rows is the geometry the PTY should be born at.
	Cols uint16
	Rows uint16
	// ControlPort is the local port the child serves its control API on
	// (OpenCode only; 0 for engines that don't expose one).
	ControlPort int
	// ControlBase is the base URL prefix for that API (e.g. "http://127.0.0.1:47312").
	ControlBase string
}

// Engine decides HOW a session is launched and how it is steered at runtime.
type Engine interface {
	// ID returns the stable engine identifier (see ID* constants).
	ID() string
	// BuildArgs assembles the argv for a PTY spawn (both engines are TTY).
	BuildArgs(opts Options) ([]string, error)
	// SpawnEnv returns the per-spawn environment entries to APPEND to the
	// scrubbed base (child-process identity, server binding, model, etc.).
	// nil = no overrides.
	SpawnEnv(opts Options) []string
	// NeedsControlPort reports whether the engine serves a side-channel API
	// on a local port. When true the spawner MUST allocate the port BEFORE
	// launching so it can be passed to the child; when false the port
	// allocation step is skipped.
	NeedsControlPort() bool
	// LookupBinary returns the absolute binary path (or an error). Engines
	// are free to honour AF_CLAUDE-style overrides when appropriate.
	LookupBinary() (string, error)
	// LookupVersion (best-effort) is used by /health; "" when unknown.
	LookupVersion(ctx context.Context) string
	// Control returns the per-run side-channel implementation for a live
	// session. Nil is a valid response for an engine that does not expose a
	// control layer at all (only KindTTY claude does, and even then
	// "thinly").
	//
	// baseURL is the resolved URL of the running engine's API (empty for
	// engines without one). port is the same port the spawner passed to the
	// child.
	Control(baseURL string) Control
}

// --- Control -----------------------------------------------------------------------

// Caps is the load-bearing signal that lets the UI render the SAME picker for
// both engines and grey out what the current engine cannot do. The control
// sheet reads this once per picker so a Claude "SetAgent" is shown
// as approximate and warns the user, while an OpenCode "SetAgent" is shown
// as exact.
type Caps struct {
	// ListModels / SetModel: read/write the active model.
	ListModels, SetModel bool
	// ListAgents / SetAgent: agents the engine exposes. (Claude: subagents
	// under .claude/agents; OpenCode: /agent.)
	ListAgents, SetAgent bool
	// ListCommands / RunCommand: slash commands the engine exposes.
	ListCommands, RunCommand bool
	// ListSkills: SKILL.md / .claude/skills surface.
	ListSkills bool
	// ListProviders / SetProvider: provider catalog + switcher (OpenCode).
	ListProviders, SetProvider bool
	// ListMCPServers: MCP server registry.
	ListMCPServers bool
	// Prompt sends a user message into the engine mid-run (Claude: stdin
	// envelope; OpenCode: POST /session/{id}/message).
	Prompt bool
	// Abort interrupts the current turn (OpenCode: POST /session/{id}/abort;
	// Claude: SIGINT-equivalent key sequence).
	Abort bool
	// ReplyPermission / ReplyQuestion: native answers to engine prompts
	// instead of typing a number into the TUI.
	ReplyPermission, ReplyQuestion bool
	// Events: SSE-style event stream (OpenCode: GET /event; Claude: hook
	// payloads via the existing path).
	Events bool
	// AttachFiles: the engine takes files as part of a user turn natively.
	// False does NOT mean images are impossible — it means the host has to
	// deliver them another way (Claude reads a path with its Read tool), so
	// this gates the delivery mechanism, not the feature.
	AttachFiles bool
}

// Attachment is one file the engineer attached to a turn. Path is a real
// path on THIS machine: the bytes were uploaded to the daemon and written to
// disk before any of this ran.
type Attachment struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Mime  string `json:"mime,omitempty"`
	Name  string `json:"name,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
}

// MarshalJSON renders Caps with the lowercase key names the frontend's zod
// schemas expect (caps.listModels, caps.setAgent, …). Without this every
// boolean would round-trip as PascalCase and the control sheet would see
// every capability as false.
func (c Caps) MarshalJSON() ([]byte, error) {
	type wire struct {
		ListModels      bool `json:"listModels"`
		SetModel        bool `json:"setModel"`
		ListAgents      bool `json:"listAgents"`
		SetAgent        bool `json:"setAgent"`
		ListCommands    bool `json:"listCommands"`
		RunCommand      bool `json:"runCommand"`
		ListSkills      bool `json:"listSkills"`
		ListProviders   bool `json:"listProviders"`
		SetProvider     bool `json:"setProvider"`
		ListMCPServers  bool `json:"listMcpServers"`
		Prompt          bool `json:"prompt"`
		Abort           bool `json:"abort"`
		ReplyPermission bool `json:"replyPermission"`
		ReplyQuestion   bool `json:"replyQuestion"`
		Events          bool `json:"events"`
		AttachFiles     bool `json:"attachFiles"`
	}
	// wire has exactly Caps' fields, so a conversion copies all of them; a
	// field added to one and not the other stops compiling here.
	return json.Marshal(wire(c))
}

// Model is a provider-aware model descriptor.
type Model struct {
	// ID is the engine-native id (claude: bare name; opencode: "prov/name").
	ID string `json:"id"`
	// DisplayName is the human-readable label; falls back to ID.
	DisplayName string `json:"displayName,omitempty"`
	// Provider is the owning provider (claude: "anthropic"; opencode: the
	// configured provider key). Empty when unknown.
	Provider string `json:"provider,omitempty"`
	// Default marks the engine's default choice.
	Default bool `json:"default"`
	// Status is the provider's own word for whether the model is usable
	// (opencode: active | disabled | error). Empty when the engine does not
	// say. This struct is the PROJECTION boundary for model listings: an
	// engine's raw entry may carry a live provider credential, and only the
	// fields named here ever reach a caller.
	Status string `json:"status,omitempty"`
}

// Agent is an agent descriptor.
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
	Default     bool   `json:"default"`
}

// Command is a slash command descriptor.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// ArgsHint names what the command expects after its name ("path",
	// "alias", "optional instructions"). Empty means the command takes no
	// argument, which is what lets the control sheet decide between running
	// it straight away and asking for one first.
	ArgsHint string `json:"argsHint,omitempty"`
	// Source says where the entry came from ("builtin" | "project" |
	// "user" | "skill"), so a catalog assembled from several places can be
	// read back.
	Source string `json:"source,omitempty"`
}

// Skill is a skill / SKILL.md descriptor.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path,omitempty"`
}

// Provider is an LLM provider descriptor (OpenCode).
type Provider struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// MCPServer is an MCP server descriptor.
type MCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"` // active | disabled | error
	URL    string `json:"url,omitempty"`
}

// PermissionRequest is one pending OpenCode permission prompt (V2 schema).
type PermissionRequest struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID,omitempty"`
	Tool      string         `json:"tool,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	Title     string         `json:"title,omitempty"`
	// Metadata preserves the raw shape for renderers that need it.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Question is one pending OpenCode question prompt.
type Question struct {
	ID        string           `json:"id"`
	SessionID string           `json:"sessionID,omitempty"`
	Header    string           `json:"header,omitempty"`
	Prompt    string           `json:"question,omitempty"`
	Options   []QuestionOption `json:"options,omitempty"`
}

// QuestionOption is a multiple-choice option on a Question prompt.
type QuestionOption struct {
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	Preview     string `json:"preview,omitempty"`
}

// SessionState is the live engine state for a single session.
type SessionState struct {
	Model    string `json:"model,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Raw is the full engine payload preserved for renderers that want more
	// than the three pinned fields above.
	Raw map[string]any `json:"raw,omitempty"`
}

// Control is the side-channel backend that backs the picker sheet.
type Control interface {
	// Capabilities returns which methods are real (vs returning ErrUnsupported).
	Capabilities() Caps

	Models(ctx context.Context) ([]Model, error)
	Agents(ctx context.Context) ([]Agent, error)
	Commands(ctx context.Context) ([]Command, error)
	Skills(ctx context.Context) ([]Skill, error)
	MCPServers(ctx context.Context) ([]MCPServer, error)
	Providers(ctx context.Context) ([]Provider, error)

	// Permissions / Questions are the live prompt streams the engine exposes
	// to its host. ReplyPermission / ReplyQuestion route the
	// user's choice back through the engine's native API. scope is
	// engine-defined (OpenCode: "session" | "always"; Claude: hook flow).
	Permissions(ctx context.Context) ([]PermissionRequest, error)
	ReplyPermission(ctx context.Context, id string, allow bool, scope string) error
	Questions(ctx context.Context) ([]Question, error)
	ReplyQuestion(ctx context.Context, id, answer string) error

	// State reads the engine's CURRENTLY ACTIVE model/agent/provider — the
	// "truth" the header chips always reconcile to.
	State(ctx context.Context) (SessionState, error)
	SetModel(ctx context.Context, model string) error
	SetAgent(ctx context.Context, agent string) error
	SetProvider(ctx context.Context, provider string) error
	RunCommand(ctx context.Context, name, args string) error

	// Prompt sends a user message into the engine mid-run.
	Prompt(ctx context.Context, text string) error
	// PromptWithFiles sends a user turn that carries files the engine should
	// see. An engine with no native file part returns ErrUnsupported, and
	// the host falls back to naming the paths in the text — which is a real
	// delivery for an engine that can read files, not a failure.
	PromptWithFiles(ctx context.Context, text string, files []Attachment) error
	// Abort interrupts the current turn (does not terminate the process).
	Abort(ctx context.Context) error

	// WaitReady blocks until the engine's control API answers health
	// checks, the spawn's TUI is reachable, or the budget elapses. The
	// spawner calls this AFTER launching the process and BEFORE persisting
	// the session as "running" — the OpenCode path relies on it to confirm
	// the HTTP server is actually up; the Claude path is a no-op.
	WaitReady(ctx context.Context, budget time.Duration) error
}

// ModelWaiter is implemented by controls whose model catalogue is not ready
// when their health endpoint is. It is optional: a caller asserts for it and
// falls back to Models, so an engine without the race needs nothing and pays
// nothing.
type ModelWaiter interface {
	WaitModels(ctx context.Context, budget time.Duration) ([]Model, error)
}

// SessionBinder is implemented by engines whose Control needs an explicit
// per-run session id to route calls (OpenCode: every per-session method
// includes /session/{id}/… in its path). The agentapi control layer calls
// BindSession with the persisted session id BEFORE the first per-session
// call so per-run methods work after a restart, when the spawn-time
// PostStart hook is no longer relevant.
type SessionBinder interface {
	BindSession(id string)
}

// PTYWriter is the raw keystroke channel into a live TUI process. It is the
// whole of what a PTY-driven Control can do: put bytes on the terminal, or
// submit one composed line.
type PTYWriter interface {
	// Write puts raw bytes on the PTY (control characters, key sequences).
	Write(b []byte) error
	// Submit sends one line as text-then-carriage-return, paced apart so
	// the TUI reads the CR as a submit rather than as a newline inside a
	// pasted chunk.
	Submit(text string) error
}

// PTYBinder is implemented by controls that are driven by keystroke
// injection rather than by an API (Claude Code: there is no control API, but
// every slash command it has is reachable by typing). The host calls BindPTY
// with the run's live PTY and cwd BEFORE the first call; a control with
// nothing bound answers ErrUnsupported, which is the honest answer for a run
// whose process is gone.
type PTYBinder interface {
	BindPTY(w PTYWriter, cwd string)
}

// ResolveID normalises a caller-supplied engine id to its canonical form,
// returning IDClaude ("claude") for empty / unknown values. This is the
// "default to what we know" behavior every existing row relies on.
func ResolveID(s string) string {
	switch s {
	case IDOpenCode:
		return IDOpenCode
	default:
		return IDClaude
	}
}
