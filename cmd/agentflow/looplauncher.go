package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/engine/opencode"
	"github.com/arthurobo/agentflow/internal/loopapi"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// loopLauncher adapts the spawner to loopapi.Launcher. It is the only place
// that knows a loop member is a process: loopapi owns the ordering and the
// failure policy, this owns argv, ports and waiting for an identity.
type loopLauncher struct {
	sp      *spawner.Spawner
	engines *engine.Registry
	log     *slog.Logger
	// identityBudget bounds the wait for the engine to say who it is. Claude's
	// own corpus discovery gives up at 90s, so anything shorter here would
	// report a member unidentified while discovery was still running.
	identityBudget time.Duration
	// tokenDir holds one owner-only file per member run with its token.
	tokenDir string
}

func newLoopLauncher(sp *spawner.Spawner, engines *engine.Registry, dataDir string, log *slog.Logger) *loopLauncher {
	return &loopLauncher{sp: sp, engines: engines, log: log, identityBudget: 100 * time.Second,
		tokenDir: filepath.Join(dataDir, "member-tokens")}
}

// hideToken takes a member's token out of its brief before the brief becomes
// argv.
//
// argv is world-readable (/proc/<pid>/cmdline, ps), so a token on it is a
// token any local user can use to post as that member. The token goes into an
// owner-only file instead, and every command in the brief reads it from there
// with $(cat ...), inside the double quotes the brief already puts around the
// header. The brief says where the token is, so the agent is never left
// guessing.
//
// This keeps the token off argv; it does not make routing between members a
// security boundary. Every member runs as the same user as agentd and could
// read any other member's file, so the loop's routing rules are a protocol
// for cooperating agents, not isolation between them.
func (l *loopLauncher) hideToken(runID string, spec loopapi.LaunchSpec) (string, error) {
	if spec.Token == "" || !strings.Contains(spec.Prompt, spec.Token) {
		return spec.Prompt, nil
	}
	path, err := l.tokenPath(runID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(l.tokenDir, 0o700); err != nil {
		return "", fmt.Errorf("token dir: %w", err)
	}
	if err := os.Chmod(l.tokenDir, 0o700); err != nil { //nolint:gosec // G302: a directory needs the execute bit; 0700 is owner-only
		return "", fmt.Errorf("token dir: %w", err)
	}
	tmp, err := os.CreateTemp(l.tokenDir, ".token-*")
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if _, err := tmp.WriteString(spec.Token); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("token file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("token file: %w", err)
	}
	prompt := strings.ReplaceAll(spec.Prompt, spec.Token, "$(cat '"+path+"')")
	return prompt + "\n\nYOUR TOKEN\n" +
		"Your agent token is not written in this brief. It is in " + path + ", which only you can read, " +
		"and the commands above read it from there. Never print it, and never paste it anywhere.\n", nil
}

// tokenPath is where a run's token file lives. The path is placed inside
// single quotes in a shell command, so anything that could end the quoting is
// refused rather than escaped.
func (l *loopLauncher) tokenPath(runID string) (string, error) {
	if runID == "" || strings.ContainsAny(runID, "/.") {
		return "", fmt.Errorf("token file: unusable run id %q", runID)
	}
	path := filepath.Join(l.tokenDir, runID+".token")
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "'\"$`\\\n") {
		return "", fmt.Errorf("token file: the data directory %q cannot be named safely in a shell command", l.tokenDir)
	}
	return path, nil
}

// forgetToken removes a run's token file.
func (l *loopLauncher) forgetToken(runID string) {
	if path, err := l.tokenPath(runID); err == nil {
		_ = os.Remove(path)
	}
}

// engineForTool maps the loop's tool vocabulary onto the engine registry's.
// They are the same two values on purpose (store/tools.go says so), so this is
// a normalisation rather than a translation.
func engineForTool(tool string) string {
	if store.NormaliseTool(tool) == store.ToolOpenCode {
		return engine.IDOpenCode
	}
	return engine.IDClaude
}

func (l *loopLauncher) Launch(ctx context.Context, runID string, spec loopapi.LaunchSpec) (loopapi.LaunchResult, error) {
	opts := spawner.Options{
		Kind:              spawner.KindTTY,
		Engine:            engineForTool(spec.Tool),
		Cwd:               spec.Cwd,
		Model:             spec.Model,
		Title:             spec.Title,
		ExcludeSessionIDs: spec.ExcludeSessionIDs,
		CreatedBy:         "loop",
	}
	prompt, err := l.hideToken(runID, spec)
	if err != nil {
		return loopapi.LaunchResult{}, fmt.Errorf("%s: %w", spec.Role, err)
	}
	opts.Prompt = prompt
	// An engine with its own HTTP control plane needs its port before the
	// process exists, exactly as the spawn handler does it.
	if l.engines != nil {
		eng, err := l.engines.Get(opts.Engine)
		if err != nil {
			return loopapi.LaunchResult{}, fmt.Errorf("%s: %w", spec.Role, err)
		}
		if eng.NeedsControlPort() {
			alloc, ok := eng.(interface {
				AllocateAndPrepare(ctx context.Context, cwd, title string) (int, string, string, error)
			})
			if !ok {
				return loopapi.LaunchResult{}, fmt.Errorf("%s: engine %s needs a control port and exposes no allocator", spec.Role, opts.Engine)
			}
			port, base, sid, err := alloc.AllocateAndPrepare(ctx, opts.Cwd, opts.Title)
			if err != nil {
				return loopapi.LaunchResult{}, fmt.Errorf("%s: %w", spec.Role, err)
			}
			opts.ControlPort, opts.ControlBase, opts.ControlSessionID = port, base, sid
		}
	}
	if _, err := l.sp.StartTTY(ctx, runID, opts); err != nil {
		l.forgetToken(runID)
		return loopapi.LaunchResult{}, fmt.Errorf("%s: %w", spec.Role, err)
	}
	sid := l.awaitIdentity(ctx, runID)
	return loopapi.LaunchResult{SessionID: sid, Identified: sid != ""}, nil
}

// awaitIdentity blocks until the run reports an engine session id, the process
// dies, or the budget runs out. Returning "" is not an error here: the caller
// decides what an unidentified member means, and it is the caller that has to
// stop it.
func (l *loopLauncher) awaitIdentity(ctx context.Context, runID string) string {
	deadline := time.Now().Add(l.identityBudget)
	for {
		sess, err := l.sp.Status(ctx, runID)
		if err == nil && sess != nil {
			if sess.SessionID != "" {
				return sess.SessionID
			}
			if sess.State.Terminal() {
				return "" // it exited before it ever said who it was
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (l *loopLauncher) Stop(ctx context.Context, runID string) error {
	l.forgetToken(runID)
	// Stop, never CloseStdin: a TTY member does not exit on a closed stdin.
	return l.sp.Stop(ctx, runID)
}

// modelCacheTTL bounds how long a catalogue is reused.
//
// The answer changes when the engineer authenticates a provider or edits
// their opencode config, which is rare and which they will not do while a
// wizard is open. Ten minutes is short enough that a new provider shows up
// without a restart and long enough that opening the picker twice, or opening
// it and then creating, costs one probe rather than three.
const modelCacheTTL = 10 * time.Minute

// loopModels answers "what can this tool run right now". It prefers a live
// OpenCode already running on this machine, because asking it is free, and
// falls back to a throwaway `opencode serve` when there is none.
//
// The probe is expensive: a process, a port, and a wait for the catalogue to
// fill. Without a cache the picker paid it, and then create paid it again to
// check the model the picker had just offered.
type loopModels struct {
	sp      *spawner.Spawner
	engines *engine.Registry
	binary  string

	// probe is the slow path, injected so a test can count how often it runs.
	probe func(ctx context.Context, binary, cwd string) ([]engine.Model, error)

	mu    sync.Mutex
	cache map[string]modelCacheEntry
}

type modelCacheEntry struct {
	models []engine.Model
	at     time.Time
}

func (m *loopModels) Models(ctx context.Context, tool, cwd string) ([]engine.Model, error) {
	if store.NormaliseTool(tool) != store.ToolOpenCode {
		return nil, fmt.Errorf("no live model source for %s", tool)
	}
	// A live OpenCode is free to ask and always current, so it outranks the
	// cache. The len check matters: a TUI that has just started answers with
	// an empty catalogue, and falling through to the probe is the recovery.
	if base := m.liveBase(ctx); base != "" {
		if out, err := opencode.NewControl(base).Models(ctx); err == nil && len(out) > 0 {
			return out, nil
		}
	}
	key := m.cacheKey(cwd)
	if out, ok := m.cached(key); ok {
		return out, nil
	}
	probe := m.probe
	if probe == nil {
		probe = func(ctx context.Context, binary, cwd string) ([]engine.Model, error) {
			return opencode.ProbeModels(ctx, binary, cwd, nil)
		}
	}
	out, err := probe(ctx, m.binary, cwd)
	if err != nil {
		return nil, err
	}
	// An empty catalogue is not cached. It is the answer of a machine with no
	// authenticated provider, and also the answer of a probe that gave up
	// early; caching it would hold the wrong one for ten minutes.
	if len(out) > 0 {
		m.store(key, out)
	}
	return out, nil
}

// cacheKey is the binary and its mtime, so upgrading opencode invalidates the
// catalogue without anyone remembering to. The cwd is deliberately NOT part of
// it: the providers come from the global config, and a cold probe returns
// nothing in any directory and then fills, which is what made this look
// directory-dependent.
func (m *loopModels) cacheKey(string) string {
	bin := m.binary
	if bin == "" {
		bin = "opencode"
	}
	if resolved, err := exec.LookPath(bin); err == nil {
		bin = resolved
	}
	if st, err := os.Stat(bin); err == nil {
		return bin + "@" + strconv.FormatInt(st.ModTime().UnixNano(), 10)
	}
	return bin
}

func (m *loopModels) cached(key string) ([]engine.Model, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.cache[key]
	if !ok || time.Since(e.at) > modelCacheTTL {
		return nil, false
	}
	return append([]engine.Model(nil), e.models...), true
}

func (m *loopModels) store(key string, models []engine.Model) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cache == nil {
		m.cache = map[string]modelCacheEntry{}
	}
	m.cache[key] = modelCacheEntry{models: append([]engine.Model(nil), models...), at: time.Now()}
}

// liveBase finds a running OpenCode's control API, if there is one.
func (m *loopModels) liveBase(ctx context.Context) string {
	if m.sp == nil {
		return ""
	}
	runs, err := m.sp.List(ctx, "")
	if err != nil {
		return ""
	}
	for _, r := range runs {
		if r == nil || r.Engine != engine.IDOpenCode || r.ControlBase == "" {
			continue
		}
		if r.State.Terminal() {
			continue
		}
		return r.ControlBase
	}
	return ""
}
