package agentapi

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ioPipe builds a fake PTY: the hub holds the read side (its "master"), the
// test writes process output through the write side.
func ioPipe() (io.ReadWriteCloser, io.WriteCloser) {
	r, w, _ := os.Pipe()
	return r, w
}

// TestTTYHubFanOut registers a fake PTY (an os.Pipe pair), attaches two
// subscribers, writes to the master and asserts both receive the bytes; then
// closes the master and asserts both subscriber channels close.
func TestTTYHubFanOut(t *testing.T) {
	hub := newTTYHub(slog.New(slog.NewTextHandler(io.Discard, nil)))

	master, fakeProc := ioPipe()
	hub.Register("run-1", master, nil, 0)

	sub1Replay, sub1, unsub1, err := hub.Subscribe("run-1")
	if err != nil {
		t.Fatalf("subscribe 1: %v", err)
	}
	defer unsub1()
	if len(sub1Replay) != 0 {
		t.Errorf("subscriber 1: fresh session must have empty replay, got %q", sub1Replay)
	}
	sub2Replay, sub2, unsub2, err := hub.Subscribe("run-1")
	if err != nil {
		t.Fatalf("subscribe 2: %v", err)
	}
	defer unsub2()
	if len(sub2Replay) != 0 {
		t.Errorf("subscriber 2: fresh session must have empty replay, got %q", sub2Replay)
	}

	if _, err := fakeProc.Write([]byte("hello pty")); err != nil {
		t.Fatalf("write: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	got := make([]string, 2)
	for i, ch := range [](<-chan []byte){sub1, sub2} {
		go func(i int, ch <-chan []byte) {
			defer wg.Done()
			select {
			case b := <-ch:
				got[i] = string(b)
			case <-time.After(2 * time.Second):
			}
		}(i, ch)
	}
	wg.Wait()
	for i, g := range got {
		if g != "hello pty" {
			t.Errorf("subscriber %d got %q, want %q", i, g, "hello pty")
		}
	}

	// master close (process exit) tears every subscriber down
	if err := fakeProc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for i, ch := range [](<-chan []byte){sub1, sub2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("subscriber %d: channel must close on master EOF", i)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("subscriber %d: channel never closed", i)
		}
	}
}

// TestTTYHubReplayOnLateSubscribe writes output BEFORE anyone attaches, then
// subscribes: the returned replay must contain the early bytes, and the live
// channel must deliver everything written after.
func TestTTYHubReplayOnLateSubscribe(t *testing.T) {
	hub := newTTYHub(slog.New(slog.NewTextHandler(io.Discard, nil)))

	master, fakeProc := ioPipe()
	defer func() { _ = fakeProc.Close() }()
	hub.Register("run-1", master, nil, 0)

	if _, err := fakeProc.Write([]byte("early history")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// give the read loop a beat to ingest the write
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hub.mu.Lock()
		n := len(hub.sessions["run-1"].replay)
		hub.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	replay, live, unsub, err := hub.Subscribe("run-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer unsub()
	if string(replay) != "early history" {
		t.Fatalf("replay = %q, want %q", replay, "early history")
	}

	if _, err := fakeProc.Write([]byte(" and live")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case b := <-live:
		if string(b) != " and live" {
			t.Errorf("live chunk = %q, want %q", b, " and live")
		}
	case <-time.After(2 * time.Second):
		t.Error("live chunk never arrived")
	}
}

// TestTTYHubSlowConsumerKicked floods a subscriber that never reads: the hub
// must KICK it (closed channel) rather than silently dropping bytes, and a
// fresh subscriber afterwards must still work with full replay.
func TestTTYHubSlowConsumerKicked(t *testing.T) {
	hub := newTTYHub(slog.New(slog.NewTextHandler(io.Discard, nil)))

	master, fakeProc := ioPipe()
	defer func() { _ = fakeProc.Close() }()
	hub.Register("run-1", master, nil, 0)

	_, slow, slowUnsub, err := hub.Subscribe("run-1")
	if err != nil {
		t.Fatalf("subscribe slow: %v", err)
	}
	defer slowUnsub()

	payload := make([]byte, 32*1024)
	// Volume, not chunk count: reads may coalesce many small writes into one
	// 32KB chunk, so guarantee overflow by writing enough bytes that EVEN IF
	// every read saturates at 32KB the chunk stream exceeds ttySubBuffer.
	//
	// Nobody reads `slow` during the flood — a concurrent reader would be a
	// fast consumer and the channel would never fill.
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		for i := 0; i < 320; i++ {
			if _, err := fakeProc.Write(payload); err != nil {
				return
			}
		}
	}()
	<-floodDone

	// drain whatever was buffered before the kick, then observe the closure
	kicked := false
	drainDeadline := time.After(5 * time.Second)
	for !kicked {
		select {
		case _, ok := <-slow:
			if !ok {
				kicked = true
			}
		case <-drainDeadline:
			t.Fatal("slow subscriber was never kicked")
		}
	}

	// the replay ring stayed within its byte budget despite the flood
	hub.mu.Lock()
	ringBytes := hub.sessions["run-1"].replayBytes
	hub.mu.Unlock()
	if ringBytes > ttyReplayCap {
		t.Errorf("replay ring = %d bytes, cap is %d", ringBytes, ttyReplayCap)
	}

	// a fresh subscriber re-attaches cleanly and receives live bytes
	replay, fresh, freshUnsub, err := hub.Subscribe("run-1")
	if err != nil {
		t.Fatalf("subscribe fresh: %v", err)
	}
	defer freshUnsub()
	if len(replay) == 0 {
		t.Error("fresh subscriber: replay ring must hold the flood")
	}
	if _, err := fakeProc.Write([]byte("after kick")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// residual flood chunks may still be in flight ahead of the marker
	got := false
	liveDeadline := time.After(5 * time.Second)
	for !got {
		select {
		case b := <-fresh:
			if string(b) == "after kick" {
				got = true
			}
		case <-liveDeadline:
			t.Fatal("fresh subscriber never received live bytes")
		}
	}
}

// TestTTYHubResizeClearsRingOnWidthChange pins the duplicated-text fix: raw
// TUI bytes embed cursor addressing and erase ranges counted at their
// EMISSION width, so replaying frames recorded at another width re-wraps
// lines, breaks the accounting and leaves stale frame copies on screen. A
// cols change must therefore drop every stored byte; rows-only resizes keep
// history. Same-size resizes must be no-ops for the ring.
func TestTTYHubResizeClearsRingOnWidthChange(t *testing.T) {
	hub := newTTYHub(slog.New(slog.NewTextHandler(io.Discard, nil)))

	master, fakeProc := ioPipe()
	defer func() { _ = fakeProc.Close() }()
	resizes := make(chan [2]uint16, 8)
	hub.Register("run-1", master, func(cols, rows uint16) error {
		resizes <- [2]uint16{cols, rows}
		return nil
	}, 80)

	if _, err := fakeProc.Write([]byte("wide-frame bytes")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// guarantee readLoop drained the wide frame BEFORE any resize, so the
	// clear below can't race an undrained pipe (chunks would coalesce)
	replayP, liveP, unsubP, err := hub.Subscribe("run-1")
	if err != nil {
		t.Fatalf("subscribe probe: %v", err)
	}
	readLive(t, liveP, "wide-frame bytes")
	unsubP()
	_ = replayP // snapshot may predate the drain; only the sync matters

	// rows-only resize: history survives
	if err := hub.Resize("run-1", 80, 30); err != nil {
		t.Fatalf("resize rows-only: %v", err)
	}
	select {
	case got := <-resizes:
		if got != [2]uint16{80, 30} {
			t.Fatalf("resize forwarded %v, want [80 30]", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rows-only resize never reached the PTY")
	}

	// width change: ring invalidated
	if err := hub.Resize("run-1", 52, 30); err != nil {
		t.Fatalf("resize cols: %v", err)
	}

	// attach AFTER the width change: whatever the ring holds by now, the
	// pre-change frames must be gone
	replayA, liveA, unsubA, err := hub.Subscribe("run-1")
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	defer unsubA()
	if strings.Contains(string(replayA), "wide-frame") {
		t.Fatalf("replay after width change still holds old-width bytes: %q", replayA)
	}

	// write post-change output and let the hub drain it deterministically
	if _, err := fakeProc.Write([]byte("narrow-frame bytes")); err != nil {
		t.Fatalf("write: %v", err)
	}
	readLive(t, liveA, "narrow-frame bytes")

	replayB, _, unsubB, err := hub.Subscribe("run-1")
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	unsubB()
	if string(replayB) != "narrow-frame bytes" {
		t.Fatalf("replay = %q, want only post-change bytes", replayB)
	}

	// same-size resize: no-op for the ring
	_, liveC, unsubC, err := hub.Subscribe("run-1")
	if err != nil {
		t.Fatalf("subscribe C: %v", err)
	}
	defer unsubC()
	if _, err := fakeProc.Write([]byte(" more")); err != nil {
		t.Fatalf("write: %v", err)
	}
	readLive(t, liveC, " more")
	if err := hub.Resize("run-1", 52, 30); err != nil {
		t.Fatalf("resize same-size: %v", err)
	}
	replayD, _, unsubD, err := hub.Subscribe("run-1")
	if err != nil {
		t.Fatalf("subscribe D: %v", err)
	}
	unsubD()
	if string(replayD) != "narrow-frame bytes more" {
		t.Fatalf("same-size resize must preserve history, got %q", replayD)
	}
}

// readLive drains a subscriber channel until the expected chunk arrives,
// tolerating interleaved PTY chunks ahead of it.
func readLive(t *testing.T, ch <-chan []byte, want string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case b := <-ch:
			if string(b) == want {
				return
			}
		case <-deadline:
			t.Fatalf("live channel never delivered %q", want)
		}
	}
}
