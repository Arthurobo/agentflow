// tty.go — the PTY-backed `tty` run kind: the genuine interactive claude TUI
// as a managed run. The process contract is deliberately dumb: raw bytes in
// (master writes), raw bytes out (the agentd tty hub fans them to WebSocket
// subscribers). No stream-json parsing: the transcript is Claude's own JSONL
// under ~/.claude/projects, exactly like a locally-driven `claude` session.
package spawner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/creack/pty"
)

// StartTTY spawns (or restarts) the interactive TUI for an existing run key.
// It returns as soon as the process is running with the PTY master wired into
// the Proc; the ttyPump goroutine discovers the claude session id from the
// corpus and finalizes state on exit. Resize/tty byte I/O go through the
// master exposed by PTYMaster.
func (s *Spawner) StartTTY(ctx context.Context, runID string, opts Options) (*Session, error) {
	if opts.Cwd == "" {
		// NEVER fall back to the agentd process cwd: that is wherever the
		// daemon happened to be started, and sessions silently spawning
		// inside agentflow's own tree was a real bug. A caller that cannot name a
		// directory gets the home dir — neutral and always writable.
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		opts.Cwd = home
	}
	if opts.Project == "" && opts.Cwd != "" {
		opts.Project = eventmodel.ProjectFromCWD(opts.Cwd)
	}
	// Pre-trust this cwd in ~/.claude.json so the freshly-spawned TUI does
	// not block on the workspace-trust dialog waiting for input the phone
	// viewer cannot answer with a comfortable key sequence. The user's PC
	// session already trusted this directory — propagating that decision
	// to the phone-resumed TUI is the obvious thing to do, and a single
	// resolved CLAUDE.md includes warning is also accepted up front so
	// the TUI doesn't stop there either. Best-effort: a corrupt or missing
	// ~/.claude.json must NOT block the spawn.
	preTrustCwdBestEffort(opts.Cwd)
	opts.Kind = KindTTY
	args, err := s.argvFor(opts)
	if err != nil {
		return nil, err
	}
	bin, err := s.binaryFor(opts)
	if err != nil {
		return nil, err
	}
	//nolint:gosec // G204: the spawner intentionally launches the user-configured engine binary with a built argv.
	cmd := exec.Command(bin, args...)
	cmd.Dir = opts.Cwd
	// agentd is a daemon: whatever TERM/COLOR/CI variables leaked into ITS
	// environment (systemd, automation shells, SSH without a TTY) must never
	// decide the child TUI's capability detection. TERM=dumb in particular
	// made claude emit zero color codes — the "black and white TUI" bug.
	// OpenCode doesn't use the scrub vars; the engine's SpawnEnv is empty
	// for it, so the scrub applies anyway.
	cmd.Env = ChildSpawnEnv(os.Environ(), KindTTY, s.envFor(opts))

	// Birth at the viewer's real size: explicit opts win, then remembered
	// geometry for the resumed claude session, then the 80×24 classic. This
	// is what keeps the resume-replay from being wrapped at the wrong width
	// before the first client resize lands.
	cols, rows := opts.Cols, opts.Rows
	if cols == 0 || rows == 0 {
		if c, r, ok := s.rememberedGeom(opts.ResumeSessionID); ok {
			if cols == 0 {
				cols = c
			}
			if rows == 0 {
				rows = r
			}
		}
	}
	if cols < 20 || cols > 500 {
		cols = 80
	}
	if rows < 5 || rows > 300 {
		rows = 24
	}

	// Fix the transcript search BEFORE the child exists: the one folder it
	// may bind to, the files already there (another session's, never ours)
	// and, on --resume, the one file it must be.
	scope := s.discoveryScopeFor(opts)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		now := s.now()
		crash := &Session{
			ID: runID, Kind: KindTTY, CWD: opts.Cwd, Project: opts.Project,
			Model: opts.Model, State: StateCrashed, StartedAt: now, EndedAt: now,
			CreatedBy:   opts.CreatedBy,
			Engine:      resolveEngineID(opts.Engine),
			ControlPort: opts.ControlPort, ControlBase: opts.ControlBase,
			LastError: err.Error(),
		}
		_ = s.persistInitial(crash)
		return crash, fmt.Errorf("spawner: start tty claude: %w", err)
	}

	now := s.now()
	sess := &Session{
		ID: runID, Kind: KindTTY, CWD: opts.Cwd, Project: opts.Project,
		Model: opts.Model, State: StateStarting, PID: cmd.Process.Pid,
		StartedAt: now, UpdatedAt: now, CreatedBy: opts.CreatedBy,
		Title:       opts.Title,
		Engine:      resolveEngineID(opts.Engine),
		ControlPort: opts.ControlPort, ControlBase: opts.ControlBase,
	}
	if opts.ResumeSessionID != "" {
		sess.ResumeFrom = opts.ResumeSessionID
	} else if opts.ResumeTitle != "" {
		sess.ResumeFrom = opts.ResumeTitle
	}
	if opts.Cols > 0 && opts.Rows > 0 {
		// the caller told us the viewer's geometry — pin it to this claude
		// session so future unattended revives reuse it
		s.rememberGeom(opts.ResumeSessionID, opts.Cols, opts.Rows)
	}

	pctx, cancel := context.WithCancel(context.Background())
	waitch := make(chan error, 1)
	go func() { waitch <- cmd.Wait() }()
	p := newProc(sess, Child{
		Process: cmd.Process,
		Stdin:   master, // PTY master: writes are terminal input
		Stdout:  master, // the tty hub reads TUI output here
		Waitch:  waitch,
	}, cancel)
	p.ptyMaster = master

	// Register the PTY master with the tty hub as soon as the process
	// exists. Without this an unattended loop member (started here with no
	// WebSocket attached) blocks once the PTY buffer fills, because nobody
	// drains the master. ensureTTY in agentapi also registers, which is
	// idempotent.
	if register := s.registrar(); register != nil {
		resizeFn := func(cols, rows uint16) error { return s.ResizeTTY(runID, cols, rows) }
		register(runID, master, resizeFn, cols)
	}

	s.mu.Lock()
	s.procs[runID] = p
	s.mu.Unlock()
	_ = s.persistInitial(sess)
	// Trust-dialog nudge (found probing a real TUI): an UNTRUSTED cwd boots the TUI
	// into the workspace-safety dialog, which blocks the positional first
	// prompt until it is answered. One Enter accepts "1. Yes, I trust this
	// folder"; on a trusted cwd the same Enter is a no-op on the empty
	// composer, so it is always safe to send.
	go func() {
		select {
		case <-p.exited:
			return
		case <-pctx.Done():
			return
		case <-time.After(2500 * time.Millisecond):
		}
		p.stdinMu.Lock()
		_, _ = p.child.Stdin.Write([]byte("\r"))
		p.stdinMu.Unlock()
	}()

	go s.ttyPump(pctx, p, sess, opts, scope)
	if hook := s.postStartFn(); hook != nil {
		// Hand the engine seam a chance to bind its side channel AFTER the
		// TUI is up. We pass a clone of sess (not the live pointer) so the
		// hook cannot race with ttyPump's updates.
		go func(snapshot *Session) {
			hook(pctx, snapshot.ID, snapshot, opts)
		}(p.snapshot())
	}
	return p.snapshot(), nil
}

// SetSessionID records the engine's session id for a run.
//
// The spawner discovers a Claude session id itself; an OpenCode one is bound
// through the control API after the TUI is up, outside the spawner. It goes
// through the same locked update every other change to a live session does,
// and is persisted at once, so a run that exits a moment later still carries
// it. A run with no live process here only has its row updated.
func (s *Spawner) SetSessionID(runID, sessionID string) error {
	if runID == "" || sessionID == "" {
		return nil
	}
	s.mu.Lock()
	p, live := s.procs[runID]
	s.mu.Unlock()
	if !live {
		if s.st == nil {
			return nil
		}
		return s.st.SetManagedSessionID(context.Background(), runID, sessionID)
	}
	p.update(func(sc *Session) {
		sc.SessionID = sessionID
		sc.ControlSessionID = sessionID
		if sc.State == StateStarting {
			s.setState(sc, StateRunning, "")
		}
		sc.UpdatedAt = s.now()
	})
	return p.persist(s, "")
}

// PTYMaster returns the live PTY master for a tty run (nil for other kinds /
// dead runs). Callers must not close it — the spawner owns the lifetime.
func (s *Spawner) PTYMaster(runID string) (*os.File, bool) {
	s.mu.Lock()
	p, ok := s.procs[runID]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	m := p.ptyMaster
	return m, m != nil
}

// TTYSize reports the live PTY's current window size (0,0,false when the run
// has no live PTY). agentd's tty hub uses it to know which width existing
// replay bytes were emitted at.
func (s *Spawner) TTYSize(runID string) (cols, rows uint16, ok bool) {
	m, live := s.PTYMaster(runID)
	if !live {
		return 0, 0, false
	}
	r, c, err := pty.Getsize(m)
	if err != nil || r < 0 || c < 0 || r > math.MaxUint16 || c > math.MaxUint16 {
		return 0, 0, false
	}
	return uint16(c), uint16(r), true
}

// ResizeTTY applies a new window size to the run's PTY (TIOCSWINSZ) and
// remembers it under the run's claude session id for future revives. An
// identical size is a no-op: every real SIGWINCH makes the TUI repaint its
// frame, and overlapping clients (two phones, retry storms) requesting
// the same geometry must not churn repaints into the output stream.
func (s *Spawner) ResizeTTY(runID string, cols, rows uint16) error {
	m, ok := s.PTYMaster(runID)
	if !ok {
		return fmt.Errorf("spawner: no live tty for %s", runID)
	}
	if curRows, curCols, err := pty.Getsize(m); err == nil &&
		curRows == int(rows) && curCols == int(cols) {
		return nil
	}
	s.mu.Lock()
	p := s.procs[runID]
	sid := ""
	if p != nil {
		p.update(func(sc *Session) { sid = sc.SessionID })
	}
	s.mu.Unlock()
	if err := pty.Setsize(m, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
		return err
	}
	s.rememberGeom(sid, cols, rows)
	return nil
}

// ttyPump is the tty-kind counterpart of pump: there is no stream-json to
// parse (the hub owns the master's bytes), so it only discovers the claude
// session id from the corpus, waits for exit, and finalizes the run state.
func (s *Spawner) ttyPump(ctx context.Context, p *Proc, sess *Session, opts Options, scope discoveryScope) {
	started := time.Now()

	// Session-id discovery: the TUI writes ~/.claude/projects/<encoded
	// cwd>/<uuid>.jsonl; poll for the newest file modified after start and
	// read the sessionId off its first line.
	deadline := time.After(90 * time.Second)
	exclude := map[string]bool{}
	for _, sid := range opts.ExcludeSessionIDs {
		if sid != "" {
			exclude[sid] = true
		}
	}
	// The loop waits on exited, which the wait goroutine closes the moment
	// the process is reaped. It used to wait on done, which only this
	// function closes further down, so a TUI that died at birth read as
	// alive until the 90 second discovery deadline.
discover:
	for p.snapshot().SessionID == "" {
		select {
		case <-ctx.Done():
			return
		case <-p.exited:
			break discover
		case <-deadline:
			break discover
		case <-time.After(500 * time.Millisecond):
		}
		sid, path := scope.find(started, exclude)
		if sid != "" {
			p.update(func(sc *Session) {
				if sc.SessionID == "" {
					sc.SessionID = sid
					s.setState(sc, StateRunning, "")
					sc.UpdatedAt = s.now()
				}
			})
			if opts.Title != "" {
				appendCustomTitle(path, scope.dir, sid, opts.Title)
			}
			_ = p.persist(s, "")
			break
		}
	}
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
			s.setState(sc, StateStopped, "")
		case stopping:
			s.setState(sc, StateStopped, "")
		default:
			s.setState(sc, StateFinished, "")
		}
	})
	_ = p.persist(s, "")
	// close the master last: subscribers' reads fail and the hub tears down.
	if m, ok := s.PTYMaster(sess.ID); ok {
		_ = m.Close()
	}
	close(p.done)
	// Anything started for this run (the engine hook, its event stream)
	// was handed this context and ends with the run.
	p.cancel()
	s.mu.Lock()
	if s.procs[sess.ID] == p {
		delete(s.procs, sess.ID)
	}
	s.mu.Unlock()
	fin := p.snapshot()
	s.log.Info("spawner: tty session finished", "run", fin.ID, "sessionId", fin.SessionID,
		"state", fin.State, "exit", exitCode)
}

// encodeProjectDir maps a working directory onto the folder name claude uses
// for that project's transcripts under ~/.claude/projects: every byte that is
// not an ASCII letter or digit becomes "-". Pinned against real folders in
// TestEncodeProjectDirMatchesClaude ("/home/a/code/myapp" is
// "-home-a-code-myapp"; the old "/"-only replacement never matched a
// path with "_" or "." in it). Symlinks are resolved first because claude
// encodes the physical path its own process sees.
func encodeProjectDir(cwd string) string {
	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != "" {
		cwd = real
	}
	var b strings.Builder
	b.Grow(len(cwd))
	for i := 0; i < len(cwd); i++ {
		c := cwd[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// projectDirFor is the ONE transcript folder a run spawned in cwd may ever be
// bound to. Empty when either side is unknown, and an empty answer means
// "nothing", never "anywhere".
func projectDirFor(root, cwd string) string {
	if root == "" || cwd == "" {
		return ""
	}
	return filepath.Join(root, encodeProjectDir(cwd))
}

// discoveryScope is everything session-id discovery is allowed to know,
// fixed BEFORE the child process starts:
// - dir: the run's own project folder (projectDirFor), the only place it
// may look;
// - preexisting: the *.jsonl names already in that folder at spawn time.
// They belong to other sessions (a live one typing in the same cwd is
// the common case: a loop member has been seen renaming another live
// session in the same cwd this way) and are never candidates, however fresh
// their mtime;
// - resumeSID: on --resume the transcript is known by name and is the
// only acceptable answer;
// - claude: only claude writes this corpus; other engines find nothing.
type discoveryScope struct {
	dir         string
	preexisting map[string]bool
	resumeSID   string
	claude      bool
}

func newDiscoveryScope(root, cwd, resumeSID string, claude bool) discoveryScope {
	d := discoveryScope{dir: projectDirFor(root, cwd), resumeSID: resumeSID, claude: claude}
	d.preexisting = jsonlNames(d.dir)
	return d
}

// discoveryScopeFor pins where a spawn's transcript may be found. Call it
// before the child exists, or a fast child's own file lands in preexisting.
func (s *Spawner) discoveryScopeFor(opts Options) discoveryScope {
	return newDiscoveryScope(s.corpusRootDir(), opts.Cwd, opts.ResumeSessionID,
		resolveEngineID(opts.Engine) == engine.IDClaude)
}

// jsonlNames lists the transcript files in dir; empty when dir is missing.
func jsonlNames(dir string) map[string]bool {
	out := map[string]bool{}
	if dir == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			out[e.Name()] = true
		}
	}
	return out
}

// find returns the run's sessionId and transcript path, or "" while nothing
// acceptable exists yet; the pump polls. It never returns a file that was
// there before the spawn, never a file outside dir, and on resume never a
// file other than the resumed session's own.
func (d discoveryScope) find(started time.Time, exclude map[string]bool) (string, string) {
	if !d.claude || d.dir == "" {
		return "", ""
	}
	if d.resumeSID != "" {
		path := filepath.Join(d.dir, d.resumeSID+".jsonl")
		if _, err := os.Stat(path); err != nil {
			return "", ""
		}
		return d.resumeSID, path
	}
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return "", "" // folder not created yet: claude has not written a line
	}
	var bestPath string
	var bestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") || d.preexisting[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(started.Add(-2*time.Second)) {
			continue
		}
		path := filepath.Join(d.dir, e.Name())
		sid, ok := headSessionID(path)
		if !ok || exclude[sid] {
			continue // unreadable, or already claimed by another run: never steal it
		}
		if bestPath == "" || info.ModTime().After(bestMod) {
			bestPath, bestMod = path, info.ModTime()
		}
	}
	if bestPath == "" {
		return "", ""
	}
	sid, ok := headSessionID(bestPath)
	if !ok || exclude[sid] {
		return "", ""
	}
	return sid, bestPath
}

// headSessionID reads the sessionId off the first lines of a corpus JSONL.
func headSessionID(path string) (string, bool) {
	f, err := os.Open(path) //nolint:gosec // corpus path walked from the fixed ~/.claude/projects root
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewReader(f)
	for i := 0; i < 5; i++ {
		line, err := sc.ReadString('\n')
		if strings.Contains(line, `"sessionId"`) {
			var head struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &head) == nil && head.SessionID != "" {
				return head.SessionID, true
			}
		}
		if err != nil {
			return "", false
		}
	}
	return "", false
}

// CwdForSession finds the transcript for a claude session id anywhere in the
// corpus and returns the working directory it ran in. A resume needs the right
// cwd because claude keys each transcript to its project folder; when neither a
// managed run nor the session index knows the cwd (the index can be empty),
// this reads it straight from the transcript so the resume opens in the right
// place and binds instead of falling through to agentd's own cwd. Read-only: it
// never writes or renames the corpus.
func (s *Spawner) CwdForSession(sid string) string {
	if sid == "" {
		return ""
	}
	root := s.corpusRootDir()
	if root == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", sid+".jsonl"))
	for _, path := range matches {
		if cwd := headCwd(path); cwd != "" {
			return cwd
		}
	}
	return ""
}

// headCwd reads the cwd off the first lines of a corpus JSONL.
func headCwd(path string) string {
	f, err := os.Open(path) //nolint:gosec // corpus path under the fixed ~/.claude/projects root
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewReader(f)
	for i := 0; i < 40; i++ {
		line, err := sc.ReadString('\n')
		if strings.Contains(line, `"cwd"`) {
			var head struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &head) == nil && head.Cwd != "" {
				return head.Cwd
			}
		}
		if err != nil {
			return ""
		}
	}
	return ""
}

// appendCustomTitle writes the user's session name into the corpus JSONL in
// claude's own custom-title record format. The sessions list prefers it as
// the session's name; claude itself knows the
// record type, so resume stays clean. Best-effort: a failed write never
// affects the run. wantDir is the run's own project folder (projectDirFor);
// any other path is refused.
func appendCustomTitle(path, wantDir, sessionID, title string) {
	if path == "" || wantDir == "" || sessionID == "" || strings.TrimSpace(title) == "" {
		return
	}
	// The transcript a run names must be the one in its own project folder,
	// whatever discovery handed back. This is the second lock on the door
	// that let a test run rename five live sessions.
	if filepath.Clean(filepath.Dir(path)) != filepath.Clean(wantDir) {
		return
	}
	line, err := json.Marshal(map[string]string{
		"type":        "custom-title",
		"customTitle": title,
		"sessionId":   sessionID,
		"timestamp":   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec // corpus file returned by discoverSessionID
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(line, '\n'))
}

// preTrustCwdBestEffort marks `cwd` as already-accepted in ~/.claude.json so
// the workspace-trust dialog the freshly-spawned claude TUI would otherwise
// show is skipped. The user's existing PC session already trusted this
// directory — propagating that decision to the phone-resumed TUI is the
// right move; asking the phone user to answer "Yes, I trust this folder"
// with the mobile key row every time they reopen an old session is a
// regression vs. running the same session on the PC.
//
// Failures are swallowed: a corrupt or absent ~/.claude.json must not block
// the spawn, and the worst case if it can't be updated is that claude shows
// its trust dialog anyway — the same behavior we have today.
func preTrustCwdBestEffort(cwd string) {
	if cwd == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(path) //nolint:gosec // well-known absolute config path
	if err != nil {
		// Missing file is the common case on first run — start a minimal one
		// with just the trust flag set; claude will populate the rest on
		// its own startup.
		if os.IsNotExist(err) {
			seed := map[string]any{
				"projects": map[string]any{
					cwd: map[string]any{
						"hasTrustDialogAccepted":              true,
						"hasClaudeMdExternalIncludesApproved": true,
					},
				},
			}
			if b, mErr := json.MarshalIndent(seed, "", " "); mErr == nil {
				_ = os.WriteFile(path, b, 0o600) //nolint:gosec // path is well-known and absolute
			}
		}
		return
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return
	}
	var projects map[string]json.RawMessage
	if raw, ok := root["projects"]; ok {
		_ = json.Unmarshal(raw, &projects)
	}
	if projects == nil {
		projects = map[string]json.RawMessage{}
	}
	// Preserve any existing per-cwd stats (lastCost, exampleFiles, etc.) and
	// only flip the two trust bits; if the cwd entry doesn't exist yet,
	// start a minimal one with just the trust flags.
	entry := map[string]any{}
	if raw, ok := projects[cwd]; ok {
		_ = json.Unmarshal(raw, &entry)
	}
	entry["hasTrustDialogAccepted"] = true
	entry["hasClaudeMdExternalIncludesApproved"] = true
	patched, mErr := json.Marshal(entry)
	if mErr != nil {
		return
	}
	projects[cwd] = patched
	root["projects"], _ = json.Marshal(projects)
	out, mErr := json.MarshalIndent(root, "", " ")
	if mErr != nil {
		return
	}
	// Atomic-ish: write to a sibling tmp file then rename so a half-written
	// ~/.claude.json cannot break claude's own startup read.
	tmp := path + ".af-trust.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil { //nolint:gosec // path is well-known and absolute
		return
	}
	_ = os.Rename(tmp, path)
}
