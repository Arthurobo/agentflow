package agentapi

import (
	"bytes"
	"testing"
)

// A full-screen TUI enables the alternate screen once at startup. The replay
// ring drops that enable when it is evicted (2MB cap) or cleared (width
// change), so a fresh attach must still be told to enter the alternate screen —
// otherwise the client renders the repaints in its normal buffer and cannot
// scroll.
func TestReplaySnapshotRestoresAlternateScreenAndMouse(t *testing.T) {
	s := &ttySess{}
	start := []byte("\x1b[?1049h\x1b[?1000h\x1b[?1006hhello")
	s.learnModes(start)
	s.remember(start)

	// Enough output (in realistic ~32KB chunks) to evict the startup chunk.
	for i := 0; i < (ttyReplayCap/(32*1024))+4; i++ {
		c := bytes.Repeat([]byte("x"), 32*1024)
		s.learnModes(c)
		s.remember(c)
	}

	snap := s.replaySnapshot()
	if !bytes.HasPrefix(snap, []byte("\x1b[?1049h")) {
		t.Fatalf("snapshot must re-enter the alternate screen; prefix=%q", snap[:min(24, len(snap))])
	}
	for _, want := range [][]byte{[]byte("\x1b[?1000h"), []byte("\x1b[?1006h")} {
		if !bytes.Contains(snap, want) {
			t.Fatalf("snapshot must restore mouse mode %q", want)
		}
	}
}

// A width change clears the ring; the learned modes must survive it so the
// next attach still restores the alternate screen.
func TestReplayModesSurviveRingClear(t *testing.T) {
	s := &ttySess{}
	s.learnModes([]byte("\x1b[?1049hframe"))
	s.remember([]byte("\x1b[?1049hframe"))
	// simulate Resize's ring invalidation (width change)
	s.replay = nil
	s.replayBytes = 0
	if !bytes.HasPrefix(s.replaySnapshot(), []byte("\x1b[?1049h")) {
		t.Fatal("alt-screen mode must survive a ring clear")
	}
}

// Leaving the alternate screen must stop it being restored. Learn the toggles
// without keeping them in the ring, so only the restore prefix can supply the
// sequence — an empty ring then yields an empty snapshot.
func TestReplayStopsRestoringAfterAltExit(t *testing.T) {
	s := &ttySess{}
	s.learnModes([]byte("\x1b[?1049hin-alt"))
	s.learnModes([]byte("\x1b[?1049lback-to-normal"))
	if snap := s.replaySnapshot(); len(snap) != 0 {
		t.Fatalf("after leaving the alternate screen the prefix must be empty; got %q", snap)
	}
}
