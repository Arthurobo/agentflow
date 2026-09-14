package main

import (
	"context"
	"log/slog"

	"github.com/arthurobo/agentflow/internal/store"
)

// orphanReaper is the slice of the spawner the boot reconciler needs. An
// interface rather than *spawner.Spawner so the scan is testable without
// spawning a process.
type orphanReaper interface {
	Generation() string
	Reap(ctx context.Context, runID string) error
}

// reapOrphans is the boot reconciler.
//
// Every agentd boot mints a generation. Any managed row still in a live
// state whose generation is not this boot's belongs to a previous agentd
// life — nobody owns its process any more, and nothing else in the daemon
// will ever flip it. Without this, a restart left rows reading "running"
// forever: the UI listed dead agents as live, and the sweep re-confirmed
// death on a corpse every cycle.
//
// It runs BEFORE anything serves, so no client can act on a row that is
// about to be reaped, and no revive path can resurrect one.
//
// Reap routes through the single termination primitive, which identity-
// checks each row against /proc first: a PID that is gone, or that has been
// recycled by an unrelated process, is written terminal WITHOUT being
// signaled. Killing a stranger because a stale row named its PID is the one
// outcome worse than a stale row.
//
// Best-effort by design: a scan that fails must not stop the daemon
// booting, and one row that will not die must not stop the rest.
func reapOrphans(ctx context.Context, st *store.Store, sp orphanReaper, log *slog.Logger) int {
	if st == nil || sp == nil {
		return 0
	}
	gen := sp.Generation()
	if gen == "" {
		// Without a generation every row looks foreign, so a reaper that ran
		// anyway would kill the runs this boot just started.
		log.Warn("agentd: no boot generation; skipping the orphan scan")
		return 0
	}
	orphans, err := st.ListActiveManagedSessionsNotInGeneration(ctx, gen)
	if err != nil {
		log.Warn("agentd: orphan scan failed", "err", err)
		return 0
	}
	if len(orphans) == 0 {
		return 0
	}
	log.Info("agentd: reaping orphaned runs from a previous generation",
		"count", len(orphans), "generation", gen)
	reaped := 0
	for _, m := range orphans {
		if err := sp.Reap(ctx, m.ID); err != nil {
			log.Warn("agentd: reap failed", "run", m.ID, "err", err)
			continue
		}
		reaped++
		log.Info("agentd: reaped orphan", "run", m.ID, "state", m.State,
			"pid", m.PID, "was_generation", m.Generation)
	}
	return reaped
}

// loopReaper is the slice of the daemon the loop sweep needs: what
// generation is current. Separate from orphanReaper because ending a loop is
// a store write, not a process kill — no spawner is involved.
type loopReaper interface {
	Generation() string
}

// reapOrphanLoops is the loop half of the boot reconciler.
//
// reapOrphans marks a previous life's member RUNS terminal. Nothing marked
// the parent loop, because the loops table carried no generation until
// migration 0027 — so a restarted loop kept status 'active' with every
// member already dead. Six of them had survived roughly ten hours and many
// restarts on the live database when this was written.
//
// A stale 'active' loop is not merely untidy. Store.Sweep iterates
// ListLoops(LoopActive) every cycle, so each of those corpses was still
// being leased, still having its members marked stranded, and still having
// stranded announcements posted into a mailbox nobody was reading.
//
// It runs BEFORE anything serves, alongside the managed sweep, so no client
// can act on a loop that is about to be ended.
//
// The loop is ended, NOT dismissed: interrupted is a thing that happened to
// it and should stay legible in the list. Dismissing would retire the
// members and make the loop vanish, which loses the evidence.
//
// Best-effort by design: a scan that fails must not stop the daemon booting,
// and one loop that will not end must not stop the rest.
func reapOrphanLoops(ctx context.Context, st *store.Store, sp loopReaper, log *slog.Logger) int {
	if st == nil || sp == nil {
		return 0
	}
	gen := sp.Generation()
	if gen == "" {
		// Without a generation every loop looks foreign, so a reaper that
		// ran anyway would end the loops this boot is about to start.
		log.Warn("agentd: no boot generation; skipping the orphan loop scan")
		return 0
	}
	orphans, err := st.ListActiveLoopsNotInGeneration(ctx, gen)
	if err != nil {
		log.Warn("agentd: orphan loop scan failed", "err", err)
		return 0
	}
	if len(orphans) == 0 {
		return 0
	}
	log.Info("agentd: ending loops orphaned by a previous generation",
		"count", len(orphans), "generation", gen)
	ended := 0
	for _, l := range orphans {
		was := l.Generation
		if was == "" {
			was = "(unstamped)"
		}
		reason := "agentd restarted (generation " + was + " -> " + gen + ")"
		if _, err := st.EndLoop(ctx, l.ID, store.LoopInterrupted, reason, false); err != nil {
			log.Warn("agentd: ending an orphan loop failed", "loop", l.ID, "err", err)
			continue
		}
		ended++
		log.Info("agentd: ended orphan loop", "loop", l.ID, "title", l.Title,
			"was_status", l.Status, "was_generation", was)
	}
	return ended
}
