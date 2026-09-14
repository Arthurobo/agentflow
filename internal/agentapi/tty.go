// tty.go — agentd's in-memory TTY hub + HTTP/WS surface for `tty` runs. The
// hub owns one read loop per live PTY and fans raw output bytes out to every
// WebSocket subscriber. Input is bytes-in; resize is cols/rows — nothing here
// parses TUI output.
package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arthurobo/agentflow/internal/spawner"
)

func deadlineSoon() time.Time { return time.Now().Add(10 * time.Second) }

// ttySess is one live PTY fan-out point.
type ttySess struct {
	master io.ReadWriteCloser
	resize func(cols, rows uint16) error
	subs   map[chan []byte]struct{}
	done   chan struct{} // closed when the master read loop ends

	// writeMu serializes input to the master. A submit is text, a pause and
	// a carriage return; held across all three, two viewers (or a viewer and
	// the courier) can never interleave their keystrokes.
	writeMu sync.Mutex

	// replay is a bounded ring of recent output (FIFO): every chunk that goes
	// through the PTY lands here first, so a LATE subscriber (phone attaching
	// mid-run, or reconnecting after a drop) gets the recent past before the
	// live stream instead of a blank terminal whose history depends on resize
	// repaint luck. Capped at ttyReplayCap bytes — beyond that the oldest
	// chunks fall off, mirroring the client's own scrollback FIFO.
	replay      [][]byte
	replayBytes int

	// bracketed mirrors the TUI's DECSET 2004 (bracketed paste) mode, learned
	// by watching its OUTPUT for the enable/disable sequences.
	//
	// It matters for multi-line injection. A turn that names an attachment
	// path on its own line and then carries the engineer's text is several
	// lines; written raw, the first newline submits it and the rest arrives
	// as separate turns. Wrapped in paste markers it arrives as one message —
	// but only if the TUI actually asked for paste mode, because a TUI that
	// did not will render the markers as literal text. So we do not guess:
	// we read what it told the terminal.
	bracketed bool

	// geomCols is the terminal WIDTH every byte in replay was emitted at.
	// Raw TUI bytes embed cursor addressing and erase ranges counted in lines
	// AT THEIR EMISSION WIDTH: replaying them into a different width re-wraps
	// soft lines, so relative cursor moves land wrong and stale copies of the
	// frame survive on screen — verbatim "repeated text". A width change
	// therefore invalidates the whole ring (see Resize).
	geomCols uint16
}

// ttyReplayCap bounds one session's replay ring (~2MB covers tens of
// thousands of TUI lines).
const ttyReplayCap = 2 << 20

// ttySubBuffer is the per-subscriber channel capacity in chunks (32KB each).
// Big enough to absorb a boot-replay burst while the browser drains over a
// mobile link; overflow KICKS the subscriber (see readLoop) rather than ever
// dropping bytes silently.
const ttySubBuffer = 256

func (s *ttySess) remember(chunk []byte) {
	s.replay = append(s.replay, chunk)
	s.replayBytes += len(chunk)
	for s.replayBytes > ttyReplayCap && len(s.replay) > 0 {
		s.replayBytes -= len(s.replay[0])
		s.replay = s.replay[1:]
	}
}

func (s *ttySess) replaySnapshot() []byte {
	out := make([]byte, 0, s.replayBytes)
	for _, c := range s.replay {
		out = append(out, c...)
	}
	return out
}

// ttyHub fans PTY output bytes out to subscribers and serializes master
// writes. Sessions register when the PTY starts and deregister when the read
// loop hits EOF/error (process exit closes the master).
type ttyHub struct {
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*ttySess
}

func newTTYHub(log *slog.Logger) *ttyHub {
	if log == nil {
		log = slog.Default()
	}
	return &ttyHub{log: log, sessions: map[string]*ttySess{}}
}

// Register wires a live PTY into the hub and starts its output fan-out loop.
// Registering the same master again is a no-op. A different master for the
// same run id replaces the entry: that is a quick restart whose old read loop
// has not noticed its master closing yet, and the new process must not be
// left without a reader. geomCols is the PTY's birth/current width
// (0 unknown) — the width existing output was emitted at.
func (h *ttyHub) Register(runID string, master io.ReadWriteCloser, resize func(cols, rows uint16) error, geomCols uint16) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.sessions[runID]; ok && cur.master == master {
		return
	}
	s := &ttySess{master: master, resize: resize, subs: map[chan []byte]struct{}{}, done: make(chan struct{}), geomCols: geomCols}
	h.sessions[runID] = s
	go h.readLoop(runID, s)
}

// RegisterTTY exposes the hub's Register on the agentapi Server so the
// spawner (a separate package) can wire PTYs into the hub. The spawner calls
// it from inside StartTTY so an unattended loop member's PTY is drained even
// before anyone attaches.
func (s *Server) RegisterTTY(runID string, master interface {
	io.ReadWriteCloser
}, resize func(cols, rows uint16) error, geomCols uint16) {
	s.tty.Register(runID, master, resize, geomCols)
}

// Subscribe attaches one consumer to a run's PTY output. It returns the
// session's replay ring snapshot (bytes produced BEFORE this attach — write
// them to the consumer first) plus the live channel, which closes when the
// session ends or the subscriber is kicked; unsub detaches early. The
// snapshot and the registration happen under one lock, so no output can slip
// between "replay" and "live".
func (h *ttyHub) Subscribe(runID string) ([]byte, <-chan []byte, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[runID]
	if !ok {
		return nil, nil, nil, errors.New("tty: no live session for " + runID)
	}
	ch := make(chan []byte, ttySubBuffer)
	s.subs[ch] = struct{}{}
	replay := s.replaySnapshot()
	unsub := func() {
		// The read loop sends to and closes subscriber channels only under
		// h.mu, and always removes a channel from subs before closing it. So
		// closing here, under the same lock and only while still subscribed,
		// closes each channel exactly once — and it is what lets the
		// consumer's range over the channel end.
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
	return replay, ch, unsub, nil
}

// Live reports whether the run has a registered PTY.
func (h *ttyHub) Live(runID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.sessions[runID]
	return ok
}

func (h *ttyHub) session(runID string) (*ttySess, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[runID]
	if !ok {
		return nil, errors.New("tty: no live session for " + runID)
	}
	return s, nil
}

// Write sends input bytes to the run's PTY master.
func (h *ttyHub) Write(runID string, b []byte) error {
	s, err := h.session(runID)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.master.Write(b)
	return err
}

// ttySubmitPause separates a submitted line's TEXT from its carriage return.
// Both TUIs read stdin in chunks and treat a multi-character chunk as a
// paste: a "\r" arriving in the same read() is a newline INSIDE the prompt
// box, not a submit. Whether the two land in one read is pure timing, which
// is why the phone's Enter worked intermittently. Pacing them apart in the
// backend is the only place it can be guaranteed — two client frames can
// still coalesce into one server-side read.
const ttySubmitPause = 40 * time.Millisecond

// Bracketed-paste markers (DECSET 2004). Wrapping the text tells a TUI that
// has enabled the mode "this is pasted content", so embedded newlines in a
// multi-line draft stay newlines instead of submitting mid-text.
const (
	bracketedPasteStart = "\x1b[200~"
	bracketedPasteEnd   = "\x1b[201~"
)

// ttySubmit writes one composed line to a PTY as a submit: the text first
// (optionally bracketed-paste wrapped), then, after ttySubmitPause, the
// carriage return as a SEPARATE write. See ttySubmitPause for why the two
// cannot share a write.
func ttySubmit(w io.Writer, text string, bracketed bool) error {
	if text != "" {
		payload := text
		if bracketed {
			payload = bracketedPasteStart + text + bracketedPasteEnd
		}
		if _, err := w.Write([]byte(payload)); err != nil {
			return err
		}
		time.Sleep(ttySubmitPause)
	}
	_, err := w.Write([]byte("\r"))
	return err
}

// DECSET 2004 enable/disable, as the TUI writes them to the terminal.
var (
	decsetBracketedOn  = []byte("\x1b[?2004h")
	decsetBracketedOff = []byte("\x1b[?2004l")
)

// BracketedPaste reports whether the run's TUI has bracketed paste on. False
// for an unknown run — the safe answer, since unwanted markers are visible
// junk in the prompt box.
func (h *ttyHub) BracketedPaste(runID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[runID]
	return ok && s.bracketed
}

// Submit sends one composed line to the run's PTY as text-then-CR (see
// ttySubmit). The control layer reuses the same pacing through the spawner's
// PTY master, so a slash command injected from the control sheet and a line
// sent from the composer reach the TUI identically.
func (h *ttyHub) Submit(runID string, text string, bracketed bool) error {
	s, err := h.session(runID)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return ttySubmit(s.master, text, bracketed)
}

// Resize applies a new window size to the run's PTY. A COLS change clears
// the replay ring: every stored frame was emitted at the old width, and
// replaying it at the new one re-wraps lines so erase/cursor accounting
// breaks — claude's post-SIGWINCH full repaint then stacks a fresh copy of
// the transcript on top of un-erased stale frames (the duplicated-text
// artifact). Keeping the ring single-width keeps replays byte-faithful.
// Rows-only changes don't re-wrap and keep history.
func (h *ttyHub) Resize(runID string, cols, rows uint16) error {
	h.mu.Lock()
	s, ok := h.sessions[runID]
	h.mu.Unlock()
	if !ok || s.resize == nil {
		return errors.New("tty: no live session for " + runID)
	}
	if cols > 0 {
		h.mu.Lock()
		if s.geomCols != cols {
			s.replay = nil
			s.replayBytes = 0
			s.geomCols = cols
		}
		h.mu.Unlock()
	}
	return s.resize(cols, rows)
}

// readLoop drains the PTY master, records every chunk into the replay ring
// and fans each chunk out to every subscriber. On EOF/read-error (process
// exit) it closes all subscriber channels and forgets the session.
//
// A subscriber whose channel is full is KICKED (channel closed, subscription
// dropped) instead of having bytes silently dropped: a gap in a raw TUI byte
// stream is corruption — half-swallowed escape sequences poison colors and
// layout until the next full repaint. Kicking makes the client reconnect and
// re-attach into the replay ring, which self-heals deterministically.
func (h *ttyHub) readLoop(runID string, s *ttySess) {
	defer func() {
		h.mu.Lock()
		// Only forget the entry if it is still this session: a restart may
		// already have registered a new master under the same run id.
		if h.sessions[runID] == s {
			delete(h.sessions, runID)
		}
		for ch := range s.subs {
			delete(s.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
		close(s.done)
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			h.mu.Lock()
			// Track the TUI's paste mode as it flips. Last one in the chunk
			// wins, which is the same order the terminal would apply them.
			if i := bytes.LastIndex(chunk, decsetBracketedOn); i >= 0 {
				if j := bytes.LastIndex(chunk, decsetBracketedOff); j < i {
					s.bracketed = true
				}
			}
			if i := bytes.LastIndex(chunk, decsetBracketedOff); i >= 0 {
				if j := bytes.LastIndex(chunk, decsetBracketedOn); j < i {
					s.bracketed = false
				}
			}
			s.remember(chunk)
			for ch := range s.subs {
				select {
				case ch <- chunk:
				default: // slow consumer: kick it (client reconnects into replay)
					delete(s.subs, ch)
					close(ch)
				}
			}
			h.mu.Unlock()
		}
		if err != nil {
			if err != io.EOF {
				h.log.Debug("tty: master read end", "run", runID, "err", err)
			}
			return
		}
	}
}

// --- HTTP/WS surface ---------------------------------------------------------------

// Errors from ensureTTY, each with its own HTTP status.
var (
	// ErrRunStopped: the run was stopped on purpose (by a person, a loop
	// cancel or the restart reaper) and an attach must not revive it. Only
	// POST /sessions/{id}/tty?restart=1 does.
	ErrRunStopped = errors.New("tty: run was stopped; restart it explicitly")
	// errSessionBusy: another live run already holds the claude session, and
	// two processes on one session corrupt its transcript.
	errSessionBusy = errors.New("tty: another live run already holds this session")
)

// ttyErrorStatus maps an ensureTTY error to its HTTP status and error code.
func ttyErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, errRunNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrRunStopped):
		return http.StatusGone, "run_stopped"
	case errors.Is(err, errSessionBusy):
		return http.StatusConflict, "session_busy"
	default:
		return http.StatusConflict, "tty_failed"
	}
}

// WebSocket frame and liveness limits for the terminal socket.
const (
	// maxTTYFrame bounds one client frame: a pasted block of text, not a file.
	maxTTYFrame = 1 << 20
	// wsPingPeriod keeps idle sockets alive through the Funnel relay and
	// mobile NATs; wsPongWait tears down a half-open socket within a minute.
	wsPingPeriod = 25 * time.Second
	wsPongWait   = 60 * time.Second
)

func (s *Server) upgrader() *websocket.Upgrader {
	return &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// wsDevice has already checked the Origin before any work was done;
		// checking again here keeps the upgrader safe on its own.
		CheckOrigin: func(r *http.Request) bool { return s.originAllowed(r.Header.Get("Origin")) },
	}
}

// handleTTYStart ensures the run's PTY is live (spawning it if needed) and
// registered with the hub. Idempotent — reconnecting browsers call it before
// every WS attach. Optional ?cols=&rows= give a fresh spawn the viewer's real
// geometry from birth; ?restart=1 revives a run that was stopped on purpose.
func (s *Server) handleTTYStart(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	cols, rows := geomFromQuery(r)
	if err := s.ensureTTY(r.Context(), runID, cols, rows, wantsRestart(r)); err != nil {
		status, code := ttyErrorStatus(err)
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": runID, "tty": true})
}

// geomFromQuery parses ?cols=&rows= with sane clamps; (0,0) means unknown —
// spawner defaults (plus remembered geometry) apply.
func geomFromQuery(r *http.Request) (uint16, uint16) {
	parse := func(key string, min, max uint16) uint16 {
		v, err := strconv.ParseUint(r.URL.Query().Get(key), 10, 32)
		if err != nil {
			return 0
		}
		if v < uint64(min) || v > uint64(max) {
			return 0
		}
		return uint16(v) //nolint:gosec // bounded by max (a uint16) just above
	}
	return parse("cols", 20, 500), parse("rows", 5, 300)
}

// wantsRestart reports whether the request asks to revive a stopped run.
// Only POST /sessions/{id}/tty honours it; an attach never does.
func wantsRestart(r *http.Request) bool {
	v := r.URL.Query().Get("restart")
	return v == "1" || v == "true"
}

// stoppedDeliberately reports whether an attach must leave the run stopped.
// Runs that finished or crashed on their own resume on attach; runs somebody
// or something stopped do not, or reopening the page would silently undo the
// stop.
func stoppedDeliberately(sess *spawner.Session) bool {
	switch strings.TrimSuffix(sess.StopReason, "_unverified") {
	case "user_stop", "user_kill", "user_cancel", "user_pause", "member_kill", "orphaned", "superseded":
		return true
	}
	// Terminate and DELETE /tty close the PTY without recording a reason, and
	// the spawner writes a stop's reason only once the process is gone, so a
	// stopped row with no reason (yet) is a deliberate stop as well.
	return sess.State == spawner.StateStopped
}

// lookupRun returns the run, waiting for a spawn this server started a moment
// ago to finish registering it. errRunNotFound when there is no such run.
func (s *Server) lookupRun(ctx context.Context, runID string) (*spawner.Session, error) {
	if runID == "" {
		return nil, errRunNotFound
	}
	s.waitPendingSpawn(ctx, runID)
	cur, err := s.spawner.Status(ctx, runID)
	if errors.Is(err, os.ErrNotExist) || (err == nil && cur == nil) {
		return nil, errRunNotFound
	}
	return cur, err
}

// ttyLockKey is what terminal starts for a run serialize on: its claude
// session id when known (every run key resuming one session shares the
// lock), otherwise the run id.
func ttyLockKey(cur *spawner.Session) string {
	if sid := sessionOf(cur); sid != "" {
		return "session:" + sid
	}
	return "run:" + cur.ID
}

// sessionOf is the claude session a run is on: its discovered session id, or
// until discovery lands, the session it was started to resume.
func sessionOf(r *spawner.Session) string {
	if r.SessionID != "" {
		return r.SessionID
	}
	return r.ResumeFrom
}

// ensureTTY is the idempotent start path: a live tty run is only registered
// with the hub; a finished or crashed one is respawned with its stored cwd,
// resuming the previous claude session when the id was captured. A run that
// was stopped on purpose is revived only when restart is set. cols/rows
// (0 = unknown) become a fresh spawn's birth geometry.
func (s *Server) ensureTTY(ctx context.Context, runID string, cols, rows uint16, restart bool) error {
	cur, err := s.lookupRun(ctx, runID)
	if err != nil {
		return err
	}
	unlock := s.ttyLocks.Lock(ttyLockKey(cur))
	defer unlock()
	return s.ensureTTYLocked(ctx, runID, cols, rows, restart)
}

// ensureTTYLocked is ensureTTY for a caller already holding the run's tty lock.
func (s *Server) ensureTTYLocked(ctx context.Context, runID string, cols, rows uint16, restart bool) error {
	if s.ttyLive(runID) {
		// already live: apply the viewer's geometry immediately so anything
		// claude renders from now on wraps for the attaching screen. Routed
		// through the hub so a width change also invalidates the replay ring
		// (mixed-width frames replay mangled).
		if cols > 0 && rows > 0 {
			_ = s.tty.Resize(runID, cols, rows)
		}
		return nil
	}
	cur, err := s.lookupRun(ctx, runID)
	if err != nil {
		return err
	}
	if cur.State.Terminal() && stoppedDeliberately(cur) && !restart {
		return ErrRunStopped
	}
	opts := spawner.Options{Kind: spawner.KindTTY, CreatedBy: "local", Cols: cols, Rows: rows}
	// a live non-tty run is taken over, not hijacked: stop the headless
	// child cleanly (stdin close = exit 0), wait for it to drain, then
	// hand the SAME claude session to the TUI.
	if !cur.State.Terminal() && cur.Kind != spawner.KindTTY {
		_ = s.spawner.CloseStdin(ctx, runID)
		for i := 0; i < 100; i++ { // ≤10s for the child to exit
			c, err := s.spawner.Status(ctx, runID)
			if err != nil || c == nil || c.State.Terminal() {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if c, err := s.spawner.Status(ctx, runID); err == nil && c != nil {
			cur = c
		}
	}
	if !cur.State.Terminal() {
		// A tty run that is not terminal but has no PTY is still being
		// started by the spawner (or its row outlived the process). Give it
		// a moment instead of starting a SECOND TUI for the same run.
		for i := 0; i < 40; i++ {
			time.Sleep(100 * time.Millisecond)
			if s.ttyLive(runID) {
				return nil
			}
			if c, err := s.spawner.Status(ctx, runID); err == nil && c != nil && c.State.Terminal() {
				cur = c
				break
			}
		}
		if cur.State.Terminal() && stoppedDeliberately(cur) && !restart {
			return ErrRunStopped
		}
	}
	opts.Cwd = cur.CWD
	opts.Model = cur.Model
	// The engine the run WAS, not the default. Without this an OpenCode
	// run restarted here came back as claude, which was handed an
	// OpenCode session id and answered "No sessions match ses_...".
	opts.Engine = cur.Engine
	opts.Title = cur.Title
	opts.ApprovalsEnabled = cur.ApprovalsEnabled
	if cur.SessionID != "" {
		if other := s.liveRunForSession(ctx, cur.SessionID, runID); other != nil {
			return errSessionBusy
		}
		opts.ResumeSessionID = cur.SessionID // restart continues the thread
	}
	// Engine knobs (OpenCode's control port) BEFORE the spawner runs: the
	// port has to be in the argv, and the spawner is what builds it.
	if err := s.prepareEngine(ctx, &opts); err != nil {
		return err
	}
	if _, err := s.spawner.StartTTY(ctx, runID, opts); err != nil {
		return err
	}
	if s.ttyLive(runID) {
		return nil
	}
	return errors.New("tty: process started but no PTY registered")
}

// ttyLive reports whether the run has a live PTY, registering the spawner's
// master with the hub if it has not been yet.
func (s *Server) ttyLive(runID string) bool {
	if m, _ := s.spawnerPTY(runID); m != nil {
		return true
	}
	return s.tty.Live(runID)
}

// liveRunForSession returns a live run, other than exceptRunID, that holds
// the claude session sid, or nil.
func (s *Server) liveRunForSession(ctx context.Context, sid, exceptRunID string) *spawner.Session {
	runs, err := s.spawner.List(ctx, "")
	if err != nil {
		return nil
	}
	for _, r := range runs {
		if r != nil && r.ID != exceptRunID && sessionOf(r) == sid && !r.State.Terminal() {
			return r
		}
	}
	return nil
}

// spawnerPTY registers (once) and returns the run's live PTY master. The
// hub learns the PTY's current width so replay bytes are tagged with their
// emission geometry.
func (s *Server) spawnerPTY(runID string) (io.ReadWriteCloser, bool) {
	m, ok := s.spawner.PTYMaster(runID)
	if !ok || m == nil {
		return nil, false
	}
	cols, _, _ := s.spawner.TTYSize(runID)
	s.tty.Register(runID, m, func(cols, rows uint16) error {
		return s.spawner.ResizeTTY(runID, cols, rows)
	}, cols)
	return m, true
}

// handleTTYBySession is the session→terminal bridge: given a CLAUDE session
// id (any session — managed run or a plain local transcript), return a live
// tty run id for it. Existing managed runs are attached (and resumed as the
// TUI when they finished or crashed); unmanaged sessions spawn a fresh tty run
// resuming the claude session, so every session is resumable as a terminal.
// A run stopped on purpose stays stopped (410), and a session a live run
// already holds is never started a second time.
func (s *Server) handleTTYBySession(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if sid == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing session id")
		return
	}
	createdBy := "local"
	if dev := deviceFrom(r.Context()); dev != nil {
		createdBy = "device:" + dev.ID
	}
	unlock := s.ttyLocks.Lock("session:" + sid)
	defer unlock()

	ctx := r.Context()
	cols, rows := geomFromQuery(r)
	cur, _ := s.spawner.ListBySessionID(ctx, sid)
	if live := s.liveRunForSession(ctx, sid, ""); live != nil {
		cur = live
	}
	if cur != nil {
		err := s.ensureTTYLocked(ctx, cur.ID, cols, rows, false)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"id": cur.ID, "sessionId": sid})
			return
		}
		if errors.Is(err, ErrRunStopped) || errors.Is(err, errSessionBusy) || s.liveRunForSession(ctx, sid, "") != nil {
			status, code := ttyErrorStatus(err)
			writeError(w, status, code, err.Error())
			return
		}
		s.log.Info("agentd: resuming session under a fresh run key", "session", sid, "run", cur.ID, "err", err)
	}
	opts := spawner.Options{
		Kind: spawner.KindTTY, ResumeSessionID: sid, CreatedBy: createdBy,
		Cols: cols, Rows: rows,
	}
	if cur != nil {
		opts.Cwd, opts.Model = cur.CWD, cur.Model
		// Same as ensureTTY: resume on the engine the session belongs to.
		opts.Engine, opts.Title = cur.Engine, cur.Title
		opts.ApprovalsEnabled = cur.ApprovalsEnabled
	} else if idx, err := s.st.GetSession(ctx, sid); err == nil && idx != nil && idx.Cwd != "" {
		// plain (unmanaged) transcript: the sessions index knows where it
		// lived — resume it THERE. Falling through to agentd's process cwd
		// spawned terminals inside agentflow's own repo (real bug).
		opts.Cwd = idx.Cwd
		// The corpus knows which engine wrote the transcript.
		opts.Engine = idx.Engine
	}
	if err := s.prepareEngine(ctx, &opts); err != nil {
		writeError(w, http.StatusBadRequest, "engine_prepare_failed", err.Error())
		return
	}
	sess, err := s.spawner.Start(ctx, opts)
	if err != nil || sess == nil {
		writeError(w, http.StatusConflict, "tty_failed", "could not start terminal for session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": sess.ID, "sessionId": sid})
}

// handleTTYStop terminates the run's PTY session (DELETE /tty).
func (s *Server) handleTTYStop(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if err := s.spawner.CloseStdin(r.Context(), runID); err != nil &&
		!strings.Contains(err.Error(), "no live process") {
		writeError(w, http.StatusConflict, "tty_stop_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": runID})
}

// ttyControlFrame is the browser→agentd text-frame contract on the tty WS.
//
// "submit" is the composer's send: unlike "input" it does not hand the TUI
// one chunk of text-plus-CR, which a TUI reads as a paste whose newline
// lands in the prompt box instead of submitting it.
type ttyControlFrame struct {
	Type string `json:"type"` // "input" | "resize" | "submit"
	Data string `json:"data,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	// Bracketed wraps a submit's text in DECSET 2004 paste markers; the
	// client sets it from the TUI's own bracketed-paste mode.
	Bracketed bool `json:"bracketed,omitempty"`
}

// handleTTYWS upgrades a browser WebSocket onto a run's PTY: binary frames
// downstream are raw TUI output; text frames upstream are ttyControlFrame
// JSON (input bytes / resize / submit).
//
// Order: token, Origin, run lookup, upgrade, and only then attach or start.
// Nothing is looked up for an unauthenticated or cross-site request, and
// nothing is started for a run that does not exist or was stopped.
func (s *Server) handleTTYWS(w http.ResponseWriter, r *http.Request) {
	d, tok := s.wsDevice(w, r)
	if d == nil {
		return
	}
	runID := r.PathValue("id")
	cur, err := s.lookupRun(r.Context(), runID)
	if err != nil {
		status, code := ttyErrorStatus(err)
		writeError(w, status, code, err.Error())
		return
	}
	if cur.State.Terminal() && stoppedDeliberately(cur) && !s.ttyLive(runID) {
		writeError(w, http.StatusGone, "run_stopped", ErrRunStopped.Error())
		return
	}
	conn, err := s.upgrader().Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxTTYFrame)
	cols, rows := geomFromQuery(r)
	if err := s.ensureTTY(r.Context(), runID, cols, rows, false); err != nil {
		_, code := ttyErrorStatus(err)
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, code),
			time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	s.serveTTYWS(conn, runID, d.ID, tok)
}

func (s *Server) serveTTYWS(conn *websocket.Conn, runID, deviceID, token string) {
	c := s.wsRegistry.Register(deviceID, "tty", conn)
	defer s.wsRegistry.Unregister(c)
	defer func() { _ = conn.Close() }()
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go s.watchDevice(watchCtx, c, token)

	// Retry Subscribe briefly: the attach can land a beat before a freshly
	// spawned TUI's master is registered with the hub. Without the retry the
	// socket closes a few ms after the upgrade and the client redials forever.
	var (
		replay []byte
		out    <-chan []byte
		unsub  func()
		err    error
	)
	for i := 0; i < 40; i++ {
		replay, out, unsub, err = s.tty.Subscribe(runID)
		if err == nil {
			break
		}
		// A run that has ended will not come back; stop waiting.
		if cur, sErr := s.spawner.Status(context.Background(), runID); sErr == nil && cur != nil && cur.State.Terminal() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return
	}
	defer unsub()

	// downstream: replay ring first (the attach's recent past), then live PTY
	// bytes as binary frames
	for off := 0; off < len(replay); off += 32 * 1024 {
		end := min(off+32*1024, len(replay))
		_ = conn.SetWriteDeadline(deadlineSoon())
		if err := conn.WriteMessage(websocket.BinaryMessage, replay[off:end]); err != nil {
			return
		}
	}

	// stop tells the writer and the pinger the handler is leaving. The
	// deferred calls run in reverse: stop them, close the socket (which
	// unblocks a write in flight), then wait for both.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() { _ = conn.Close() }()
	defer close(stop)

	// The writer is the only goroutine that calls WriteMessage; pings go
	// through WriteControl, which gorilla allows alongside one writer.
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case chunk, ok := <-out:
				if !ok {
					// master closed (or this viewer was too slow and kicked):
					// tell the browser, then drop the socket so it redials
					// into the replay ring.
					_ = conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseNormalClosure, "pty closed"),
						time.Now().Add(time.Second))
					_ = conn.Close()
					return
				}
				_ = conn.SetWriteDeadline(deadlineSoon())
				if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		t := time.NewTicker(wsPingPeriod)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	// upstream: binary frames are raw input, text frames are control JSON.
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	for {
		mt, raw, err := conn.ReadMessage()
		if err != nil || c.Closed() {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
		switch mt {
		case websocket.BinaryMessage:
			_ = s.tty.Write(runID, raw)
		case websocket.TextMessage:
			var f ttyControlFrame
			if err := json.Unmarshal(raw, &f); err != nil {
				continue
			}
			switch f.Type {
			case "input":
				_ = s.tty.Write(runID, []byte(f.Data))
			case "resize":
				_ = s.tty.Resize(runID, f.Cols, f.Rows)
			case "submit":
				_ = s.tty.Submit(runID, f.Data, f.Bracketed)
			}
		}
	}
}

// keyedMutex hands out one mutex per key and forgets it once nobody holds or
// waits for it.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// Lock locks key and returns its unlock func.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
