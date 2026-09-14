package store

import (
	"sync"
	"testing"
	"time"
)

// Presence is the truth about who is holding a line, so the leave must run on
// every exit including the one nobody chose: a dropped connection unwinds the
// handler, and if the entry outlived that, a dead client would read as
// listening forever, which is worse than no presence at all.
func TestPresenceLeavesOnEveryExit(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()

	if _, ok := s.Listening("m1"); ok {
		t.Fatal("nothing is listening before anything parks")
	}
	leave := s.EnterPolling("m1")
	since, ok := s.Listening("m1")
	if !ok {
		t.Fatal("a parked poll must be listening")
	}
	if since == 0 {
		t.Fatal("listening must carry when it started")
	}
	leave()
	if _, ok := s.Listening("m1"); ok {
		t.Fatal("the leave must remove it")
	}
	// Double leave is what a defer plus an explicit call looks like, and it
	// must not underflow into a negative depth.
	leave()
	if _, ok := s.Listening("m1"); ok {
		t.Fatal("a second leave must not resurrect it")
	}
}

// One member may hold two polls across a reconnect. The second leaving must
// not erase the first, or a reconnecting agent flickers out of presence at
// exactly the moment the sweep looks.
func TestPresenceIsReentrantPerMember(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()

	first := s.EnterPolling("m1")
	second := s.EnterPolling("m1")
	second()
	if _, ok := s.Listening("m1"); !ok {
		t.Fatal("one poll leaving must not evict a member that still holds another")
	}
	first()
	if _, ok := s.Listening("m1"); ok {
		t.Fatal("the last leave evicts")
	}
}

// The registry is touched by every parked poll and by the sweep on another
// goroutine. Run with -race: a data race here is a heisenbug that looks like
// an agent randomly reading as stranded.
func TestPresenceUnderConcurrentPollsAndSweeps(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// The sweep, snapshotting while everything churns.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for id := range s.ListeningNow() {
				_, _ = s.Listening(id)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "member-" + string(rune('a'+n%8))
			for j := 0; j < 50; j++ {
				leave := s.EnterPolling(id)
				time.Sleep(time.Microsecond)
				leave()
			}
		}(i)
	}
	// Let the pollers finish before stopping the sweep.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	<-done

	if left := s.ListeningNow(); len(left) != 0 {
		t.Fatalf("every poll left, so the registry must be empty, got %v", left)
	}
}
