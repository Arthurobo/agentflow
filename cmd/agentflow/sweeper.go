package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/engine/opencode"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// sweepInterval is how often leases are expired and presence is judged.
//
// It has to be well under the 90 second stranded grace so a transition is
// noticed promptly, and well over the cost of one pass, which is a handful of
// indexed queries per open loop.
const sweepInterval = 30 * time.Second

// nudgeRetry is how long before a member that is STILL stranded is nudged
// again. Once per transition would leave a member that ignored the first nudge
// silent forever; every tick would be a keystroke every thirty seconds into a
// terminal the engineer may be reading.
const nudgeRetry = 5 * time.Minute

// nudger writes one line into a stranded member's TUI.
//
// It is the last resort behind the courier, which delivers the mail itself.
// A member only reaches the sweep's stranded state when delivery did not
// happen, so this says "there is mail you have not got" and nothing more.
//
// It is an accelerator on the subset of processes agentd owns, and the system
// must be correct with it removed: an engineer continuing work from their own
// laptop in a session agentd never spawned gets no nudge and is reached by the
// detector, the orchestrator's notice and the board instead. It carries ONE
// line and never a message body, because the bus is the only contract every
// agent speaks and a body arriving by two routes is two sources of truth.
type nudger struct {
	// submit types a line into a member's terminal and presses Enter as a
	// separate write after a pause, exactly as the composer does. A line
	// with its carriage return glued on arrives as a paste, and a TUI that
	// treats a paste as text leaves it sitting unsent in the prompt box.
	submit  func(runID, text string) error
	sp      *spawner.Spawner
	engines *engine.Registry
	log     *slog.Logger

	mu   sync.Mutex
	last map[string]time.Time
}

func newNudger(submit func(runID, text string) error, sp *spawner.Spawner, engines *engine.Registry, log *slog.Logger) *nudger {
	return &nudger{submit: submit, sp: sp, engines: engines, log: log, last: map[string]time.Time{}}
}

// nudgeLine is the whole payload. It says a fact and deliberately says
// nothing about who sent what.
//
// It no longer names a polling command. The courier delivers mail into the
// terminal, so a member is told never to block on a poll; a nudge that said
// "run WAIT" would be the one instruction contradicting its own brief. This
// fires only for a member the courier could NOT reach, which is why it points
// at the one-shot fallback rather than at nothing.
const nudgeLine = "You have mail that was not delivered. Fetch it once."

// Nudge pokes one stranded member, and reports whether it did. A member with
// no run gets nothing: there is no process here to poke.
func (n *nudger) Nudge(ctx context.Context, m store.StrandedMember) bool {
	if m.RunID == "" {
		return false
	}
	if !n.due(m.MemberID) {
		return false
	}
	switch store.NormaliseTool(m.Tool) {
	case store.ToolOpenCode:
		return n.nudgeOpenCode(ctx, m)
	default:
		if n.submit == nil {
			return false
		}
		if err := n.submit(m.RunID, nudgeLine); err != nil {
			n.log.Debug("agentd: nudge", "run", m.RunID, "err", err)
			return false
		}
		n.mark(m.MemberID)
		return true
	}
}

func (n *nudger) nudgeOpenCode(ctx context.Context, m store.StrandedMember) bool {
	if n.sp == nil {
		return false
	}
	run, err := n.sp.Status(ctx, m.RunID)
	if err != nil || run == nil || run.ControlBase == "" {
		return false
	}
	c := opencode.NewControl(run.ControlBase)
	if run.SessionID != "" {
		c.BindSession(run.SessionID)
	}
	if err := c.Prompt(ctx, nudgeLine); err != nil {
		n.log.Debug("agentd: nudge opencode", "run", m.RunID, "err", err)
		return false
	}
	n.mark(m.MemberID)
	return true
}

func (n *nudger) due(memberID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	at, ok := n.last[memberID]
	return !ok || time.Since(at) >= nudgeRetry
}

func (n *nudger) mark(memberID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	n.last[memberID] = now
	// A member nudged longer ago than the retry interval is due again
	// anyway, so its entry carries nothing and can go.
	for id, at := range n.last {
		if now.Sub(at) > nudgeRetry {
			delete(n.last, id)
		}
	}
}

// runSweeper is the ticker. RequeueExpiredMail had no caller anywhere outside
// its own definition, so leases were decorative; this is that caller. It is
// also what ends a loop agentd decides is over, stopping its members through
// stopRuns, the same path an engineer's end takes.
func runSweeper(ctx context.Context, st *store.Store, n *nudger, rep *reporter,
	stopRuns func(runIDs []string) int, log *slog.Logger) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	days := retentionDays()
	var pruned time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		sweepOnce(ctx, st, n, rep, stopRuns, log)
		if days > 0 && time.Since(pruned) >= retentionEvery {
			pruned = time.Now()
			pruneIndex(ctx, st, days, log)
		}
	}
}

// retentionEvery is how often the session index is pruned.
const retentionEvery = 24 * time.Hour

// retentionDays reads AF_RETENTION_DAYS: how many days a session nobody has
// touched stays in the index. Unset or unreadable means the default; 0 keeps
// everything.
func retentionDays() int {
	v := strings.TrimSpace(os.Getenv("AF_RETENTION_DAYS"))
	if v == "" {
		return store.DefaultRetentionDays
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return store.DefaultRetentionDays
	}
	return n
}

// pruneIndex drops the indexed history of sessions untouched for days.
func pruneIndex(ctx context.Context, st *store.Store, days int, log *slog.Logger) {
	rep, err := st.PruneSessionsNotUpdatedSince(ctx, time.Now().AddDate(0, 0, -days))
	if err != nil {
		log.Warn("agentd: retention", "err", err)
		return
	}
	if rep.Sessions > 0 || rep.Blobs > 0 {
		log.Info("agentd: pruned old session history", "days", days,
			"sessions", rep.Sessions, "events", rep.Events, "blobs", rep.Blobs)
	}
}

// sweepOnce is one pass of the sweeper.
func sweepOnce(ctx context.Context, st *store.Store, n *nudger, rep *reporter,
	stopRuns func(runIDs []string) int, log *slog.Logger) {
	finishLoops(ctx, st, stopRuns, log)
	res, err := st.Sweep(ctx)
	if err != nil {
		log.Warn("agentd: sweep", "err", err)
		return
	}
	if res.Requeued > 0 {
		log.Info("agentd: expired leases returned to the queue", "count", res.Requeued)
	}
	for _, m := range res.Stranded {
		log.Warn("agentd: nobody is reading a member's mail",
			"loop", m.LoopID, "role", m.Role, "run", m.RunID)
		if n != nil {
			n.Nudge(ctx, m)
		}
	}
	// A member that did the work and did not send it. Unlike every other
	// signal here this one is not advisory: the loop cannot move until that
	// report reaches the orchestrator, so the reporter asks once and then
	// hands the output over itself.
	for _, u := range res.Unreported {
		if rep != nil {
			rep.Handle(ctx, u)
		}
	}
}

// finishLoops ends every active loop that is over one of its limits or whose
// play has finished, and stops its members' processes.
//
// The runs are read before the loop ends: ending it parks the members, and a
// member parked idle still has a process that has to be stopped.
func finishLoops(ctx context.Context, st *store.Store, stopRuns func(runIDs []string) int, log *slog.Logger) {
	due, err := st.LoopsToFinish(ctx)
	if err != nil {
		log.Warn("agentd: loop limits", "err", err)
		return
	}
	for _, f := range due {
		runs, err := st.LiveMemberRunIDs(ctx, f.LoopID)
		if err != nil {
			log.Warn("agentd: list member runs", "loop", f.LoopID, "err", err)
			continue
		}
		ended, err := st.FinishLoop(ctx, f.LoopID, f.Status, f.Reason)
		if err != nil {
			log.Warn("agentd: end loop", "loop", f.LoopID, "err", err)
			continue
		}
		if !ended {
			continue // someone else ended it first, and stopped what they meant to
		}
		stopped := 0
		if stopRuns != nil {
			stopped = stopRuns(runs)
		}
		log.Warn("agentd: ended a loop", "loop", f.LoopID, "status", f.Status,
			"reason", f.Reason, "members_stopped", stopped)
	}
}
