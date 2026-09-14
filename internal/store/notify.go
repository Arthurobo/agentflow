// notify.go — the wake-up signal a long poll waits on. It exists so a poll
// never has to hold a database connection while it waits: the pool is four
// connections and a poll is held for minutes, so a handler that waits with a
// connection in hand deadlocks the whole application on the fourth agent.

package store

import "sync"

type mailNotifier struct {
	mu      sync.Mutex
	waiters map[string][]chan struct{}
	// watchers are subscribers to EVERY member's mail rather than one
	// member's. A long poll knows which mailbox it is waiting on; the
	// courier does not, because it delivers to whoever mail arrives for.
	watchers map[int]chan string
	nextID   int
}

func (s *Store) notifier() *mailNotifier {
	s.notifyOnce.Do(func() {
		s.notify = &mailNotifier{
			waiters:  map[string][]chan struct{}{},
			watchers: map[int]chan string{},
		}
	})
	return s.notify
}

// WaitForMail returns a channel closed when mail lands for this member, plus a
// release the caller MUST call. Nothing here touches the database, which is
// the whole point.
func (s *Store) WaitForMail(memberID string) (<-chan struct{}, func()) {
	n := s.notifier()
	ch := make(chan struct{})
	n.mu.Lock()
	n.waiters[memberID] = append(n.waiters[memberID], ch)
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		list := n.waiters[memberID]
		for i, c := range list {
			if c == ch {
				n.waiters[memberID] = append(list[:i:i], list[i+1:]...)
				break
			}
		}
		if len(n.waiters[memberID]) == 0 {
			delete(n.waiters, memberID)
		}
	}
}

// WatchMail subscribes to mail landing for ANY member, yielding the member id.
// Returns the channel and a release the caller MUST call.
//
// This is what lets agentd deliver a message the moment it is committed
// instead of waiting for the next tick. It is lossy on purpose: the channel
// is buffered and a send that would block is dropped, because a subscriber
// that has fallen behind will find the same mail with its next sweep, and
// blocking here would block the commit path of every producer in the process.
func (s *Store) WatchMail() (<-chan string, func()) {
	n := s.notifier()
	ch := make(chan string, 256)
	n.mu.Lock()
	id := n.nextID
	n.nextID++
	n.watchers[id] = ch
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.watchers, id)
	}
}

// notifyMail wakes every poller waiting on this member, and every watcher.
// Called after a message row is committed, never before: a waiter woken early
// would find nothing and go back to sleep having burned a query.
func (s *Store) notifyMail(memberIDs ...string) {
	n := s.notifier()
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, id := range memberIDs {
		for _, ch := range n.waiters[id] {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
		delete(n.waiters, id)
		for _, w := range n.watchers {
			select {
			case w <- id:
			default: // a slow watcher catches up on its own sweep
			}
		}
	}
}

// WaiterCount reports how many pollers are parked, for tests and diagnostics.
func (s *Store) WaiterCount() int {
	n := s.notifier()
	n.mu.Lock()
	defer n.mu.Unlock()
	total := 0
	for _, list := range n.waiters {
		total += len(list)
	}
	return total
}
