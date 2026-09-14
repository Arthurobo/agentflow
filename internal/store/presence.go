// presence.go — who is holding a long poll open right now.
//
// The mailbox could not tell a member that was working from one that had
// stopped polling, so a stranded agent looked exactly like a busy one. The
// truth is the set of long polls THIS process is holding, which is why it
// lives in memory rather than in a column.
//
// A polling_since column would be a lie the moment a connection drops or
// agentd restarts, because the clear would never run. An empty registry after
// a restart is not a gap: it says nobody is holding a line, which is true, and
// every live member re-polls within seconds.

package store

import (
	"sync"
	"time"
)

type presence struct {
	mu    sync.Mutex
	since map[string]int64
	depth map[string]int
}

func (s *Store) presenceReg() *presence {
	s.presenceOnce.Do(func() {
		s.presence = &presence{since: map[string]int64{}, depth: map[string]int{}}
	})
	return s.presence
}

// EnterPolling records that a long poll for this member is open, and returns
// the leave. The caller MUST defer it: a dropped connection unwinds the
// handler, so the deferred leave is what makes a dead client stop counting as
// present.
//
// Reentrant by count, because one member may legitimately have two polls in
// flight across a reconnect, and the second must not erase the first.
func (s *Store) EnterPolling(memberID string) func() {
	if memberID == "" {
		return func() {}
	}
	p := s.presenceReg()
	p.mu.Lock()
	p.depth[memberID]++
	if p.depth[memberID] == 1 {
		p.since[memberID] = time.Now().UnixMilli()
	}
	p.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.depth[memberID]--
			if p.depth[memberID] <= 0 {
				delete(p.depth, memberID)
				delete(p.since, memberID)
			}
		})
	}
}

// Listening reports whether a long poll for this member is open right now, and
// since when.
func (s *Store) Listening(memberID string) (int64, bool) {
	p := s.presenceReg()
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.since[memberID]
	return at, ok
}

// ListeningNow snapshots the whole registry, for a sweep that would otherwise
// take and release the lock once per member.
func (s *Store) ListeningNow() map[string]int64 {
	p := s.presenceReg()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int64, len(p.since))
	for id, at := range p.since {
		out[id] = at
	}
	return out
}
