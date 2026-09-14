package uploads

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

// A client-supplied name is the one thing the client contributes to a path,
// so it is the one thing that has to survive contact with a hostile value.
func TestSanitizeNameCannotEscapeTheDirectory(t *testing.T) {
	cases := map[string]string{
		"shot.png":                 "shot.png",
		"../../etc/passwd":         "passwd",
		"..\\..\\windows\\sys.ini": "sys.ini",
		"/absolute/path/thing.png": "thing.png",
		"..":                       "image",
		".":                        "image",
		"...":                      "image",
		"":                         "image",
		"   ":                      "image",
		"my photo.png":             "my-photo.png",
		"scr€€nshot.png":           "scr--nshot.png",
		"emoji-📸.png":              "emoji--.png",
		"nul\x00byte.png":          "nul-byte.png",
		"-leading-and-trailing-":   "leading-and-trailing",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}

	// Whatever comes out must be a leaf: no separators, no dot-segments.
	for in := range cases {
		got := SanitizeName(in)
		if strings.ContainsAny(got, `/\`) {
			t.Fatalf("SanitizeName(%q) = %q still contains a separator", in, got)
		}
		if got == "." || got == ".." {
			t.Fatalf("SanitizeName(%q) = %q is a dot segment", in, got)
		}
	}

	// A very long name is truncated but keeps a short extension.
	long := strings.Repeat("a", 400) + ".png"
	got := SanitizeName(long)
	if len(got) > 96 {
		t.Fatalf("long name not capped: %d chars", len(got))
	}
	if !strings.HasSuffix(got, ".png") {
		t.Fatalf("long name lost its extension: %q", got)
	}
}

// The destination is the server's decision end to end; a client name only
// ever contributes a leaf inside our own root.
func TestNewDestStaysInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	s := New(root, DefaultLimits())

	for _, name := range []string{"shot.png", "../../escape.png", "/etc/passwd", ".."} {
		dest, err := s.NewDest("webapp", "sess-1", name)
		if err != nil {
			t.Fatalf("NewDest(%q): %v", name, err)
		}
		if !withinRoot(root, dest.Final) {
			t.Fatalf("NewDest(%q) escaped the root: %s", name, dest.Final)
		}
		if !strings.HasSuffix(dest.Part, ".part") {
			t.Fatalf("partial path should be a .part: %s", dest.Part)
		}
		if dest.ID == "" {
			t.Fatal("every destination needs an id")
		}
	}

	// A hostile project or session name cannot climb out either.
	dest, err := s.NewDest("../../..", "../../..", "x.png")
	if err != nil {
		t.Fatalf("NewDest: %v", err)
	}
	if !withinRoot(root, dest.Final) {
		t.Fatalf("hostile project/session escaped: %s", dest.Final)
	}

	// Directories are 0700: these are somebody's screenshots.
	info, err := os.Stat(filepath.Dir(dest.Final))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("upload directory mode = %o, want 700", perm)
	}
}

// The type is decided by the bytes. A .png that is really an executable is
// exactly what a name-based allowlist waves through.
func TestSniffMimeTrustsBytesNotNames(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
	gif := []byte("GIF89a" + strings.Repeat("\x00", 16))
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0}
	elf := []byte{0x7F, 'E', 'L', 'F', 1, 1, 1, 0, 0, 0, 0, 0}
	script := []byte("#!/bin/sh\nrm -rf /\n")

	if got := SniffMime(png); got != "image/png" {
		t.Fatalf("png sniffed as %q", got)
	}
	if got := SniffMime(gif); got != "image/gif" {
		t.Fatalf("gif sniffed as %q", got)
	}
	if got := SniffMime(jpeg); got != "image/jpeg" {
		t.Fatalf("jpeg sniffed as %q", got)
	}
	if got := SniffMime(elf); got != "" {
		t.Fatalf("an ELF must never be allowed, sniffed as %q", got)
	}
	if got := SniffMime(script); got != "" {
		t.Fatalf("a shell script must never be allowed, sniffed as %q", got)
	}
	if got := SniffMime(nil); got != "" {
		t.Fatalf("empty content sniffed as %q", got)
	}
}

func TestLimitsFromEnvIgnoresNonsense(t *testing.T) {
	t.Setenv("AF_UPLOADS_MAX_BYTES", "12345")
	t.Setenv("AF_UPLOADS_RETENTION_DAYS", "3")
	// A typo must leave the default in place rather than removing a limit.
	t.Setenv("AF_UPLOADS_SESSION_FILES", "not-a-number")
	t.Setenv("AF_UPLOADS_RATE_PER_MINUTE", "-5")

	l := LimitsFromEnv()
	if l.MaxBytes != 12345 {
		t.Fatalf("MaxBytes = %d", l.MaxBytes)
	}
	if l.RetentionDays != 3 {
		t.Fatalf("RetentionDays = %d", l.RetentionDays)
	}
	if l.SessionFiles != DefaultLimits().SessionFiles {
		t.Fatalf("a bad value must keep the default, got %d", l.SessionFiles)
	}
	if l.RatePerMinute != DefaultLimits().RatePerMinute {
		t.Fatalf("a negative value must keep the default, got %d", l.RatePerMinute)
	}
}

func TestRootPrefersTheEnvOverride(t *testing.T) {
	t.Setenv("AF_UPLOADS_DIR", "/tmp/af-uploads-test")
	if got := Root(); got != "/tmp/af-uploads-test" {
		t.Fatalf("Root() = %q", got)
	}
	t.Setenv("AF_UPLOADS_DIR", "")
	if got := Root(); !strings.HasSuffix(got, filepath.Join(".local", "share", "agentflow", "uploads")) {
		t.Fatalf("default root = %q, want ~/.local/share/agentflow/uploads", got)
	}
}

// Quotas and the rate limit are checked BEFORE a byte is accepted: refusing
// after a 10MB transfer answers the same question far more expensively.
func TestAllowRefusesBeforeAnyBytesAreTaken(t *testing.T) {
	s := New(t.TempDir(), Limits{
		MaxBytes: 1000, SessionBytes: 5000, SessionFiles: 3, RatePerMinute: 2, RetentionDays: 7,
	})

	if err := s.Allow("run-1", 1001, 0, 0); err != ErrSize {
		t.Fatalf("oversize: %v", err)
	}
	if err := s.Allow("run-1", 0, 0, 0); err != ErrSize {
		t.Fatalf("zero size: %v", err)
	}
	if err := s.Allow("run-1", 100, 0, 3); err != ErrQuota {
		t.Fatalf("file count quota: %v", err)
	}
	if err := s.Allow("run-1", 100, 4950, 0); err != ErrQuota {
		t.Fatalf("byte quota: %v", err)
	}

	// Rate: two allowed, the third refused, and a different run is unaffected.
	if err := s.Allow("run-rate", 10, 0, 0); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.Allow("run-rate", 10, 0, 0); err != nil {
		t.Fatalf("second: %v", err)
	}
	if err := s.Allow("run-rate", 10, 0, 0); err != ErrRate {
		t.Fatalf("third should be rate limited, got %v", err)
	}
	if err := s.Allow("run-other", 10, 0, 0); err != nil {
		t.Fatalf("a different run must have its own budget: %v", err)
	}
}

// --- retention -----------------------------------------------------------

// fakeRegistry is an in-memory Registry so the sweep can be tested without a
// database.
type fakeRegistry struct {
	mu   sync.Mutex
	rows map[string]*store.Attachment
	// orphans are ids whose run row is gone.
	orphans map[string]bool
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{rows: map[string]*store.Attachment{}, orphans: map[string]bool{}}
}

func (f *fakeRegistry) InsertAttachment(_ context.Context, a *store.Attachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[a.ID] = a
	return nil
}

func (f *fakeRegistry) SessionAttachmentUsage(_ context.Context, sessionID string) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var files, bytes int64
	for _, a := range f.rows {
		if a.SessionID == sessionID {
			files++
			bytes += a.Bytes
		}
	}
	return files, bytes, nil
}

func (f *fakeRegistry) AttachmentsOlderThan(_ context.Context, cutoff int64, _ int) ([]*store.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*store.Attachment{}
	for _, a := range f.rows {
		if a.CreatedAt < cutoff {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeRegistry) AttachmentsWithoutASession(_ context.Context, _ int) ([]*store.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*store.Attachment{}
	for id, a := range f.rows {
		if f.orphans[id] {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeRegistry) DeleteAttachment(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, id)
	return nil
}

func (f *fakeRegistry) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func TestSweepRemovesExpiredAndOrphanedAttachments(t *testing.T) {
	root := t.TempDir()
	s := New(root, Limits{MaxBytes: 1 << 20, SessionBytes: 1 << 30, SessionFiles: 100, RatePerMinute: 60, RetentionDays: 7})
	reg := newFakeRegistry()
	now := time.Now()

	write := func(id string, age time.Duration, orphan bool) string {
		t.Helper()
		path := filepath.Join(root, id+".png")
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = reg.InsertAttachment(context.Background(), &store.Attachment{
			ID: id, RunID: "run-1", SessionID: "sess-1", Path: path,
			Bytes: 4, CreatedAt: now.Add(-age).UnixMilli(),
		})
		if orphan {
			reg.orphans[id] = true
		}
		return path
	}

	fresh := write("fresh", 1*time.Hour, false)
	old := write("old", 8*24*time.Hour, false)
	orphan := write("orphan", 1*time.Hour, true)

	removed, bytes, err := s.Sweep(context.Background(), reg, now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 2 || bytes != 8 {
		t.Fatalf("removed %d files / %d bytes, want 2 / 8", removed, bytes)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a fresh attachment must survive the sweep")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("an expired attachment's FILE must be deleted")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("an orphaned attachment's file must be deleted")
	}
	if reg.count() != 1 {
		t.Fatalf("rows left = %d, want just the fresh one", reg.count())
	}
}

// A file that is already gone is not a failure — the row is what the sweep
// is really cleaning up, and a half-deleted pair must converge.
func TestSweepToleratesAMissingFile(t *testing.T) {
	root := t.TempDir()
	s := New(root, Limits{MaxBytes: 1 << 20, SessionBytes: 1 << 30, SessionFiles: 100, RatePerMinute: 60, RetentionDays: 7})
	reg := newFakeRegistry()
	now := time.Now()

	_ = reg.InsertAttachment(context.Background(), &store.Attachment{
		ID: "ghost", RunID: "r", SessionID: "s",
		Path:  filepath.Join(root, "never-existed.png"),
		Bytes: 10, CreatedAt: now.Add(-30 * 24 * time.Hour).UnixMilli(),
	})
	removed, _, err := s.Sweep(context.Background(), reg, now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 || reg.count() != 0 {
		t.Fatalf("a row whose file is gone must still be cleaned up (removed=%d rows=%d)", removed, reg.count())
	}
}
