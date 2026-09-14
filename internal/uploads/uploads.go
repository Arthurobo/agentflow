// Package uploads is the server side of image attachments: where a file may
// be written, what it is allowed to be, and how much of it a session may
// keep.
//
// Everything here starts from one assumption: the client is not trusted. It
// does not choose the path, it does not get to say what the bytes are, and
// it cannot name a file it did not upload. The client supplies bytes and a
// desired name; the server decides everything else and hands back an id.
package uploads

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Limits are the policy knobs. Every one is settable from the environment
// with a safe default, because the right number depends on the machine and
// the wrong number should never require a rebuild.
type Limits struct {
	// MaxBytes is the largest single file.
	MaxBytes int64
	// SessionBytes / SessionFiles bound what one session can accumulate.
	SessionBytes int64
	SessionFiles int64
	// TotalBytes is the global disk cap across every upload. 0 disables
	// the cap.
	TotalBytes int64
	// RatePerMinute bounds uploads per run per minute.
	RatePerMinute int
	// RetentionDays is how long a file survives the sweep.
	RetentionDays int
}

// DefaultLimits are the documented defaults (10MB per file, 200MB and 100
// files per session, 2GB total disk, 20 uploads a minute, 7 days).
func DefaultLimits() Limits {
	return Limits{
		MaxBytes:      10 << 20,
		SessionBytes:  200 << 20,
		SessionFiles:  100,
		TotalBytes:    2 << 30,
		RatePerMinute: 20,
		RetentionDays: 7,
	}
}

// LimitsFromEnv applies AF_UPLOADS_* overrides over the defaults. A value
// that does not parse, or is not positive, leaves the default in place: a
// typo in a unit file must not silently remove a limit.
func LimitsFromEnv() Limits {
	l := DefaultLimits()
	intEnv := func(key string, cur int64) int64 {
		v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64)
		if err != nil || v <= 0 {
			return cur
		}
		return v
	}
	l.MaxBytes = intEnv("AF_UPLOADS_MAX_BYTES", l.MaxBytes)
	l.SessionBytes = intEnv("AF_UPLOADS_SESSION_BYTES", l.SessionBytes)
	l.SessionFiles = intEnv("AF_UPLOADS_SESSION_FILES", l.SessionFiles)
	l.TotalBytes = intEnv("AF_UPLOADS_TOTAL_BYTES", l.TotalBytes)
	l.RatePerMinute = int(intEnv("AF_UPLOADS_RATE_PER_MINUTE", int64(l.RatePerMinute)))
	l.RetentionDays = int(intEnv("AF_UPLOADS_RETENTION_DAYS", int64(l.RetentionDays)))
	return l
}

// Root is the storage root: AF_UPLOADS_DIR, else
// ~/.local/share/agentflow/uploads, next to the rest of the daemon's data.
func Root() string {
	if dir := strings.TrimSpace(os.Getenv("AF_UPLOADS_DIR")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agentflow-uploads")
	}
	return filepath.Join(home, ".local", "share", "agentflow", "uploads")
}

// migrateLegacyUploadsDir moves a single leftover ~/.agentflow/uploads
// directory (if any) to the new ~/.local/share/agentflow/uploads.
// Best effort: a failure is logged and ignored — the new directory
// is always created empty.
func migrateLegacyUploadsDir(logf func(string, ...any)) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	old := filepath.Join(home, ".agentflow", "uploads")
	if _, err := os.Stat(old); err != nil {
		return
	}
	newDir := Root()
	if old == newDir {
		return
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o700); err != nil {
		logf("uploads: mkdir %s: %v", filepath.Dir(newDir), err)
		return
	}
	if err := os.Rename(old, newDir); err == nil {
		logf("uploads: migrated %s -> %s", old, newDir)
		return
	}
	// The new directory already exists (a rename cannot replace a non-empty
	// directory): move entries across one by one, never overwriting, and
	// remove the old directory only once it is empty.
	entries, err := os.ReadDir(old)
	if err != nil {
		logf("uploads: read %s: %v", old, err)
		return
	}
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		logf("uploads: mkdir %s: %v", newDir, err)
		return
	}
	moved, kept := 0, 0
	for _, e := range entries {
		src, dst := filepath.Join(old, e.Name()), filepath.Join(newDir, e.Name())
		if _, err := os.Lstat(dst); err == nil {
			kept++
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			logf("uploads: migrate %s: %v", src, err)
			kept++
			continue
		}
		moved++
	}
	if kept == 0 {
		_ = os.Remove(old)
	}
	logf("uploads: merged %s into %s (%d moved, %d left in place)", old, newDir, moved, kept)
}

// allowedMime is the image allowlist. It is checked against SNIFFED bytes,
// never against what the client called the file.
var allowedMime = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// Errors the upload handler maps onto HTTP status codes.
var (
	ErrType      = errors.New("uploads: file type not allowed")
	ErrSize      = errors.New("uploads: file too large")
	ErrQuota     = errors.New("uploads: session quota exhausted")
	ErrRate      = errors.New("uploads: too many uploads")
	ErrHash      = errors.New("uploads: content hash mismatch")
	ErrShort     = errors.New("uploads: fewer bytes than announced")
	ErrOverrun   = errors.New("uploads: more bytes than announced")
	ErrInternal  = errors.New("uploads: could not write the file")
	ErrNoSession = errors.New("uploads: a session id is required")
)

// SniffMime identifies content from its leading bytes and returns "" for
// anything not on the allowlist.
//
// http.DetectContentType is the whole check. The client's declared mime is
// advisory at best and an attack surface at worst: a .png that is really an
// ELF is exactly what an allowlist keyed on the name would wave through.
func SniffMime(head []byte) string {
	if len(head) == 0 {
		return ""
	}
	// DetectContentType reads at most 512 bytes and never fails.
	got := http.DetectContentType(head)
	if i := strings.IndexByte(got, ';'); i >= 0 {
		got = strings.TrimSpace(got[:i])
	}
	if !allowedMime[got] {
		return ""
	}
	return got
}

// SanitizeName reduces a client-supplied filename to something that can only
// ever be a leaf inside our own directory.
//
// Order matters. Take the base name FIRST so "../../etc/passwd" loses its
// path before anything else looks at it, then restrict the characters, then
// deal with the leftovers that are still legal but hostile: a name that is
// all dots, a name that is empty, a name long enough to hit the filesystem's
// own limit.
func SanitizeName(name string) string {
	// Strip any directory component the client tried to include, in both
	// separator conventions — a Windows-style "..\\x" must not survive on a
	// Unix host just because filepath.Base does not treat \ as a separator.
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.TrimSpace(name)

	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteRune('-')
		default:
			// Everything else — including every non-ASCII rune — becomes a
			// hyphen rather than being dropped, so two different names
			// cannot collapse into the same one as easily.
			b.WriteRune('-')
		}
	}
	clean := strings.Trim(b.String(), "-")
	// "." and ".." survive the character filter and are the two names that
	// must never reach a path join.
	if strings.Trim(clean, ".") == "" {
		clean = "image"
	}
	// Leave room for the timestamp prefix inside the usual 255-byte limit.
	const maxName = 96
	if len(clean) > maxName {
		ext := filepath.Ext(clean)
		if len(ext) > 8 {
			ext = ""
		}
		clean = clean[:maxName-len(ext)] + ext
	}
	return clean
}

// slug reduces a project name to a safe directory component.
func slug(s string) string {
	out := SanitizeName(s)
	if out == "image" || out == "" {
		return "project"
	}
	return out
}

// Store is the policy + filesystem gate. One per agentd.
type Store struct {
	root   string
	limits Limits

	mu sync.Mutex
	// recent tracks upload times per run for the rate limit. Entries idle
	// for rateIdle are evicted (lazily in allowRate and in Sweep), so the
	// map is bounded by recently active runs, not by every run that ever
	// uploaded.
	recent map[string][]time.Time
	// lastEvict is when allowRate last scanned recent for idle entries.
	lastEvict time.Time
	// totalBytes is a running count of bytes currently on disk under
	// root, for the global disk cap. Rebuilt from disk by RecomputeTotal
	// and Sweep; adjusted by Reserve/Release in between.
	totalBytes int64
	// now is the clock (tests inject one).
	now func() time.Time
}

// rateIdle is how long a run's rate-limit entry survives without an upload.
const rateIdle = time.Hour

// New builds a Store rooted at root (Root() when empty).
func New(root string, limits Limits) *Store {
	if root == "" {
		root = Root()
	}
	if limits.MaxBytes <= 0 {
		limits = DefaultLimits()
	}
	return &Store{root: root, limits: limits, recent: map[string][]time.Time{}, now: time.Now}
}

func (s *Store) Root() string   { return s.root }
func (s *Store) Limits() Limits { return s.limits }

// Allow applies the pre-transfer checks: announced size, per-session quota,
// global disk cap, and per-run rate. It is deliberately called BEFORE a
// single byte is accepted — refusing after a 10MB transfer is a worse
// answer to the same question.
//
// usedFiles/usedBytes come from the caller's registry so this package does
// not need a database.
func (s *Store) Allow(runID string, announced, usedBytes, usedFiles int64) error {
	if announced <= 0 || announced > s.limits.MaxBytes {
		return ErrSize
	}
	if usedFiles+1 > s.limits.SessionFiles || usedBytes+announced > s.limits.SessionBytes {
		return ErrQuota
	}
	if s.limits.TotalBytes > 0 {
		s.mu.Lock()
		tb := s.totalBytes
		s.mu.Unlock()
		if tb+announced > s.limits.TotalBytes {
			return ErrQuota
		}
	}
	return s.allowRate(runID)
}

// reserveBytes bumps totalBytes after a successful upload. The caller
// invokes this once the file is fully on disk and hashed.
func (s *Store) reserveBytes(n int64) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.totalBytes += n
	s.mu.Unlock()
}

// releaseBytes decrements totalBytes when a file is deleted (sweep or
// explicit remove).
func (s *Store) releaseBytes(n int64) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.totalBytes -= n
	if s.totalBytes < 0 {
		s.totalBytes = 0
	}
	s.mu.Unlock()
}

// Reserve adds n bytes to the running disk total once an upload is on disk.
func (s *Store) Reserve(n int64) { s.reserveBytes(n) }

// Release subtracts n bytes from the running disk total after a delete.
func (s *Store) Release(n int64) { s.releaseBytes(n) }

// RecomputeTotal walks the root and rebuilds totalBytes from disk. Call it
// when the Store is built, so the global cap counts what earlier daemon
// lives left behind, and after anything deletes files outside the Store.
func (s *Store) RecomputeTotal() int64 {
	fresh := s.diskBytes()
	s.mu.Lock()
	s.totalBytes = fresh
	s.mu.Unlock()
	return fresh
}

// TotalBytes reports the running disk total the global cap is checked
// against.
func (s *Store) TotalBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalBytes
}

func (s *Store) diskBytes() int64 {
	var fresh int64
	_ = filepath.WalkDir(s.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				fresh += info.Size()
			}
		}
		return nil
	})
	return fresh
}

// evictIdleLocked drops rate-limit entries whose newest upload is older than
// rateIdle. Caller holds s.mu.
func (s *Store) evictIdleLocked(now time.Time) {
	cutoff := now.Add(-rateIdle)
	for runID, ts := range s.recent {
		if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
			delete(s.recent, runID)
		}
	}
	s.lastEvict = now
}

func (s *Store) allowRate(runID string) error {
	now := s.now()
	cutoff := now.Add(-time.Minute)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Without the sweeper nothing else would ever forget a run, so the map
	// is pruned here too, at most once a minute to keep the hot path cheap.
	if now.Sub(s.lastEvict) >= time.Minute {
		s.evictIdleLocked(now)
	}
	kept := s.recent[runID][:0]
	for _, t := range s.recent[runID] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= s.limits.RatePerMinute {
		s.recent[runID] = kept
		return ErrRate
	}
	s.recent[runID] = append(kept, now)
	return nil
}

// Dest is where one upload will be written. The client sees the id; the path
// is the server's business.
type Dest struct {
	// Final is the path the completed file takes.
	Final string
	// Part is where bytes land until the hash checks out.
	Part string
	// ID is the attachment id.
	ID string
}

// NewDest chooses the destination for one upload: a server-built path under
// <root>/<project>/<session>/, named with a UTC timestamp and the sanitized
// client name.
//
// An empty sessionID is rejected. Without a session the per-session quota
// cannot apply, and uploads would bypass it.
func (s *Store) NewDest(project, sessionID, clientName string) (Dest, error) {
	if sessionID == "" {
		return Dest{}, fmt.Errorf("uploads: a session id is required")
	}
	id, err := newID()
	if err != nil {
		return Dest{}, err
	}
	dir := filepath.Join(s.root, slug(project), slug(sessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Dest{}, fmt.Errorf("uploads: mkdir: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	final := filepath.Join(dir, stamp+"-"+SanitizeName(clientName))

	// A repeat name in the same second is rare but not impossible; never
	// clobber a file that is already there.
	if _, statErr := os.Stat(final); statErr == nil {
		final = filepath.Join(dir, stamp+"-"+id[:6]+"-"+SanitizeName(clientName))
	}

	// Belt and braces against a sanitizer bug: whatever we just built must
	// still be inside the root.
	if !withinRoot(s.root, final) {
		return Dest{}, ErrInternal
	}
	return Dest{Final: final, Part: final + ".part", ID: id}, nil
}

// withinRoot reports whether path is inside root after symlink-free
// lexical cleaning.
func withinRoot(root, path string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Sweep deletes expired and orphaned attachments. It returns how many files
// it removed and how many bytes it reclaimed.
//
// The FILE goes first and the row second: losing the row while the file
// survives leaks disk that nothing can ever find again. A file that is
// already gone is not an error — the row is what we are really cleaning up.
//
// Also rebuilds totalBytes from disk and evicts per-run rate-limit entries
// idle for over an hour.
func (s *Store) Sweep(ctx context.Context, reg Registry, now time.Time) (removed int, bytes int64, err error) {
	cutoff := now.AddDate(0, 0, -s.limits.RetentionDays).UnixMilli()
	old, err := reg.AttachmentsOlderThan(ctx, cutoff, 1000)
	if err != nil {
		return 0, 0, err
	}
	orphaned, err := reg.AttachmentsWithoutASession(ctx, 1000)
	if err != nil {
		return 0, 0, err
	}

	seen := map[string]bool{}
	for _, a := range append(old, orphaned...) {
		if a == nil || seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		if rmErr := os.Remove(a.Path); rmErr != nil && !os.IsNotExist(rmErr) {
			// Leave the row alone so the next sweep tries again rather than
			// forgetting a file it could not delete.
			continue
		}
		if delErr := reg.DeleteAttachment(ctx, a.ID); delErr != nil {
			continue
		}
		removed++
		bytes += a.Bytes
		s.releaseBytes(a.Bytes)
	}

	// Rebuild totalBytes from disk so a manual rm outside the daemon
	// cannot permanently over-count.
	fresh := s.diskBytes()
	s.mu.Lock()
	s.totalBytes = fresh
	s.evictIdleLocked(now)
	s.mu.Unlock()
	return removed, bytes, nil
}
