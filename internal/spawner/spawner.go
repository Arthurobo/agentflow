// Package spawner owns the lifecycle of the engine processes agentd
// supervises: interactive TUIs in a PTY, one-shot `claude -p` runs and
// persistent chat sessions over stream-json, signal routing, crash
// supervision with --resume recovery, and event intake through the shared
// single-writer ingest pipeline.
//
// Headless sessions never prompt interactively: a denied tool
// surfaces as a system/permission_denied stream event + result.permission_denials
// — the spawner records denials and marks the run "awaiting" so the UI can
// surface them; it does NOT build an interactive permission path.
package spawner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
	"github.com/arthurobo/agentflow/internal/claudelog/parse"
	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/store"
)

// Kind is the claude session flavour.
type Kind string

const (
	// KindOneShot runs `claude -p "<prompt>" --output-format stream-json
	// --verbose`: one turn, exits after the result event.
	KindOneShot Kind = "one_shot"
	// KindChat runs `claude -p --input-format stream-json ...`: stdin stays
	// open for user turns as {"type":"user","message":{...}} envelopes
	//; ends cleanly on stdin close.
	KindChat Kind = "chat"
	// KindTTY runs the real interactive `claude` TUI inside a PTY: no -p, no
	// stream-json — raw terminal bytes in/out (the tty hub owns the master).
	KindTTY Kind = "tty"
)

// spawnEnvDropKeys are host-environment variables that leak into agentd and
// silently decide the spawned claude process's terminal capability detection.
// agentd is a daemon launched from arbitrary contexts (systemd, automation
// shells, SSH without a TTY): a stray TERM=dumb or NO_COLOR/CI in ITS
// environment made the TUI emit zero color codes — the "black and white
// terminal" bug. The spawner therefore strips them from the inherited base
// and, for TTY children, pins a real color-capable terminal identity.
var spawnEnvDropKeys = map[string]struct{}{
	"TERM":           {},
	"COLORTERM":      {},
	"NO_COLOR":       {},
	"FORCE_COLOR":    {},
	"CLICOLOR":       {},
	"CLICOLOR_FORCE": {},
	"CI":             {},

	// Claude Code's OWN session-identity variables. These are set by a
	// running claude process for its children, and they MUST NOT reach a
	// session agentd spawns on a user's behalf.
	//
	// agentd is frequently (re)started from inside a Claude Code session —
	// an agent restarting the service does exactly that — and it inherits
	// that session's environment. Passing it on told every spawned claude
	// "you are a CHILD of session <parent-uuid>", so the child adopted the
	// parent's identity and never created its own transcript. The observable
	// symptom: a session started from the phone works perfectly and answers
	// questions, but writes no ~/.claude/projects/**/*.jsonl, so session-id
	// discovery never resolves, the run is stuck in `starting` forever, and
	// it is invisible in the transcript-derived session list — while the
	// conversation exists only in that process's memory and dies with it.
	//
	// Stripping them makes a spawned session a genuine top-level session
	// regardless of where agentd was launched from. Pinned by
	// TestChildSpawnEnvStripsParentClaudeSession.
	"CLAUDECODE":                   {},
	"CLAUDE_CODE_CHILD_SESSION":    {},
	"CLAUDE_CODE_SESSION_ID":       {},
	"CLAUDE_CODE_ENTRYPOINT":       {},
	"CLAUDE_CODE_EXECPATH":         {},
	"CLAUDE_CODE_MESSAGING_SOCKET": {},
	"CLAUDE_CODE_MESSAGING_TOKEN":  {},
	"CLAUDE_PID":                   {},
	"CLAUDE_EFFORT":                {},
}

// childEnvDropPrefixes are whole namespaces a child never inherits.
//
// AF_ is agentflow's own configuration: the daemon's knobs, its remote mode,
// its paths. None of it is the child's business. TS_ is Tailscale's, and
// includes TS_AUTHKEY: a key that joins a machine to the owner's tailnet,
// which a dropped-by-name list let straight through to every agent and every
// command an agent runs.
var childEnvDropPrefixes = []string{"AF_", "TS_"}

// ChildSpawnEnv builds the environment for a spawned claude process: base
// minus the capability-detection and session-identity variables above and
// the AF_/TS_ namespaces, plus — for TTY children only — TERM/COLORTERM
// declaring a truecolor xterm. extra (deliberate per-run overrides) always
// lands last with the final word. Pinned by
// TestChildSpawnEnvStripsAgentflowAndTailscaleEnv.
func ChildSpawnEnv(base []string, kind Kind, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra)+2)
next:
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 {
			name := kv[:i]
			if _, drop := spawnEnvDropKeys[name]; drop {
				continue
			}
			for _, prefix := range childEnvDropPrefixes {
				if strings.HasPrefix(name, prefix) {
					continue next
				}
			}
		}
		out = append(out, kv)
	}
	if kind == KindTTY {
		out = append(out, "TERM=xterm-256color", "COLORTERM=truecolor")
	}
	return append(out, extra...)
}

// State values for the spawner state machine.
//
// State is the spawner state machine. Relationships:
//
//	starting -> running (system/init captured the claude session id)
//	running -> awaiting (a tool was denied; headless auto-denies and the
//	                       model may continue — awaiting is the UI's prompt to
//	                       steer or stop; cleared on the next model activity)
//	running|awaiting -> finished (clean exit, result seen)
//	running|awaiting -> stopped (user-requested SIGINT / stdin close)
//	starting|running|awaiting -> crashed (abnormal exit; resumable via --resume)
//
// finished/stopped/crashed are terminal. crashed runs may be resumed.
type State string

// State values of the spawner state machine.
const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateAwaiting State = "awaiting"
	StateFinished State = "finished"
	StateStopped  State = "stopped"
	StateCrashed  State = "crashed"
)

// Terminal reports whether a state is terminal (no more process activity).
func (s State) Terminal() bool {
	switch s {
	case StateFinished, StateStopped, StateCrashed:
		return true
	}
	return false
}

// Options configures one spawned session.
type Options struct {
	Kind Kind
	// Cwd scopes the process + permission workspace.
	Cwd string
	// Project is a display label (defaults from Cwd).
	Project string
	// Model overrides the claude model ("" = default).
	Model string
	// Agent selects the engine's named agent (Claude subagents; OpenCode
	// agents). Empty = engine default. The Claude path surfaces this via
	// --append-system-prompt; the OpenCode path forwards it as --agent.
	Agent string
	// AppendSystemPrompt, when non-empty, is passed to claude as
	// --append-system-prompt. It is the engine's loop-constitution summary
	// and lives one rung above the positional first-prompt — rules and
	// identity survive auto-compact better here. Empty = omit.
	AppendSystemPrompt string
	// AllowedTools is the --allowedTools allowlist (scoped autonomy; the safe
	// alternative to --dangerously-skip-permissions). Comma-separated upstream.
	AllowedTools []string
	// PermissionMode: "acceptEdits" default; also "default", "plan", "auto".
	// NOTE: manual aliases default.
	PermissionMode string
	// Effort is the reasoning effort for models that support it ("" = default).
	Effort string
	// Prompt is the initial prompt: one-shot's single turn, chat's first turn.
	Prompt string
	// Title names the session up front (tty runs): written into the session
	// JSONL as a custom-title record — the same format claude itself uses —
	// so the sessions list shows the user's name immediately.
	Title string
	// ResumeSessionID resumes an existing session with --resume <uuid>.
	ResumeSessionID string
	// ResumeTitle resumes by title (--resume "<title>"),
	ResumeTitle string
	// AddDirs grants extra --add-dir permission workspaces.
	AddDirs []string
	// AdditionalArgs are appended verbatim after the standard ones.
	AdditionalArgs []string
	// ExtraEnv appends KEY=VALUE entries to the child env.
	ExtraEnv []string
	// CreatedBy identifies the caller for auditing ("local" | "device:<id>").
	CreatedBy string
	// ApprovalsEnabled routes this run's PreToolUse calls through the approval
	// hook.
	ApprovalsEnabled bool
	// ExcludeSessionIDs (tty kind only): claude session ids that session-id
	// discovery must NEVER claim. Loop members spawn back-to-back in the SAME
	// cwd — without this, member B's poll finds member A's fresh transcript
	// and both runs end up bound to one session (the identity-collision bug).
	ExcludeSessionIDs []string
	// Cols/Rows (tty kind only): the client terminal's real geometry so the
	// PTY is born at the size the viewer will actually render — claude's very
	// first output (the --resume transcript replay) is then wrapped correctly
	// instead of at 80×24 and re-broken by xterm (the mangled-scrollback bug).
	// Zero values mean "unknown": defaults apply, plus last-known remembered
	// geometry for resumed sessions.
	Cols uint16
	Rows uint16
	// InitTimeout bounds how long a spawn call waits for system/init (default 12s).
	InitTimeout time.Duration
	// Engine is the dual-engine identity. Empty / "claude" preserves the
	// existing TTY TUI path; "opencode" routes through the engine registry
	// (spawn argv differs; a control port is allocated and recorded on the
	// session row). This field is the spawn-time counterpart of the
	// managed_sessions.engine column (migration 0015).
	Engine string
	// ControlPort is the local loopback port the OpenCode TUI serves its
	// control API on (set by the spawner when Engine == "opencode"; 0
	// otherwise).
	ControlPort int
	// ControlBase is the resolved base URL for that API
	// (e.g. http://127.0.0.1:47312).
	ControlBase string
	// ControlSessionID is the engine's session id (OpenCode: pre-created via
	// POST /session; Claude: empty until corpus discovery).
	ControlSessionID string
	// PostStart is an optional hook fired once after the TUI process has
	// started. It receives the live PTY size and is the right place to bind
	// an OpenCode session id (POST /session then POST /tui/select-session)
	// AFTER WaitReady succeeds — the engine seam needs this to keep the
	// control sheet honest without coupling the spawner to engine APIs.
	// Errors are logged, never fatal.
	PostStart func(ctx context.Context, runID string, sess *Session, opts Options)
}

// Session is the public view of a managed run (mirrors store.ManagedSession
// plus live process info). ID is the agentd run key; SessionID is the claude
// session id discovered from system/init.
type Session struct {
	ID               string  `json:"id"`
	SessionID        string  `json:"sessionId,omitempty"`
	Kind             Kind    `json:"kind"`
	CWD              string  `json:"cwd,omitempty"`
	Project          string  `json:"project,omitempty"`
	Model            string  `json:"model,omitempty"`
	Prompt           string  `json:"prompt,omitempty"`
	ResumeFrom       string  `json:"resumeFrom,omitempty"`
	State            State   `json:"state"`
	Blocked          bool    `json:"blocked,omitempty"` // awaiting input after denial
	PID              int     `json:"pid,omitempty"`
	StartedAt        int64   `json:"startedAt,omitempty"`
	EndedAt          int64   `json:"endedAt,omitempty"`
	ExitCode         int     `json:"exitCode,omitempty"`
	EventCount       int64   `json:"eventCount"`
	PermissionDenial int64   `json:"permissionDenials,omitempty"`
	TotalCostUSD     float64 `json:"totalCostUsd,omitempty"`
	TerminalReason   string  `json:"terminalReason,omitempty"`
	LastError        string  `json:"lastError,omitempty"`
	// StopReason is why the run was stopped (user_stop, orphaned, ...), read
	// from the persisted row. Empty while alive and for runs that ended on
	// their own.
	StopReason string `json:"stopReason,omitempty"`
	CreatedBy  string `json:"createdBy,omitempty"`
	UpdatedAt  int64  `json:"updatedAt,omitempty"`
	// ApprovalsEnabled: whether this run's PreToolUse calls route to the
	// approval hook.
	ApprovalsEnabled bool `json:"approvalsEnabled,omitempty"`
	// Title is the user's chosen session name (mirrors spawner.Options.Title
	// — populated so the engine seam's PostStart hook can pass it to
	// OpenCode's POST /session as the session title).
	Title string `json:"title,omitempty"`
	// Engine is the dual-engine identity (Plan / migration 0015).
	Engine string `json:"engine,omitempty"`
	// ControlPort / ControlBase identify the OpenCode control API endpoint
	// (0 / "" for non-OpenCode runs).
	ControlPort int    `json:"controlPort,omitempty"`
	ControlBase string `json:"controlBase,omitempty"`
	// ControlSessionID is the engine's session id (OpenCode: bound by the
	// PostStart hook; Claude: empty until corpus discovery).
	ControlSessionID string `json:"controlSessionId,omitempty"`
}

// Child is the OS-level process handle the spawner drives. Stdin is only used
// by chat sessions (stream-json writer); Stdout is expected to be the
// stream-json NDJSON stream; Waitch delivers the process exit error. Signal,
// when set, is used instead of Process.Signal (tests inject a recorder).
type Child struct {
	Process *os.Process
	Stdin   io.WriteCloser
	Stdout  io.ReadCloser
	Stderr  io.ReadCloser
	Waitch  chan error
	Signal  func(sig os.Signal) error
}

// Runner abstracts process creation so tests can substitute a fake claude that
// replays a golden stream-json fixture and captures written stdin.
type Runner interface {
	// Start launches claude with the given argv, working directory and env.
	// It must return as soon as the process is running, with a goroutine that
	// sends the eventual exit error to Waitch when the process dies.
	Start(argv []string, cwd string, env []string) (Child, error)
}

// ExecRunner is the default Runner: exec.Command("claude", ...) with pipes.
type ExecRunner struct {
	// Path is the claude binary (default "claude", resolved by LookPath).
	Path string
}

var _ Runner = (*ExecRunner)(nil)

// LookupClaude returns the absolute claude binary path, or an error.
func LookupClaude(path string) (string, error) {
	if path == "" {
		path = "claude"
	}
	if !strings.Contains(path, "/") {
		if p, err := exec.LookPath(path); err == nil {
			return p, nil
		}
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("claude binary %q: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// Start launches claude with argv and wires the three pipes.
func (r *ExecRunner) Start(argv []string, cwd string, env []string) (Child, error) {
	bin, err := LookupClaude(r.Path)
	if err != nil {
		return Child{}, err
	}
	//nolint:gosec // G204: the spawner intentionally launches the user-configured claude binary with a built argv.
	cmd := exec.Command(bin, argv...)
	cmd.Dir = cwd
	cmd.Env = ChildSpawnEnv(os.Environ(), "", env)
	// The kill unit is the process group: the child leads its own (pgid ==
	// pid), so kill(-pgid) reaches every descendant it spawned.
	cmd.SysProcAttr = childSysProcAttr()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Child{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Child{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Child{}, err
	}
	if err := cmd.Start(); err != nil {
		return Child{}, err
	}
	out := &eofReader{r: stdout, eof: make(chan struct{})}
	child := Child{
		Process: cmd.Process,
		Stdin:   stdin,
		Stdout:  out,
		Stderr:  stderr,
		Waitch:  make(chan error, 1),
	}
	exited := waitExited(cmd.Process.Pid)
	go func() {
		// Wait closes the stdout pipe the moment it reaps the child, and
		// whatever the child wrote last is still sitting in that pipe: the
		// result event, usually. Calling Wait straight away threw it away
		// whenever the pump was a line behind. So Wait comes after stdout
		// reaches EOF, or, when a grandchild keeps stdout open after the
		// child itself has exited, a short drain after that exit.
		select {
		case <-out.eof:
		case <-exited:
			t := time.NewTimer(outputDrainGrace)
			select {
			case <-out.eof:
			case <-t.C:
			}
			t.Stop()
		}
		child.Waitch <- cmd.Wait()
	}()
	return child, nil
}

// outputDrainGrace is how long a headless child's stdout may stay open after
// the child exited before Wait closes it anyway. Only a descendant still
// holding the pipe keeps it open that long.
const outputDrainGrace = 2 * time.Second

// eofReader reports when a child's stdout has been read to its end.
type eofReader struct {
	r    io.ReadCloser
	eof  chan struct{}
	once sync.Once
}

func (e *eofReader) Read(b []byte) (int, error) {
	n, err := e.r.Read(b)
	if err != nil {
		e.once.Do(func() { close(e.eof) })
	}
	return n, err
}

func (e *eofReader) Close() error {
	e.once.Do(func() { close(e.eof) })
	return e.r.Close()
}

// Spawner manages the lifecycle of all claude children agentd supervises.
type Spawner struct {
	st     *store.Store
	ing    *ingest.Service
	runner Runner
	log    *slog.Logger

	// engines is the dual-engine registry. When opts.Engine is
	// non-empty, the spawner looks up the matching engine to read its
	// BuildArgs (argv) and SpawnEnv (per-spawn env overrides). When
	// opts.Engine is empty (legacy callers), the spawner falls back to
	// the claude argv produced by buildArgs() and the claude binary
	// resolved by LookupClaude.
	engines *engine.Registry

	// Generation is a ULID minted at agentd boot (see New / SetGeneration).
	// Every new managed row gets stamped with it; the boot reconciler uses
	// it to tell "this run was spawned by THIS agentd life" from "this run
	// was spawned by a previous agentd life and is now an orphan".
	generation string

	mu      sync.Mutex
	procs   map[string]*Proc
	binPath string
	// corpusRoot overrides ~/.claude/projects for session-id discovery.
	// Tests point it at a temp dir so a spawn can never touch the real corpus.
	corpusRoot string
	now        func() int64

	// ttyRegistrar is the hook the spawner calls with the PTY master as
	// soon as a TUI process exists, so the tty hub drains and fans out its
	// output even when no WebSocket is attached. Without it an unattended
	// loop member blocks once the PTY buffer fills, because nobody reads
	// the master. Guarded by mu: it is installed after the spawner exists.
	ttyRegistrar func(runID string, master io.ReadWriteCloser, resize func(cols, rows uint16) error, geomCols uint16)

	// postStart is the optional engine-aware hook fired by StartTTY after
	// the TUI process is up. See SetPostStart / Options.PostStart. Guarded
	// by mu.
	postStart func(ctx context.Context, runID string, sess *Session, opts Options)

	// geomMu guards lastGeom: the last client-confirmed terminal geometry per
	// CLAUDE session id. ResizeTTY records it; tty spawns with an unknown or
	// zero geometry but a known ResumeSessionID reuse it, so unattended
	// auto-revives (loop supervision) also re-open at the viewer's real size.
	geomMu   sync.Mutex
	lastGeom map[string][2]uint16
}

// SetEngines wires the dual-engine registry into the spawner so start() and
// StartTTY can pick the right argv/binary/env for opts.Engine. Nil is safe
// (the spawner falls back to the legacy claude path).
func (s *Spawner) SetEngines(r *engine.Registry) {
	s.engines = r
}

// argvFor returns the per-engine argv for opts. The legacy buildArgs()
// produces claude's TTY argv (--name, --dangerously-skip-permissions,
// etc); engine.BuildArgs returns the engine's own argv (opencode:
// --port, --hostname, optional --session, cwd at the tail).
func (s *Spawner) argvFor(opts Options) ([]string, error) {
	if s.engines != nil && opts.Engine != "" {
		if eng, err := s.engines.Get(opts.Engine); err == nil {
			return eng.BuildArgs(engine.Options{
				Cwd:             opts.Cwd,
				Model:           opts.Model,
				Agent:           opts.Agent,
				Title:           opts.Title,
				Prompt:          opts.Prompt,
				ResumeSessionID: opts.ResumeSessionID,
				// The port the host reserved for this run. Without it the
				// engine allocates a second one and the child ends up
				// listening somewhere nobody is looking.
				ControlPort: opts.ControlPort,
			})
		}
	}
	return buildArgs(opts)
}

// binaryFor returns the per-engine binary path. For OpenCode the engine
// resolves its own binary; for Claude (or any other engine without a
// LookupBinary implementation) we fall back to LookupClaude.
func (s *Spawner) binaryFor(opts Options) (string, error) {
	if s.engines != nil && opts.Engine != "" {
		if eng, err := s.engines.Get(opts.Engine); err == nil {
			return eng.LookupBinary()
		}
	}
	return LookupClaude(s.binPath)
}

// envFor returns the per-spawn env entries to APPEND to the scrubbed base:
// the engine's own entries, then the caller's ExtraEnv, which has the final
// word either way.
func (s *Spawner) envFor(opts Options) []string {
	if s.engines != nil && opts.Engine != "" {
		if eng, err := s.engines.Get(opts.Engine); err == nil {
			return append(eng.SpawnEnv(engine.Options{
				Cwd:             opts.Cwd,
				Model:           opts.Model,
				Agent:           opts.Agent,
				Title:           opts.Title,
				Prompt:          opts.Prompt,
				ResumeSessionID: opts.ResumeSessionID,
			}), opts.ExtraEnv...)
		}
	}
	return opts.ExtraEnv
}

// rememberGeom stores the geometry under the claude session id (when known).
func (s *Spawner) rememberGeom(sessionID string, cols, rows uint16) {
	if sessionID == "" || cols == 0 || rows == 0 {
		return
	}
	s.geomMu.Lock()
	if s.lastGeom == nil {
		s.lastGeom = map[string][2]uint16{}
	}
	s.lastGeom[sessionID] = [2]uint16{cols, rows}
	s.geomMu.Unlock()
}

// rememberedGeom looks up last-known geometry for a claude session id.
func (s *Spawner) rememberedGeom(sessionID string) (uint16, uint16, bool) {
	if sessionID == "" {
		return 0, 0, false
	}
	s.geomMu.Lock()
	defer s.geomMu.Unlock()
	g, ok := s.lastGeom[sessionID]
	return g[0], g[1], ok
}

// New builds a Spawner; runner may be nil for the default ExecRunner (the
// claude binary is resolved lazily on first spawn). A fresh agentd
// generation is minted on construction — see SetGeneration for the boot
// reconciler flow.
func New(st *store.Store, ing *ingest.Service, log *slog.Logger) *Spawner {
	if log == nil {
		log = slog.Default()
	}
	return &Spawner{
		st:         st,
		ing:        ing,
		log:        log,
		procs:      map[string]*Proc{},
		lastGeom:   map[string][2]uint16{},
		now:        func() int64 { return time.Now().UnixMilli() },
		generation: newGeneration(),
	}
}

// SetCorpusRoot points session-id discovery (and the title write that follows
// it) at a corpus other than ~/.claude/projects. Every test that spawns through
// a real Spawner sets this to a temp dir: with the default root a fake-claude
// spawn bound itself to the operator's live transcripts and renamed them.
func (s *Spawner) SetCorpusRoot(root string) {
	s.mu.Lock()
	s.corpusRoot = root
	s.mu.Unlock()
}

// corpusRootDir is the folder session-id discovery may look under. Empty
// means "unknown", and discovery treats that as "nowhere".
func (s *Spawner) corpusRootDir() string {
	s.mu.Lock()
	root := s.corpusRoot
	s.mu.Unlock()
	if root != "" {
		return root
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// SetGeneration overrides the boot generation. Tests pin a stable value;
// the boot reconciler reads it back to know "this row's generation is
// current" vs "this row's generation is from a previous life".
func (s *Spawner) SetGeneration(g string) {
	s.mu.Lock()
	s.generation = g
	s.mu.Unlock()
}

// SetTTYRegistrar installs the hook StartTTY calls with the PTY master. The
// agentapi tty hub uses it to drain and fan out output before any WebSocket
// attaches; unattended loop members depend on it.
func (s *Spawner) SetTTYRegistrar(fn func(runID string, master io.ReadWriteCloser, resize func(cols, rows uint16) error, geomCols uint16)) {
	s.mu.Lock()
	s.ttyRegistrar = fn
	s.mu.Unlock()
}

// registrar returns the installed tty registrar, nil when there is none.
func (s *Spawner) registrar() func(runID string, master io.ReadWriteCloser, resize func(cols, rows uint16) error, geomCols uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttyRegistrar
}

// Generation returns the current agentd boot id (a ULID).
func (s *Spawner) Generation() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// SetRunner swaps the process runner (tests). Runner() returns the default
// ExecRunner when unset.
func (s *Spawner) SetRunner(r Runner) { s.runner = r }

// SetPostStart registers the engine-aware post-start hook fired by TTY
// spawns (see Options.PostStart). The spawner does not interpret the hook —
// it is purely an extension point for engine-specific binding (the
// OpenCode seam uses it to POST /session after the TUI comes up).
func (s *Spawner) SetPostStart(fn func(ctx context.Context, runID string, sess *Session, opts Options)) {
	s.mu.Lock()
	s.postStart = fn
	s.mu.Unlock()
}

// postStart is the registered hook (nil when no engine seam is wired).
func (s *Spawner) postStartFn() func(ctx context.Context, runID string, sess *Session, opts Options) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.postStart
}

// Runner returns the configured runner (default ExecRunner).
func (s *Spawner) Runner() Runner {
	if s.runner != nil {
		return s.runner
	}
	return &ExecRunner{Path: s.binPath}
}

// SetClaudePath pins the claude binary used by the default runner.
func (s *Spawner) SetClaudePath(path string) {
	if p, err := LookupClaude(path); err == nil {
		s.binPath = p
	}
}

// ClaudePath returns the resolved claude binary path ("" if unset).
func (s *Spawner) ClaudePath() string {
	if s.binPath != "" {
		return s.binPath
	}
	if p, err := LookupClaude("claude"); err == nil {
		return p
	}
	return ""
}

// Version runs `claude --version` (used by doctor; best-effort).
func (s *Spawner) Version(ctx context.Context) string {
	bin := s.ClaudePath()
	if bin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	//nolint:gosec // G204: reading the pinned claude binary's version string.
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Proc is the live process wrapper for one run.
type Proc struct {
	sess   *Session
	sessMu sync.Mutex // guards all fields of sess (pump writes vs readers)
	child  Child
	cancel context.CancelFunc
	// done is closed once the pump has recorded the run's final state.
	done chan struct{}
	// exited is closed as soon as the process has exited and been reaped,
	// which can be long before the pump gets round to finishing. exitErr
	// is the Wait result and may only be read after exited is closed. A
	// nil exited (a Proc assembled by hand) means "not known to be gone".
	exited    chan struct{}
	exitErr   error
	stopped   atomic.Bool // user-requested interrupt/close-stdin (Stop semantics)
	stdinMu   sync.Mutex  // serializes stream-json envelope writes (chat mode)
	ptyMaster *os.File    // tty kind only: the PTY master (agentd tty hub reads it)
}

// newProc wraps a started child and begins watching for its exit.
func newProc(sess *Session, child Child, cancel context.CancelFunc) *Proc {
	p := &Proc{sess: sess, child: child, cancel: cancel,
		done: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		switch {
		case child.Waitch != nil:
			p.exitErr = <-child.Waitch
		case child.Process != nil:
			p.exitErr = child.Process.Release()
		}
		close(p.exited)
	}()
	return p
}

// hasExited reports whether the process is known to have exited.
func (p *Proc) hasExited() bool {
	if p.exited == nil {
		return false
	}
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// exitSignal is the channel that closes when the process is gone: exited
// when it is being watched, done otherwise.
func (p *Proc) exitSignal() <-chan struct{} {
	if p.exited != nil {
		return p.exited
	}
	return p.done
}

// signal delivers sig to a live child and to the process group it leads.
//
// It never signals a child already known to have exited: once reaped, its
// pid is free for the kernel to hand to an unrelated process. The leader is
// signalled through the os.Process handle, which is pidfd-backed on Linux and
// so cannot reach a recycled pid; the group signal carries it to the child's
// own descendants. pgid 0 means the group is unknown.
func (p *Proc) signal(sig syscall.Signal, pgid int) error {
	if p.hasExited() {
		return os.ErrProcessDone
	}
	if p.child.Signal != nil {
		return p.child.Signal(sig)
	}
	var err error
	if p.child.Process != nil {
		err = p.child.Process.Signal(sig)
	}
	if pgid > 0 {
		if kerr := syscall.Kill(-pgid, sig); kerr != nil && err == nil {
			err = kerr
		}
	}
	return err
}

// update applies fn to the live session under the per-process lock.
func (p *Proc) update(fn func(s *Session)) {
	p.sessMu.Lock()
	defer p.sessMu.Unlock()
	fn(p.sess)
}

// snapshot returns a copy of the live session under the per-process lock.
func (p *Proc) snapshot() *Session {
	p.sessMu.Lock()
	defer p.sessMu.Unlock()
	c := *p.sess
	return &c
}

// buildArgs assembles the claude argv for an Options. Ordering is load-bearing:
// the one-shot prompt must sit IMMEDIATELY after `-p` (the
// verified pattern); chat mode has NO positional prompt (the pump sends the
// first user turn via the stdin envelope, ). Value flags
// (--mcp-config etc.) after the prompt parse unreliably — verified live.
func buildArgs(opts Options) ([]string, error) {
	if opts.Kind == "" {
		opts.Kind = KindOneShot
	}
	// tty: the genuine TUI — no -p, no stream-json wrappers; the PTY carries
	// the whole interactive session (input bytes in, rendered TUI out). The
	// optional initial prompt rides as the FINAL POSITIONAL argument after all
	// flags: claude (2.1.239) processes it as the session's first turn
	// immediately at boot — no typing
	// into the PTY, no paste-mangling of long briefs.
	if opts.Kind == KindTTY {
		var args []string
		if opts.Title != "" {
			// claude's own (hidden) session-name flag — the name shows in the
			// TUI and `claude -r` exactly as if the user named it there
			args = append(args, "--name", opts.Title)
		}
		args = append(args, commonArgs(opts)...)
		if opts.Prompt != "" {
			args = append(args, opts.Prompt)
		}
		return args, nil
	}
	args := []string{"-p"}
	switch opts.Kind {
	case KindChat:
		args = append(args, "--input-format", "stream-json")
	case KindOneShot:
		args = append(args, opts.Prompt)
	default:
		return nil, fmt.Errorf("spawner: unknown kind %q", opts.Kind)
	}
	// NOTE: chat mode intentionally has NO positional prompt —
	// verified the first user turn arrives via the stdin envelope.
	args = append(args, "--output-format", "stream-json", "--verbose")
	args = append(args, commonArgs(opts)...)
	return args, nil
}

// commonArgs assembles the flags shared by every kind: resume, model,
// allowlist, permission mode, effort, add-dirs, extras (chat/one-shot append
// them after the prompt+stream-json block; tty has nothing before them).
func commonArgs(opts Options) []string {
	var args []string
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	} else if opts.ResumeTitle != "" {
		args = append(args, "--resume", opts.ResumeTitle)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.AppendSystemPrompt != "" {
		// loop-constitution summary lives in the system-prompt zone so it
		// survives auto-compact and competes with user input for primary
		// instruction status. Placed BEFORE the positional prompt at the
		// spawn boundary.
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	if len(opts.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(opts.AllowedTools, ","))
	}
	pm := opts.PermissionMode
	if pm == "" {
		// Product default: full autonomy on every run — managed sessions get
		// everything a normal interactive session has, no denials, no prompts
		// a headless process could never answer.
		pm = "bypassPermissions"
	}
	if pm == "bypassPermissions" {
		// The canonical CLI spelling of full autonomy.
		args = append(args, "--dangerously-skip-permissions")
	} else {
		args = append(args, "--permission-mode", pm)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	for _, d := range opts.AddDirs {
		args = append(args, "--add-dir", d)
	}
	args = append(args, opts.AdditionalArgs...)
	return args
}

// NewRunID generates a run key (exposed so the agentd API can pre-create the id
// before a background spawn returns).
func NewRunID() string { return newUUID() }

// Start spawns a new session. It returns as soon as the process is running;
// the run key is stable immediately. InitTimeout (default 12 s) bounds how long
// Start waits for system/init to reveal the claude session id; after it the run
// stays "starting" and the pump records the id when init lands.
func (s *Spawner) Start(ctx context.Context, opts Options) (*Session, error) {
	return s.start(ctx, NewRunID(), opts)
}

// StartWithID is Start with a caller-supplied run key (the agentd API uses it
// to answer 202 before the background spawn settles).
func (s *Spawner) StartWithID(ctx context.Context, runID string, opts Options) (*Session, error) {
	return s.start(ctx, runID, opts)
}

func (s *Spawner) start(ctx context.Context, runID string, opts Options) (*Session, error) {
	// tty runs live in a PTY, not exec pipes — a wholly different start path.
	if opts.Kind == KindTTY {
		return s.StartTTY(ctx, runID, opts)
	}
	if opts.Cwd == "" {
		var err error
		opts.Cwd, err = os.Getwd()
		if err != nil {
			opts.Cwd = "."
		}
	}
	if opts.Project == "" && opts.Cwd != "" {
		opts.Project = eventmodel.ProjectFromCWD(opts.Cwd)
	}
	args, err := s.argvFor(opts)
	if err != nil {
		return nil, err
	}
	child, err := s.Runner().Start(args, opts.Cwd, s.envFor(opts))
	if err != nil {
		// persist a crashed record so a failed background spawn is visible in
		// the run list instead of silently vanishing.
		now := s.now()
		crash := &Session{
			ID: runID, Kind: opts.Kind, CWD: opts.Cwd, Project: opts.Project,
			Model: opts.Model, Prompt: opts.Prompt, State: StateCrashed,
			StartedAt: now, EndedAt: now, CreatedBy: opts.CreatedBy,
			ApprovalsEnabled: opts.ApprovalsEnabled,
			Engine:           resolveEngineID(opts.Engine),
			ControlPort:      opts.ControlPort,
			ControlBase:      opts.ControlBase,
			LastError:        err.Error(),
		}
		_ = s.persistInitial(crash)
		return crash, fmt.Errorf("spawner: start claude: %w", err)
	}

	now := s.now()
	sess := &Session{
		ID:               runID,
		Kind:             opts.Kind,
		CWD:              opts.Cwd,
		Project:          opts.Project,
		Model:            opts.Model,
		Prompt:           opts.Prompt,
		State:            StateStarting,
		PID:              child.Process.Pid,
		StartedAt:        now,
		UpdatedAt:        now,
		CreatedBy:        opts.CreatedBy,
		ApprovalsEnabled: opts.ApprovalsEnabled,
		Engine:           resolveEngineID(opts.Engine),
		ControlPort:      opts.ControlPort,
		ControlBase:      opts.ControlBase,
	}
	if opts.ResumeSessionID != "" {
		sess.ResumeFrom = opts.ResumeSessionID
	} else if opts.ResumeTitle != "" {
		sess.ResumeFrom = opts.ResumeTitle
	}

	pctx, cancel := context.WithCancel(context.Background())
	p := newProc(sess, child, cancel)
	s.mu.Lock()
	s.procs[runID] = p
	s.mu.Unlock()

	// persist before the pump so a crash during intake keeps a record.
	_ = s.persistInitial(sess)

	go s.pump(pctx, p, sess, opts)

	// fire-and-forget stderr drain so a verbose child writing to stderr never
	// blocks on a full pipe.
	go func() { _, _ = io.Copy(io.Discard, child.Stderr) }()

	initTimeout := opts.InitTimeout
	if initTimeout <= 0 {
		initTimeout = 12 * time.Second
	}
	initCtx, initCancel := context.WithTimeout(ctx, initTimeout)
	defer initCancel()
	for {
		select {
		case <-initCtx.Done():
			return p.snapshot(), nil
		case <-p.done:
			return p.snapshot(), nil // exited before init (crash path)
		default:
		}
		if cur := p.snapshot(); cur.SessionID != "" {
			return cur, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// userEnvelope renders a user-turn envelope with the exact
// upstream field order: {"type":"user","message":{"role":"user","content":
// [{"type":"text","text":"..."}]}}. Go's map marshaling sorts keys, so the
// envelope is assembled by hand (text still JSON-escaped).
func userEnvelope(text string) string {
	var b strings.Builder
	b.WriteString(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":`)
	tb, _ := json.Marshal(text)
	b.Write(tb)
	b.WriteString(`}]}}` + "\n")
	return b.String()
}

// SendMessage injects a user turn into a chat session ( envelope:
// {"type":"user","message":{"role":"user","content":[...]}}). Not supported on
// one-shot runs.
func (s *Spawner) SendMessage(ctx context.Context, runID, text string) error {
	p, sess, err := s.proc(ctx, runID)
	if err != nil {
		return err
	}
	if sess.Kind != KindChat {
		return fmt.Errorf("spawner: session %s is %s, mid-run messages need a chat session", runID, sess.Kind)
	}
	if sess.State.Terminal() {
		return fmt.Errorf("spawner: session %s is %s, cannot send messages", runID, sess.State)
	}
	p.stdinMu.Lock()
	_, werr := io.WriteString(p.child.Stdin, userEnvelope(text))
	p.stdinMu.Unlock()
	if werr != nil {
		return fmt.Errorf("spawner: write message: %w", werr)
	}
	// a user reply clears the awaiting/blocked marker
	p.update(func(sc *Session) { s.setState(sc, StateRunning, "") })
	return p.persist(s, "")
}

// Interrupt routes the raw signal semantics of Stop: it sends the given signal
// to the claude process (SIGINT default), which claude interprets as "stop the
// current turn" ( — closing stdin is the clean exit 0; SIGINT
// interrupts the in-flight answer without killing the session).
func (s *Spawner) Interrupt(ctx context.Context, runID string, sig os.Signal) error {
	p, sess, err := s.proc(ctx, runID)
	if err != nil {
		return err
	}
	if sess.State.Terminal() {
		return fmt.Errorf("spawner: session %s already %s", runID, sess.State)
	}
	if sig == nil {
		sig = os.Interrupt
	}
	if p.child.Signal != nil {
		if err := p.child.Signal(sig); err != nil {
			return fmt.Errorf("spawner: signal %v: %w", sig, err)
		}
	} else if err := p.child.Process.Signal(sig); err != nil {
		return fmt.Errorf("spawner: signal %v: %w", sig, err)
	}
	p.stopped.Store(true)
	s.log.Info("spawner: interrupt routed", "run", runID, "pid", sess.PID, "sig", sig)
	return nil
}

// Wait blocks until the session's process exits (terminal). It returns the
// final session.
func (s *Spawner) Wait(ctx context.Context, runID string) (*Session, error) {
	p, _, err := s.proc(ctx, runID)
	if err != nil {
		if m, serr := s.st.GetManagedSession(ctx, runID); serr == nil && m != nil {
			return s.fromStore(m), nil
		}
		return nil, err
	}
	select {
	case <-p.done:
		return p.snapshot(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Remove forgets a terminal run (the managed row is kept for history).
func (s *Spawner) Remove(runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.procs[runID]; ok {
		p.cancel()
	}
	delete(s.procs, runID)
	return nil
}

// stopGrace is how long terminate waits between SIGTERM and SIGKILL. Short
// on purpose — we want fast cancellation for the engine's loop-cancel
// path, but a non-zero grace lets the child run any signal handlers /
// atexit hooks so a well-behaved claude can flush before we escalate.
const stopGrace = 1500 * time.Millisecond

// terminateConfirmTimeout bounds the final "is the process actually gone
// yet" poll after the SIGKILL. Long enough for the kernel to reap a
// sigkilled process; short enough to fail loudly if something is wrong.
const terminateConfirmTimeout = 10 * time.Second

// terminate is the SINGLE kill path for any run, alive or orphaned. The
// escalation order is:
//
// 1. Resolve target — in-memory proc if present, else the persisted
// row (so an orphan from a previous agentd life is also killable).
// 2. Identity-check — read /proc/<pid>/stat and confirm field 22
// (starttime) matches the stored proc_start_ticks. A mismatch means
// the PID was reused by an unrelated process; we mark the row gone
// and return without signaling.
// 3. Mark `stopped.Store(true)` so the live pump records StateStopped,
// not StateCrashed.
// 4. Close the right fd by kind (PTY master or chat stdin) for the
// graceful-EOF nudge.
// 5. SIGTERM to the PROCESS GROUP (kill(-pgid)) so claude's children
// are notified too. Fall back to the direct pid if pgid is 0 (e.g.
// a process started before Setpgid landed in the codebase).
// 6. After stopGrace, SIGKILL to the group.
// 7. Poll identity until gone (bounded by terminateConfirmTimeout).
// 8. Persist state=stopped + ended_at + stop_reason.
//
// No-op (returns nil) for already-terminal sessions. The caller can
// safely call terminate twice.
func (s *Spawner) terminate(ctx context.Context, runID, reason string) error {
	if reason == "" {
		reason = "user_stop"
	}

	// 1. resolve target
	p, sess, alive := s.resolveTarget(ctx, runID)
	if sess != nil && sess.State != "" {
		// already terminal
		if isTerminalState(string(sess.State)) {
			return nil
		}
	}
	if !alive {
		// no in-memory proc AND no persisted row — truly gone
		return nil
	}

	// 2. identity check (only meaningful when the row was loaded from
	// the store rather than the live in-memory procs map)
	pgid, identityValid := s.identityCheck(p, sess)
	if p != nil && p.hasExited() {
		// Already gone: the pump is recording how it ended. Signalling a
		// reaped pid could reach whatever the kernel handed it to next.
		p.stopped.Store(true)
		go s.awaitFinal(p, reason) //nolint:gosec // G118: records the final row after the caller's request has returned
		return nil
	}
	if !identityValid {
		// The row names a PID that is gone, or that now belongs to an
		// unrelated process. Such a row must be marked gone and NEVER
		// signaled — but the escalation below signals unconditionally, and
		// for a recycled PID identityCheck returns the STRANGER's pgid, so
		// the SIGTERM and the SIGKILL 1.5s later both land on kill(-pgid)
		// of a process group we do not own. confirmDead then recorded
		// "_unverified", whose comment says we never signaled it.
		//
		// This is the path every orphan from a previous agentd life takes,
		// and reaping a restart's worth of them is exactly when PIDs are
		// most likely to have been reused. Record the row terminal and
		// touch no process.
		s.persistTerminated(sess, reason, false)
		return nil
	}

	// 3. mark stopped so the live pump records the right final state
	if p != nil {
		p.stopped.Store(true)
	}

	// 4. close fd by kind
	if p != nil {
		switch sess.Kind {
		case KindChat:
			if p.child.Stdin != nil {
				p.stdinMu.Lock()
				_ = p.child.Stdin.Close()
				p.stdinMu.Unlock()
			}
		case KindTTY:
			if p.ptyMaster != nil {
				_ = p.ptyMaster.Close()
			}
		}
	}

	// 5+6. SIGTERM the group, then SIGKILL after grace. The whole
	// escalation runs in a goroutine so the caller's REST handler
	// returns immediately. The engine has already flipped the loop row
	// to a transitional state before calling, so the UI is consistent
	// even if the kill takes the full grace.
	s.terminateAsync(p, sess, pgid, identityValid, reason)
	return nil
}

func (s *Spawner) terminateAsync(p *Proc, sess *Session, pgid int, identityValid bool, reason string) {
	// pick the kill target. pgid > 0 means the child leads its own group:
	// kill the group (neg pid = group). Otherwise fall back to the pid.
	target := sess.PID
	if pgid > 0 {
		target = -pgid
	}

	// Graceful signal: SIGTERM to the whole target, NOT SIGINT to the bare
	// pid. Two reasons this matters for a claude TUI:
	// - SIGINT is what Ctrl-C sends; the TUI treats it as "cancel the
	// current turn" and stays alive, so the graceful phase never
	// actually worked and every kill waited out the full grace.
	// - signaling only sess.PID leaves claude's own children (bash, MCP
	// servers, subagents) running (the kill unit is the group).
	if p != nil {
		if err := p.signal(syscall.SIGTERM, pgid); err != nil {
			s.log.Info("spawner: terminate SIGTERM (best-effort)", "run", sess.ID, "target", target, "err", err)
		}
		go func() {
			timer := time.NewTimer(stopGrace)
			defer timer.Stop()
			select {
			case <-p.exitSignal():
				s.awaitFinal(p, reason)
				return
			case <-timer.C:
			}
			if err := p.signal(syscall.SIGKILL, pgid); err != nil {
				s.log.Info("spawner: terminate SIGKILL (best-effort)", "run", sess.ID, "target", target, "err", err)
			} else {
				s.log.Info("spawner: terminate escalated to SIGKILL", "run", sess.ID, "target", target)
			}
			s.awaitFinal(p, reason)
		}()
		return
	}

	// An orphan: no pump and no handle, only a pid the identity check has
	// vouched for. Grace, then SIGKILL, then poll until it is gone.
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil { //nolint:gosec // target is a pid or negative pgid
		s.log.Info("spawner: terminate SIGTERM (best-effort)", "run", sess.ID, "target", target, "err", err)
	}
	go func() {
		time.Sleep(stopGrace)
		if err := syscallKill(target); err != nil {
			s.log.Info("spawner: terminate SIGKILL (best-effort)", "run", sess.ID, "target", target, "err", err)
		} else {
			s.log.Info("spawner: terminate escalated to SIGKILL", "run", sess.ID, "target", target)
		}
		s.confirmDead(sess, identityValid, reason)
	}()
}

// awaitFinal waits for a live run's pump to record how the process ended,
// then adds why it was stopped.
//
// The pump's row is the truth: it carries the exit code, the event count and
// the cost as they were at the very end. Writing the snapshot taken when the
// stop was requested over it (what this used to do) threw all of that away.
// Only when the pump never finishes is a row written from here, and then from
// the freshest snapshot there is.
func (s *Spawner) awaitFinal(p *Proc, reason string) {
	timer := time.NewTimer(terminateConfirmTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
		if s.st != nil {
			if err := s.st.SetManagedStopReason(context.Background(), p.snapshot().ID, reason); err != nil {
				s.log.Warn("spawner: record stop reason", "run", p.snapshot().ID, "err", err)
			}
		}
	case <-timer.C:
		fin := p.snapshot()
		s.log.Warn("spawner: terminate could not confirm the run finished", "run", fin.ID, "pid", fin.PID,
			"waited", terminateConfirmTimeout.String())
		s.persistTerminated(fin, reason, true)
	}
}

// resolveTarget returns the live Proc + Session if the run is in memory
// AND the row exists. Falls back to the persisted row when the in-memory
// procs map is empty (orphan — the supervisor restarted). Returns
// (nil, nil, false) when the row is gone too.
func (s *Spawner) resolveTarget(ctx context.Context, runID string) (*Proc, *Session, bool) {
	s.mu.Lock()
	p, ok := s.procs[runID]
	s.mu.Unlock()
	if ok {
		return p, p.snapshot(), true
	}
	if s.st == nil {
		return nil, nil, false
	}
	m, err := s.st.GetManagedSession(ctx, runID)
	if err != nil || m == nil {
		return nil, nil, false
	}
	// build a synthetic Session from the row so the rest of the path
	// can use it without in-memory state.
	sess := &Session{
		ID: m.ID, SessionID: m.SessionID, Kind: Kind(m.Kind),
		CWD: m.CWD, Project: m.Project, Model: m.Model,
		State: State(m.State), PID: m.PID, ExitCode: m.ExitCode,
		StopReason: m.StopReason,
	}
	return nil, sess, true
}

// identityCheck returns the (pgid, ok) pair from /proc/<pid>/stat and
// confirms the stored proc_start_ticks still matches. ok=false means
// "the process at this pid is NOT the one we spawned" — usually PID
// reuse, sometimes our own process death. The caller treats that as
// "no signal; just mark gone".
func (s *Spawner) identityCheck(p *Proc, sess *Session) (int, bool) {
	if p != nil {
		// A live child agentd started leads its own process group, so its
		// pgid is its pid; no /proc read, and the handle it is signalled
		// through cannot be confused with a recycled pid.
		return sess.PID, true
	}
	// orphan path: must verify
	pgid, ticks := ReadProcIdentity(sess.PID)
	if pgid == 0 && ticks == 0 {
		// /proc/<pid>/stat was unreadable; the process is gone (it
		// can't be running if we couldn't read its stat). Mark gone.
		return 0, false
	}
	if stored, ok := s.lookupStoredIdentity(sess); ok && stored != 0 && ticks != stored {
		// PID was reused — DO NOT SIGNAL
		return pgid, false
	}
	return pgid, true
}

func (s *Spawner) lookupStoredIdentity(sess *Session) (int64, bool) {
	if s.st == nil {
		return 0, false
	}
	m, err := s.st.GetManagedSession(context.Background(), sess.ID)
	if err != nil || m == nil {
		return 0, false
	}
	return m.ProcStartTicks, true
}

// confirmDead polls /proc/<pid>/stat until the process is gone (ESRCH),
// then persists the terminal state. Bounded by terminateConfirmTimeout.
func (s *Spawner) confirmDead(sess *Session, identityValid bool, reason string) {
	if !identityValid {
		s.persistTerminated(sess, reason, false)
		return
	}
	deadline := time.Now().Add(terminateConfirmTimeout)
	for time.Now().Before(deadline) {
		if !ProcessAlive(sess.PID) {
			s.persistTerminated(sess, reason, true)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Timed out with the process still alive. Persist the terminal state
	// anyway so the loop can settle, but say so loudly — a member that
	// survives SIGKILL is either an unkillable-state kernel task or a pgid
	// we got wrong, and both need a human.
	s.log.Warn("spawner: terminate could not confirm death", "run", sess.ID, "pid", sess.PID,
		"waited", terminateConfirmTimeout.String())
	s.persistTerminated(sess, reason, true)
}

// persistTerminated writes state=stopped + ended_at + stop_reason for
// runs terminate() got to the end for. Best-effort: a write failure here
// just means the sweep sees a stale "running" one more cycle.
func (s *Spawner) persistTerminated(sess *Session, reason string, confirmed bool) {
	if s.st == nil {
		return
	}
	now := s.now()
	if !confirmed {
		// `confirmed == false` means the identity check failed: the pid is
		// either gone or now belongs to an unrelated process. Either way OUR
		// process no longer exists, so the row IS terminal and must be
		// written as such — with a distinct reason so the audit trail shows
		// we never signaled it.
		//
		// Returning early here (the previous behavior) left the row
		// `running` forever: nothing else flips it outside the boot
		// reconciler, so the loop stayed in `cancelling` and the sweep
		// re-confirmed death on a corpse every 15s, for the life of the
		// daemon.
		reason += "_unverified"
	}
	_ = s.st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: sess.ID, SessionID: sess.SessionID, Kind: string(sess.Kind),
		CWD: sess.CWD, Project: sess.Project, Model: sess.Model,
		Prompt: sess.Prompt, ResumeFrom: sess.ResumeFrom,
		State: string(StateStopped), PID: sess.PID,
		StartedAt: sess.StartedAt, EndedAt: now, ExitCode: sess.ExitCode,
		EventCount: sess.EventCount, PermissionDenial: sess.PermissionDenial,
		TotalCostUSD: sess.TotalCostUSD, TerminalReason: sess.TerminalReason,
		LastError: sess.LastError, CreatedBy: sess.CreatedBy, UpdatedAt: now,
		StopReason: reason,
		Engine:     sess.Engine, ControlPort: sess.ControlPort, ControlBase: sess.ControlBase,
	})
	s.log.Info("spawner: terminate confirmed", "run", sess.ID, "reason", reason, "elapsed_ms", now-sess.StartedAt)
}

// syscallKill sends SIGKILL to a pid, or to a whole process group when
// target is a negative pgid.
func syscallKill(target int) error {
	return syscall.Kill(target, syscall.SIGKILL) //nolint:gosec // target is either a positive pid or negative pgid
}

// isTerminalState reports whether the spawner-state column already
// reflects a non-running outcome.
func isTerminalState(s string) bool {
	switch s {
	case string(StateFinished), string(StateStopped), string(StateCrashed):
		return true
	}
	return false
}

// Stop is the public name for terminate — loop cancel, run UI button, the
// reconciler all call this. Backwards-compatible with the prior Stop
// signature.
func (s *Spawner) Stop(ctx context.Context, runID string) error {
	return s.terminate(ctx, runID, "user_stop")
}

// Reap terminates a run left behind by a PREVIOUS agentd life. It is Stop's path exactly — one termination primitive —
// and differs only in the reason it records, so the audit trail separates
// "a person stopped this" from "a restart found it running and nobody
// owned it".
//
// The identity check inside terminate does the load-bearing work here: an
// orphan's PID is usually gone or recycled, and a row whose process cannot
// be verified is written terminal with reason "orphaned_unverified" rather
// than being signaled.
func (s *Spawner) Reap(ctx context.Context, runID string) error {
	return s.terminate(ctx, runID, "orphaned")
}

// StopAll stops every live run and waits for them to exit.
//
// Every run gets the normal stop (SIGTERM, then SIGKILL after the grace)
// at once, and StopAll then waits for the processes to actually be gone.
// When ctx's deadline arrives first, whatever is still running is
// SIGKILLed with its process group, so a daemon that is shutting down never
// leaves a child behind just because the child ignored SIGTERM. The error
// names how many had to be killed that way.
func (s *Spawner) StopAll(ctx context.Context) error {
	s.mu.Lock()
	procs := make(map[string]*Proc, len(s.procs))
	for id, p := range s.procs {
		procs[id] = p
	}
	s.mu.Unlock()
	var firstErr error
	for id := range procs {
		if err := s.terminate(ctx, id, "shutdown"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	all := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, p := range procs {
			wg.Add(1)
			go func(p *Proc) {
				defer wg.Done()
				select {
				case <-p.exitSignal():
				case <-ctx.Done():
				}
			}(p)
		}
		wg.Wait()
		close(all)
	}()
	select {
	case <-all:
		if ctx.Err() == nil {
			return firstErr
		}
	case <-ctx.Done():
	}
	<-all
	killed := 0
	for id, p := range procs {
		if p.hasExited() {
			continue
		}
		select {
		case <-p.done:
			continue
		default:
		}
		sess := p.snapshot()
		if err := p.signal(syscall.SIGKILL, sess.PID); err != nil && err != os.ErrProcessDone {
			s.log.Warn("spawner: shutdown SIGKILL", "run", id, "pid", sess.PID, "err", err)
		}
		killed++
	}
	if killed > 0 {
		return fmt.Errorf("spawner: %d run(s) did not exit before the shutdown deadline and were killed", killed)
	}
	return firstErr
}

// Kill is a hard-only escape hatch; it always goes straight to SIGKILL
// (no grace), for when the grace is unacceptable.
func (s *Spawner) Kill(ctx context.Context, runID string) error {
	p, sess, alive := s.resolveTarget(ctx, runID)
	if !alive {
		return nil
	}
	if isTerminalState(string(sess.State)) {
		return nil
	}
	if p != nil {
		p.stopped.Store(true)
	}
	pgid, identityValid := s.identityCheck(p, sess)
	if !identityValid {
		s.persistTerminated(sess, "user_kill", false)
		return nil
	}
	if p != nil {
		if err := p.signal(syscall.SIGKILL, pgid); err != nil && err != os.ErrProcessDone {
			return fmt.Errorf("spawner: kill %s: %w", runID, err)
		}
		go s.awaitFinal(p, "user_kill") //nolint:gosec // G118: records the final row after the caller's request has returned
		return nil
	}
	target := sess.PID
	if pgid > 0 {
		target = -pgid
	}
	if err := syscallKill(target); err != nil {
		return fmt.Errorf("spawner: kill %s: %w", runID, err)
	}
	go s.confirmDead(sess, identityValid, "user_kill")
	return nil
}

// CloseStdin is the chat-mode graceful path (: closing stdin
// is the clean exit 0). Kept on the public API for callers that don't
// want SIGTERM escalation; the engine's CancelLoop now prefers Stop
// (= terminate). TTY sessions get the same kind-aware close path.
func (s *Spawner) CloseStdin(ctx context.Context, runID string) error {
	p, sess, alive := s.resolveTarget(ctx, runID)
	if !alive {
		return nil
	}
	if isTerminalState(string(sess.State)) {
		return nil
	}
	if p != nil {
		p.stopped.Store(true)
	}
	switch sess.Kind {
	case KindChat:
		if p != nil && p.child.Stdin != nil {
			p.stdinMu.Lock()
			_ = p.child.Stdin.Close()
			p.stdinMu.Unlock()
		}
	case KindTTY:
		if p != nil && p.ptyMaster != nil {
			_ = p.ptyMaster.Close()
		}
	}
	return nil
}

// List returns copies of all managed sessions (live + persisted history),
// optionally filtered by state.
func (s *Spawner) List(ctx context.Context, state string) ([]*Session, error) {
	rows, err := s.st.ListManagedSessions(ctx, state, 200)
	if err != nil {
		return nil, err
	}
	out := make([]*Session, 0, len(rows))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range rows {
		sc := s.fromStore(m)
		if p, ok := s.procs[m.ID]; ok {
			sc = s.mergeLive(p.snapshot(), sc)
		}
		out = append(out, sc)
	}
	return out, nil
}

// Status returns the current session (live + persisted).
func (s *Spawner) Status(ctx context.Context, runID string) (*Session, error) {
	m, err := s.st.GetManagedSession(ctx, runID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	p, live := s.procs[runID]
	s.mu.Unlock()
	if m == nil && !live {
		return nil, os.ErrNotExist
	}
	if m == nil {
		return s.copySession(runID), nil
	}
	sc := s.fromStore(m)
	if live {
		sc = s.mergeLive(p.snapshot(), sc)
	}
	return sc, nil
}

// ListBySessionID finds the managed run whose claude session id matches.
func (s *Spawner) ListBySessionID(ctx context.Context, sid string) (*Session, error) {
	m, err := s.st.GetManagedSessionBySessionID(ctx, sid)
	if err != nil || m == nil {
		return nil, err
	}
	return s.Status(ctx, m.ID)
}

// --- internals ------------------------------------------------------------------

func (s *Spawner) proc(ctx context.Context, runID string) (*Proc, *Session, error) {
	s.mu.Lock()
	p, ok := s.procs[runID]
	if !ok {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("spawner: no live process for %s", runID)
	}
	sc := s.copySessionLocked(runID)
	s.mu.Unlock()
	return p, sc, nil
}

func (s *Spawner) copySession(runID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copySessionLocked(runID)
}

func (s *Spawner) copySessionLocked(runID string) *Session {
	p, ok := s.procs[runID]
	if !ok {
		return nil
	}
	return p.snapshot()
}

func (s *Spawner) setState(sc *Session, st State, lastErr string) {
	sc.State = st
	if st == StateAwaiting {
		sc.Blocked = true
	}
	if st != StateAwaiting && st != StateStarting {
		sc.Blocked = false
	}
	if lastErr != "" {
		sc.LastError = lastErr
	}
	if st.Terminal() && sc.EndedAt == 0 {
		sc.EndedAt = s.now()
	}
}

// persist writes the session row into managed_sessions under the per-process
// lock (reads the freshest state; the store commit is backpressured by the
// shared single writer).
func (p *Proc) persist(s *Spawner, lastErr string) error {
	p.sessMu.Lock()
	defer p.sessMu.Unlock()
	sc := p.sess
	if lastErr != "" {
		sc.LastError = lastErr
	}
	sc.UpdatedAt = s.now()
	err := s.st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: sc.ID, SessionID: sc.SessionID, Kind: string(sc.Kind), CWD: sc.CWD,
		Project: sc.Project, Model: sc.Model, Prompt: sc.Prompt, ResumeFrom: sc.ResumeFrom,
		State: string(sc.State), PID: sc.PID, StartedAt: sc.StartedAt, EndedAt: sc.EndedAt,
		ExitCode: sc.ExitCode, EventCount: sc.EventCount, PermissionDenial: sc.PermissionDenial,
		TotalCostUSD: sc.TotalCostUSD, TerminalReason: sc.TerminalReason,
		LastError: sc.LastError, CreatedBy: sc.CreatedBy, UpdatedAt: sc.UpdatedAt,
		ApprovalsEnabled: sc.ApprovalsEnabled,
		Engine:           sc.Engine, ControlPort: sc.ControlPort, ControlBase: sc.ControlBase,
	})
	if err != nil {
		s.log.Warn("spawner: persist managed session", "run", sc.ID, "err", err)
	}
	return err
}

// persistInitial writes the starting-session row before the pump starts (no
// concurrency yet — direct store write). The kill identity (pgid,
// proc_start_ticks, generation, stop_reason) is captured here so the
// boot reconciler and `terminate()` can reap orphans correctly across
// agentd restarts.
func (s *Spawner) persistInitial(sc *Session) error {
	sc.UpdatedAt = s.now()
	pgid, startTicks := ReadProcIdentity(sc.PID)
	return s.st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: sc.ID, SessionID: sc.SessionID, Kind: string(sc.Kind), CWD: sc.CWD,
		Project: sc.Project, Model: sc.Model, Prompt: sc.Prompt, ResumeFrom: sc.ResumeFrom,
		State: string(sc.State), PID: sc.PID, StartedAt: sc.StartedAt, EndedAt: sc.EndedAt,
		ExitCode: sc.ExitCode, EventCount: sc.EventCount, PermissionDenial: sc.PermissionDenial,
		TotalCostUSD: sc.TotalCostUSD, TerminalReason: sc.TerminalReason,
		LastError: sc.LastError, CreatedBy: sc.CreatedBy, UpdatedAt: sc.UpdatedAt,
		ApprovalsEnabled: sc.ApprovalsEnabled,
		Pgid:             pgid,
		ProcStartTicks:   startTicks,
		Generation:       s.Generation(),
		// The name the run was given, on the ROW rather than only in memory.
		// It used to reach a list only through appendCustomTitle writing a
		// Claude transcript record, which is a Claude mechanism — so an
		// OpenCode run had its name nowhere and showed as "Untitled".
		Title:  sc.Title,
		Engine: sc.Engine, ControlPort: sc.ControlPort, ControlBase: sc.ControlBase,
	})
}

// pump reads the child's stream-json stdout, normalizes it with the claudelog
// stream reader, enqueues batches through the shared single writer, updates
// run state, and finalizes on process exit.
//
// Parsing runs in its own goroutine and hands events over a channel, so
// everything the pump owns (the pending batch, the session meta) is touched
// by the pump goroutine alone. The periodic flush used to run on a ticker
// goroutine of its own and raced the pump for both.
func (s *Spawner) pump(ctx context.Context, p *Proc, sess *Session, opts Options) {
	if p.child.Stdin != nil && sess.Kind == KindChat && opts.Prompt != "" {
		// chat: the initial prompt is the first user envelope on stdin
		p.stdinMu.Lock()
		_, _ = io.WriteString(p.child.Stdin, userEnvelope(opts.Prompt))
		p.stdinMu.Unlock()
	}

	parsedC := make(chan []*eventmodel.Event, 16)
	go func() {
		defer close(parsedC)
		sp := parse.NewStreamParser(p.child.Stdout, parse.Options{RetainRaw: false})
		for {
			parsed, err := sp.Next()
			if err != nil {
				if err != io.EOF {
					s.log.Debug("spawner: stream parse", "run", sess.ID, "err", err)
				}
				return
			}
			parsedC <- parsed.Events
		}
	}()

	var pending []store.Incoming
	var meta *store.SessionMeta
	flush := func() {
		if len(pending) == 0 || meta == nil {
			return
		}
		if err := s.ing.EnqueueLive(ctx, pending, meta); err != nil {
			s.log.Warn("spawner: enqueue", "run", sess.ID, "err", err)
		}
		n := int64(len(pending))
		p.update(func(sc *Session) {
			sc.EventCount += n
			sc.UpdatedAt = s.now()
		})
		pending = nil
		_ = p.persist(s, "")
	}

	// The ticker drains pending events to the index while the child is
	// quiet between lines (live latency).
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
read:
	for {
		var events []*eventmodel.Event
		select {
		case <-ticker.C:
			flush()
			continue
		case evs, ok := <-parsedC:
			if !ok {
				break read
			}
			events = evs
		}
		for _, ev := range events {
			if ev.Type == eventmodel.EventSystemMeta && ev.Subtype == eventmodel.SubtypeInit {
				p.update(func(sc *Session) {
					if ev.SessionID != "" && sc.SessionID == "" {
						sc.SessionID = ev.SessionID
						s.setState(sc, StateRunning, "")
						sc.UpdatedAt = s.now()
					}
				})
			}
			if ev.Type == eventmodel.EventPermission {
				p.update(func(sc *Session) {
					sc.PermissionDenial++
					s.setState(sc, StateAwaiting, "")
					sc.UpdatedAt = s.now()
				})
			} else {
				// the model pressed on after a denial (headless auto-denies)
				p.update(func(sc *Session) {
					if sc.State == StateAwaiting {
						s.setState(sc, StateRunning, "")
						sc.UpdatedAt = s.now()
					}
				})
			}
			if meta == nil {
				meta = &store.SessionMeta{
					SessionID: ev.SessionID,
					CWD:       ev.CWD,
					Project:   ev.Project,
					FilePath:  "/agentd/" + sess.ID + ".jsonl",
				}
			}
			inc := ingest.ToIncoming(ev, meta.FilePath)
			if inc.SessionID == "" {
				inc.SessionID = ev.SessionID
			}
			pending = append(pending, inc)
			if ev.Type == eventmodel.EventResult {
				p.update(func(sc *Session) {
					sc.TotalCostUSD = ev.CostUSD
					if ev.TerminalReason != "" {
						sc.TerminalReason = ev.TerminalReason
					}
					if n := permissionDenialLength(ev.PermissionDenials); n > 0 && int64(n) > sc.PermissionDenial {
						sc.PermissionDenial = int64(n)
					}
					sc.UpdatedAt = s.now()
				})
				// a result ends a turn: flush the pending batch immediately
				flush()
			}
		}
		if len(pending) >= 64 {
			flush()
		}
		_ = p.persist(s, "")
	}
	flush()

	<-p.exited
	exitErr := p.exitErr
	exitCode := 0
	if ec, ok := exitErr.(interface{ ExitCode() int }); ok {
		exitCode = ec.ExitCode()
	} else if exitErr != nil {
		exitCode = -1
	}

	stopping := p.stopped.Load()

	p.update(func(sc *Session) {
		sc.ExitCode = exitCode
		switch {
		case exitCode != 0 && !stopping:
			s.setState(sc, StateCrashed, fmt.Sprintf("claude exited with code %d", exitCode))
		case sc.State == StateStarting:
			s.setState(sc, StateCrashed, "process ended before system/init")
		case stopping:
			s.setState(sc, StateStopped, "")
		case sc.Kind == KindChat:
			s.setState(sc, StateStopped, "") // clean stdin-close exit
		default:
			s.setState(sc, StateFinished, "")
		}
	})
	_ = p.persist(s, "")
	close(p.done)
	// Anything started for this run (the engine hook, its event stream)
	// was handed this context and ends with the run.
	p.cancel()
	// A finished run has nothing left to drive; the row is its history.
	s.mu.Lock()
	if s.procs[sess.ID] == p {
		delete(s.procs, sess.ID)
	}
	s.mu.Unlock()
	fin := p.snapshot()
	s.log.Info("spawner: session finished", "run", fin.ID, "sessionId", fin.SessionID,
		"state", fin.State, "exit", exitCode, "events", fin.EventCount)
}

// fromStore converts a persisted row to a Session.
func (s *Spawner) fromStore(m *store.ManagedSession) *Session {
	return &Session{
		ID: m.ID, SessionID: m.SessionID, Kind: Kind(m.Kind), CWD: m.CWD,
		Project: m.Project, Model: m.Model, Prompt: m.Prompt, ResumeFrom: m.ResumeFrom,
		State: State(m.State), PID: m.PID, StartedAt: m.StartedAt, EndedAt: m.EndedAt,
		ExitCode: m.ExitCode, EventCount: m.EventCount, PermissionDenial: m.PermissionDenial,
		TotalCostUSD: m.TotalCostUSD, TerminalReason: m.TerminalReason,
		LastError: m.LastError, StopReason: m.StopReason, CreatedBy: m.CreatedBy, UpdatedAt: m.UpdatedAt,
		ApprovalsEnabled: m.ApprovalsEnabled,
		Title:            m.Title,
		Engine:           m.Engine, ControlPort: m.ControlPort, ControlBase: m.ControlBase,
	}
}

// mergeLive overlays live process state over the persisted row.
func (s *Spawner) mergeLive(live *Session, persisted *Session) *Session {
	if live == nil {
		return persisted
	}
	c := *live
	if c.SessionID == "" {
		c.SessionID = persisted.SessionID
	}
	if c.TerminalReason == "" {
		c.TerminalReason = persisted.TerminalReason
	}
	if c.StopReason == "" {
		c.StopReason = persisted.StopReason
	}
	if c.Engine == "" {
		c.Engine = persisted.Engine
	}
	if c.ControlPort == 0 {
		c.ControlPort = persisted.ControlPort
	}
	if c.ControlBase == "" {
		c.ControlBase = persisted.ControlBase
	}
	return &c
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// newGeneration is the per-agentd-boot identifier stamped on every new
// managed row. The boot reconciler uses it to distinguish "this run is
// from the current agentd life" from "this run is an orphan from a
// previous life". UUIDv4 is fine — the reconciler only does equality
// comparisons, no sorting.
func newGeneration() string {
	return newUUID()
}

func permissionDenialLength(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0
	}
	return len(arr)
}
