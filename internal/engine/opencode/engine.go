// Package opencode is the OpenCode engine implementation. OpenCode's TUI
// runs a full HTTP API on a configurable local port (verified live:
// `opencode --port 47312` answers `GET /api/health` while the TUI runs),
// and that API is the native control plane. The argv is tiny:
//
//	opencode --port <n> --hostname 127.0.0.1 [dir]
//
// The spawner MUST pre-allocate the port (the child picks its own if you
// pass `--port 0` and we cannot discover it), then pre-create the session
// via POST /session so the session id is known BEFORE the process starts —
// the same lifecycle invariant Claude enjoys after , without the
// 150s DiscoverTimeout wait.
package opencode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
)

// ID is engine.IDOpenCode.
const ID = engine.IDOpenCode

// Config configures the OpenCode engine. Zero values use sensible defaults
// (binary via PATH, hostname 127.0.0.1, port from the spawner's allocator).
type Config struct {
	// Path is the opencode binary override (default: PATH lookup).
	Path string
	// Hostname is the address the child binds on (default 127.0.0.1 — the
	// control API must not be reachable off-machine).
	Hostname string
	// PortAllocator is invoked to pick the port the child will bind on.
	// The spawner wires the default (system ephemeral range) by calling
	// ephemeralPort at startup; tests inject a fake. Must not return 0
	// (the child would then pick its own, which we cannot discover).
	PortAllocator func() (int, error)
}

// Engine is the OpenCode engine.
type Engine struct {
	cfg Config
}

// New builds the OpenCode engine with the given config. A nil cfg yields the
// production defaults.
func New(cfg Config) *Engine {
	if cfg.Hostname == "" {
		cfg.Hostname = "127.0.0.1"
	}
	if cfg.PortAllocator == nil {
		cfg.PortAllocator = ephemeralPort
	}
	return &Engine{cfg: cfg}
}

// ID implements engine.Engine.
func (e *Engine) ID() string { return ID }

// ErrModelRef refuses a model that is not in OpenCode's provider/model form.
var ErrModelRef = errors.New("opencode: model must be provider/model")

// ValidateModelRef enforces the provider/model shape. A bare id is not an
// error to opencode, it silently resolves to nothing and the member runs on
// whatever the default is, which is worse than a refusal. The model half may
// itself contain slashes (openrouter routes look like
// openrouter/anthropic/claude-3), so only the FIRST separator is structural.
func ValidateModelRef(model string) error {
	m := strings.TrimSpace(model)
	if m == "" {
		return nil // empty means "let opencode choose"
	}
	provider, rest, ok := strings.Cut(m, "/")
	if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(rest) == "" {
		return fmt.Errorf("%w: got %q, want something like anthropic/claude-sonnet-4-5", ErrModelRef, model)
	}
	return nil
}

// BuildArgs assembles the opencode argv. Order: --port, --hostname, the
// optional flags, --prompt, then the optional positional directory LAST.
//
// --prompt is what makes an OpenCode member a loop member: verified live
// against opencode 1.18.29, a TUI given --prompt processes it as the first
// turn at boot, which is the same contract claude gets from its trailing
// positional. Without it a member boots with no instructions and never polls.
//
// --auto is OpenCode's --dangerously-skip-permissions and defaults to FALSE,
// so a member without it stalls on its first permission prompt with nobody at
// the keyboard to answer. It is unconditional here for the same reason the
// claude engine's --dangerously-skip-permissions is: a managed TTY run has no
// one to answer a prompt menu. The engineer's own config already allows
// everything, so the difference is not observable on his machine; it is set
// explicitly so a member on any other machine behaves the same.
func (e *Engine) BuildArgs(opts engine.Options) ([]string, error) {
	// The port the host already allocated and recorded on the row. It MUST
	// be the one in the argv.
	//
	// This used to call the allocator again, because Options carried no
	// port to use. The child then listened on that second port while
	// PostStart polled the first, WaitReady timed out after 12s, the
	// session id was never bound, and the run sat in "starting" forever —
	// invisible to the Sessions list, which is keyed on session id. Two
	// allocations for one run is one too many.
	//
	// Allocating here is now only the un-prepared path: a caller that never
	// reserved a port (tests that care about argv shape, or a future host
	// that lets the engine choose).
	port := opts.ControlPort
	if port == 0 && e.cfg.PortAllocator != nil {
		if p, err := e.cfg.PortAllocator(); err == nil {
			port = p
		}
	}
	args := []string{"--port", strconv.Itoa(port), "--hostname", e.cfg.Hostname}
	if opts.ResumeSessionID != "" {
		// OpenCode CLI uses --session <id> to resume (verified against the
		// `opencode --help` output).
		args = append(args, "--session", opts.ResumeSessionID)
	}
	if opts.Agent != "" {
		args = append(args, "--agent", opts.Agent)
	}
	if opts.Model != "" {
		if err := ValidateModelRef(opts.Model); err != nil {
			return nil, err
		}
		args = append(args, "--model", opts.Model)
	}
	args = append(args, "--auto")
	if opts.Prompt != "" {
		args = append(args, "--prompt", opts.Prompt)
	}
	if opts.Cwd != "" {
		args = append(args, opts.Cwd)
	}
	return args, nil
}

// SpawnEnv returns the per-spawn env entries for an OpenCode child. The only
// override needed is the port (when supplied); the child reads it from argv
// directly, but tests + port-allocation integrity checks may want to see it
// in the env too. nil when no port is known yet.
func (e *Engine) SpawnEnv(_ engine.Options) []string { return nil }

// NeedsControlPort reports that OpenCode serves a side-channel API. The
// spawner MUST pre-allocate the port and pass it to BuildArgs / argv.
func (e *Engine) NeedsControlPort() bool { return true }

// LookupBinary returns the resolved opencode path. Honors the engine-supplied
// override first, then falls back to PATH lookup.
func (e *Engine) LookupBinary() (string, error) {
	if e.cfg.Path != "" {
		if p, err := lookupPath(e.cfg.Path); err == nil {
			return p, nil
		}
	}
	return lookupPath("opencode")
}

// LookupVersion returns "opencode 1.18.25" or similar (best-effort).
func (e *Engine) LookupVersion(ctx context.Context) string {
	bin, err := e.LookupBinary()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output() //nolint:gosec // G204: the engine binary resolved from PATH
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// AllocateAndPrepare is the engine-specific spawn preparation helper
// invoked by agentapi before calling spawner.StartWithID. It (1) allocates a
// loopback port for the TUI's control API, (2) waits for the TUI to come
// up (if it's already running — a no-op when not), and (3) pre-creates a
// session via POST /session so the session id is known BEFORE the spawner
// launches the TUI process.
//
// Called by the spawner ONLY when NeedsControlPort() is true. The returned
// (port, base, sessionID) are stamped on spawner.Options so the persisted
// managed row carries the control endpoint and the spawner argv includes
// the port.
//
// In the standard agentd flow the OpenCode TUI is a child of this very
// spawn — there is nothing for WaitReady to wait on (the API doesn't exist
// yet). The function therefore SKIPS the wait and returns an ephemeral
// port + an empty session id; the TUI then comes up, the spawner's port-
// injection flow binds the session, and the control sheet reads state
// afterward. Re-entering this path while an OpenCode TUI is already
// running (one reused for several sessions) would
// WaitReady first — that path is tested separately.
func (e *Engine) AllocateAndPrepare(_ context.Context, cwd, title string) (int, string, string, error) {
	port, err := e.cfg.PortAllocator()
	if err != nil {
		return 0, "", "", fmt.Errorf("opencode: allocate port: %w", err)
	}
	if err := ValidatePort(port); err != nil {
		return 0, "", "", err
	}
	base := fmt.Sprintf("http://%s:%d", e.cfg.Hostname, port)
	return port, base, "", nil
}

// AllocatedBaseURL builds the control API base URL for a given port. Exposed
// for callers that allocate the port themselves (e.g. tests).
func (e *Engine) AllocatedBaseURL(port int) string {
	return fmt.Sprintf("http://%s:%d", e.cfg.Hostname, port)
}

// PostStart binds the spawned TUI's control API. It waits for /api/health,
// pre-creates a session via POST /session, focuses it via
// POST /tui/select-session, and updates the Session row through the supplied
// store (mirroring what the spawner would have done if it knew about
// OpenCode). Designed to be passed to spawner.Options.PostStart.
//
// bound is the supplied engine.WriteBack callback used to stamp the bound
// session id onto the managed_sessions row.
//
// resumeSessionID makes it resume-aware, and that is not a refinement: it is
// the difference between getting your conversation back and getting an empty
// one. `opencode --session <id>` reopens the existing session by itself —
// verified live against opencode 1.18.29, where a TUI respawned with the flag
// and NO API call set its terminal title to "OC | round13 resume probe", the
// title of the session being resumed. If PostStart then created a session
// anyway, its FocusSession pulled the TUI off that session onto the new empty
// one; the observed title sequence was:
//
//	OpenCode -> OC | round13 resume probe -> OpenCode -> OC | WRONG new session
//
// and the run row was bound to the wrong id as well. So on a resume there is
// nothing to create: wait for the API, bind and focus the id the child was
// already told to open.
func (e *Engine) PostStart(ctx context.Context, base string, cwd, title, resumeSessionID string, bound func(sessionID string)) error {
	if base == "" {
		return errors.New("opencode: PostStart: empty base URL")
	}
	cli := NewControl(base)
	waitCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := cli.WaitReady(waitCtx, 12*time.Second); err != nil {
		return err
	}
	sessionID := strings.TrimSpace(resumeSessionID)
	if sessionID == "" {
		sess, err := CreateSession(ctx, base, cwd, title)
		if err != nil {
			return err
		}
		sessionID = sess.ID
	}
	if err := FocusSession(ctx, base, sessionID); err != nil {
		// Best-effort: a focus failure shouldn't block the run.
		_ = err
	}
	cli.BindSession(sessionID)
	if bound != nil {
		bound(sessionID)
	}
	return nil
}

// Control returns the OpenCode HTTP control implementation bound to the
// given base URL. baseURL is typically "http://127.0.0.1:47312".
func (e *Engine) Control(baseURL string) engine.Control {
	if baseURL == "" {
		return NewControl("")
	}
	return NewControl(baseURL)
}

// ephemeralPort binds an ephemeral TCP socket on the loopback interface and
// returns the OS-assigned port, immediately closing the listener. The brief
// race window is acceptable because the child re-binds within a few ms; the
// only failure mode is a collision (port already taken), which is rare in
// the ephemeral range.
func ephemeralPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("opencode: ephemeral port allocator: not a TCP addr")
	}
	return addr.Port, nil
}

// ValidatePort checks a pre-allocated port is in range and not 0 (the spawner
// contract: never pass --port 0 to the child).
func ValidatePort(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("opencode: invalid control port %d", port)
	}
	return nil
}

func lookupPath(name string) (string, error) {
	if strings.Contains(name, "/") {
		if _, err := os.Stat(name); err != nil {
			return "", fmt.Errorf("opencode binary %q: %w", name, err)
		}
		return name, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("opencode binary %q: not found in PATH", name)
}
