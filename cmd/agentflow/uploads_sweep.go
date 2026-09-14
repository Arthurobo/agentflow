package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
	"github.com/arthurobo/agentflow/internal/uploads"
)

// sweepEvery is how often the retention sweep runs after boot.
const sweepEvery = 6 * time.Hour

// runUploadSweeper deletes expired and orphaned attachments on boot and every
// six hours after, using the same uploads.Store the API writes through, so
// the sweep and the disk quota agree about what is on disk.
//
// Attachments are screenshots someone pushed at an agent from their phone.
// They are worth keeping for as long as the conversation is live and worth
// nothing afterwards, so they expire on a clock (7 days by default) and
// whenever the run they belonged to is gone.
//
// It NEVER fails boot. A sweep that cannot run is a disk that fills slowly;
// a boot that will not complete is every session on the machine unreachable,
// and those are not the same size of problem.
//
// up must be the same instance the upload channels use (the control API's
// Uploads()), so the disk total they enforce drops when files are swept.
func runUploadSweeper(ctx context.Context, st *store.Store, up *uploads.Store, log *slog.Logger) {
	if st == nil || up == nil {
		return
	}
	sweep := func() {
		defer func() {
			// A panic in a background cleaner must not take the daemon with
			// it. The next tick tries again.
			if r := recover(); r != nil {
				log.Error("uploads: sweep panicked", "panic", r)
			}
		}()
		removed, bytes, err := up.Sweep(ctx, st, time.Now())
		if err != nil {
			log.Warn("uploads: sweep failed", "err", err)
			return
		}
		if removed > 0 {
			log.Info("uploads: swept expired attachments",
				"files", removed, "bytes", bytes,
				"retention_days", up.Limits().RetentionDays, "root", up.Root())
		}
	}

	go func() {
		sweep()
		t := time.NewTicker(sweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sweep()
			}
		}
	}()
}
