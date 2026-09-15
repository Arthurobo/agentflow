package uploads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

// pngBytes returns n bytes that http.DetectContentType reads as image/png:
// the PNG signature followed by padding.
func pngBytes(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("\x89PNG\r\n\x1a\n"))
	return b
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// filesUnder lists the completed (non-.part) files under a store root.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(p) == ".part" {
			return nil
		}
		out = append(out, p)
		return nil
	})
	return out
}

// A brand-new session has no engine session id for the first few seconds, but
// attaching a screenshot right after starting one is the most common upload
// flow. Such an upload must succeed, accounted against the run id so the
// per-session quota is still charged (not bypassed).
func TestAnUploadForARunWithoutASessionUsesTheRunID(t *testing.T) {
	root := t.TempDir()
	st := New(root, Limits{MaxBytes: 1 << 20, SessionBytes: 10 << 20, SessionFiles: 10, RatePerMinute: 60, RetentionDays: 7})
	reg := newFakeRegistry()

	payload := pngBytes(1024)
	att, err := st.Ingest(context.Background(), reg,
		RunInfo{RunID: "run-nosession", Project: "webapp"},
		"shot.png", sha256Hex(payload), int64(len(payload)), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Ingest with a run id but no session id = %v, want success", err)
	}
	if att.RunID != "run-nosession" {
		t.Fatalf("attachment RunID = %q, want run-nosession", att.RunID)
	}
	if att.SessionID != "run-nosession" {
		t.Fatalf("attachment SessionID = %q, want the run id fallback", att.SessionID)
	}
	if reg.count() != 1 {
		t.Fatalf("one row must be registered, have %d", reg.count())
	}
}

// A run with neither a session id nor a run id has nowhere to account the
// bytes — usage keyed on "" reads as zero and would bypass the quota — so it
// is refused before anything is written or the rate budget is spent.
func TestAnUploadWithNoRunOrSessionIsRefused(t *testing.T) {
	root := t.TempDir()
	st := New(root, Limits{MaxBytes: 1 << 20, SessionBytes: 10 << 20, SessionFiles: 10, RatePerMinute: 60, RetentionDays: 7})
	reg := newFakeRegistry()

	payload := pngBytes(1024)
	_, err := st.Ingest(context.Background(), reg,
		RunInfo{Project: "webapp"},
		"shot.png", sha256Hex(payload), int64(len(payload)), bytes.NewReader(payload))
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("Ingest with no ids = %v, want ErrNoSession", err)
	}
	if files := filesUnder(t, root); len(files) != 0 {
		t.Fatalf("nothing may be written, found %v", files)
	}
	if reg.count() != 0 {
		t.Fatalf("no row may be registered, have %d", reg.count())
	}
}

// The global disk cap is only as good as the running total. It has to count
// what earlier daemon lives left on disk, grow with every stored upload, and
// shrink again when a file is deleted.
func TestTotalBytesTrackDiskAcrossUploadsAndDeletes(t *testing.T) {
	root := t.TempDir()
	leftover := filepath.Join(root, "old-project", "old-session", "left.png")
	if err := os.MkdirAll(filepath.Dir(leftover), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leftover, make([]byte, 700), 0o600); err != nil {
		t.Fatal(err)
	}
	st := New(root, Limits{MaxBytes: 1 << 20, SessionBytes: 10 << 20, SessionFiles: 10, TotalBytes: 3000, RatePerMinute: 60, RetentionDays: 7})
	if got := st.RecomputeTotal(); got != 700 {
		t.Fatalf("RecomputeTotal = %d, want the 700 bytes already on disk", got)
	}

	reg := newFakeRegistry()
	run := RunInfo{RunID: "run-1", SessionID: "sess-1", Project: "webapp"}
	upload := func(name string, n int) error {
		payload := pngBytes(n)
		_, err := st.Ingest(context.Background(), reg, run, name, sha256Hex(payload), int64(n), bytes.NewReader(payload))
		return err
	}

	if err := upload("u1.png", 2000); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if got := st.TotalBytes(); got != 2700 {
		t.Fatalf("TotalBytes after an upload = %d, want 2700", got)
	}
	// 2700 + 400 crosses the 3000 cap: refused before any byte moves.
	if err := upload("u2.png", 400); !errors.Is(err, ErrQuota) {
		t.Fatalf("an upload over the global cap = %v, want ErrQuota", err)
	}

	// Expire the stored attachment; the sweep deletes it and the total
	// must come back down, so the cap is not permanently used up.
	reg.mu.Lock()
	for _, a := range reg.rows {
		a.CreatedAt = time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	}
	reg.mu.Unlock()
	if removed, _, err := st.Sweep(context.Background(), reg, time.Now()); err != nil || removed != 1 {
		t.Fatalf("Sweep removed %d (%v), want 1", removed, err)
	}
	if got := st.TotalBytes(); got != 700 {
		t.Fatalf("TotalBytes after the delete = %d, want 700", got)
	}
	if err := upload("u3.png", 400); err != nil {
		t.Fatalf("with space freed the upload must succeed: %v", err)
	}
}

// failingRegistry refuses every insert, so a stored file has to be undone.
type failingRegistry struct{ *fakeRegistry }

func (failingRegistry) InsertAttachment(context.Context, *store.Attachment) error {
	return errors.New("disk full")
}

// A file removed because its row could not be written must give its bytes
// back; otherwise every failed insert leaks a piece of the global cap.
func TestAFailedRegistryInsertReleasesItsBytes(t *testing.T) {
	root := t.TempDir()
	st := New(root, Limits{MaxBytes: 1 << 20, SessionBytes: 10 << 20, SessionFiles: 10, TotalBytes: 1 << 20, RatePerMinute: 60, RetentionDays: 7})
	run := RunInfo{RunID: "run-1", SessionID: "sess-1", Project: "webapp"}
	payload := pngBytes(4096)
	_, err := st.Ingest(context.Background(), failingRegistry{newFakeRegistry()}, run,
		"a.png", sha256Hex(payload), int64(len(payload)), bytes.NewReader(payload))
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("Ingest with a failing registry = %v, want ErrInternal", err)
	}
	if got := st.TotalBytes(); got != 0 {
		t.Fatalf("TotalBytes = %d after the file was removed, want 0", got)
	}
	if files := filesUnder(t, root); len(files) != 0 {
		t.Fatalf("the unregistered file must be gone, found %v", files)
	}
}

// The per-run rate map must stay bounded even when the sweeper never runs:
// a run idle for an hour is forgotten on the next upload anywhere.
func TestIdleRateEntriesAreEvictedWithoutTheSweeper(t *testing.T) {
	st := New(t.TempDir(), Limits{MaxBytes: 1000, SessionBytes: 5000, SessionFiles: 3, RatePerMinute: 5, RetentionDays: 7})
	clock := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return clock }

	if err := st.Allow("run-old", 10, 0, 0); err != nil {
		t.Fatalf("run-old: %v", err)
	}
	clock = clock.Add(30 * time.Minute)
	if err := st.Allow("run-mid", 10, 0, 0); err != nil {
		t.Fatalf("run-mid: %v", err)
	}
	st.mu.Lock()
	n := len(st.recent)
	st.mu.Unlock()
	if n != 2 {
		t.Fatalf("both runs are within the hour, want 2 entries, have %d", n)
	}

	clock = clock.Add(31 * time.Minute) // run-old idle 61m, run-mid 31m
	if err := st.Allow("run-new", 10, 0, 0); err != nil {
		t.Fatalf("run-new: %v", err)
	}
	st.mu.Lock()
	_, oldKept := st.recent["run-old"]
	_, midKept := st.recent["run-mid"]
	st.mu.Unlock()
	if oldKept {
		t.Fatal("a run idle for over an hour must be evicted")
	}
	if !midKept {
		t.Fatal("a run active within the hour must be kept")
	}
}
