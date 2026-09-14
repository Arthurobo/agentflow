// Package ingest is the single writer for the session index. Everything that
// adds events to it (a headless run's stream, an OpenCode run's event stream,
// the OpenCode storage backfill) hands batches to one goroutine, which
// commits them with store.InsertBatch, so SQLite sees one writer for the
// index however many runs are producing.
package ingest

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

// Service funnels every index write through a single writer goroutine.
type Service struct {
	st *store.Store

	wch chan *writeReq

	// pending counts batches enqueued but not yet committed, so a caller
	// can wait for everything it handed over to be written.
	pending atomic.Int64

	startOnce sync.Once
}

// writeReq is one batch handed to the writer.
type writeReq struct {
	meta   *store.SessionMeta
	events []store.Incoming
	blobs  []store.Blob
}

// Options once configured the Claude corpus tailer and backfill, which were
// never started by the shipped daemon and are gone. It survives only so
// callers that still pass it compile; neither field has any effect.
type Options struct {
	CorpusRoot string
	Live       bool
}

// New creates a Service over st. The writer starts with the first batch.
func New(st *store.Store, _ Options) *Service {
	return &Service{st: st, wch: make(chan *writeReq, 256)}
}

// Store returns the underlying store.
func (s *Service) Store() *store.Store { return s.st }

// EnqueueLive hands a batch to the single writer. It returns once the batch
// is queued, blocking while the queue is full; durability is the writer's
// commit.
func (s *Service) EnqueueLive(ctx context.Context, evs []store.Incoming, meta *store.SessionMeta) error {
	if len(evs) == 0 {
		return nil
	}
	s.start()
	req := &writeReq{meta: meta, events: evs, blobs: collectBlobs(evs)}
	s.pending.Add(1)
	select {
	case s.wch <- req:
		return nil
	case <-ctx.Done():
		s.pending.Add(-1)
		return ctx.Err()
	}
}

// Flush waits until every batch enqueued so far has been committed, or ctx
// ends.
func (s *Service) Flush(ctx context.Context) error {
	for s.pending.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}

// start runs the writer goroutine once. The writer lives for the process,
// never for one caller's context: a short-lived request context must not be
// able to kill it mid-commit.
func (s *Service) start() {
	s.startOnce.Do(func() { go s.writer() })
}

// writer consumes batches and commits them. Single goroutine => the index
// never contends with itself for the SQLite write lock.
func (s *Service) writer() {
	for req := range s.wch {
		// Coalesce what is already queued, but only a little: every events
		// row fires the search trigger inside the transaction, so a large
		// batch holds the write lock long enough for other writers (device
		// pairing, loop mail) to time out.
		batch := []*writeReq{req}
	drain:
		for len(batch) < 16 {
			select {
			case more := <-s.wch:
				batch = append(batch, more)
			default:
				break drain
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		for _, r := range batch {
			s.writeOne(ctx, r)
			s.pending.Add(-1)
		}
		cancel()
	}
}

// writeOne commits one batch.
func (s *Service) writeOne(ctx context.Context, r *writeReq) {
	b := &store.Batch{Events: r.events, Blobs: r.blobs, Session: r.meta}
	if err := s.st.InsertBatch(ctx, b); err != nil {
		path := ""
		if r.meta != nil {
			path = r.meta.FilePath
		}
		fmt.Fprintf(os.Stderr, "ingest: insert batch for %s: %v\n", path, err)
	}
}

// collectBlobs extracts blob rows from oversized events (contentHash is the
// parser's pre-truncation hash — the blob key matches events.content_hash).
func collectBlobs(evs []store.Incoming) []store.Blob {
	var out []store.Blob
	for i := range evs {
		ev := &evs[i]
		if ev.Oversized && ev.BlobContent != "" && ev.ContentHash != "" {
			out = append(out, store.Blob{Hash: ev.ContentHash, Content: ev.BlobContent})
		}
	}
	return out
}
