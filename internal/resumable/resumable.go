// Package resumable provides the opt-in "make this browser-started session
// visible in `claude -r` / /resume" compat shim (see MakeResumable). Shared by
// the `agentd make-resumable` CLI and the auto-on-Terminate hook in the agentd
// control API.
package resumable

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/arthurobo/agentflow/internal/store"
)

// Report describes what MakeResumable did.
type Report struct {
	SessionID       string `json:"sessionId"`
	Transcript      string `json:"transcript"`
	RewrittenEntry  int    `json:"rewrittenEntrypoint"`
	RewrittenPrompt int    `json:"rewrittenPromptSource"`
	Backup          string `json:"backup,omitempty"`
}

// MakeResumable rewrites a browser-started session's launch markers so Claude
// Code's /resume picker includes it.
//
// Why: Claude Code's /resume picker deliberately hides sessions whose launch
// `entrypoint` is sdk-* ("Session <id> filtered from /resume:
// entrypoint=sdk-cli", anthropics/claude-code #84421). agentd launches claude
// with --print/stream-json, so every browser-started session is born sdk-cli
// and never appears in `claude -r` — even after a local resume, because the
// picker reads the launch entrypoint off the transcript records (upstream
// verified this reads from the file itself).
//
// This EDITS the user's own transcript — a deliberate, explicit exception to
// the read-only-transcripts tenet: it backs the file up first, refuses while
// the session is live, and can be reverted via Undo. Nothing else in agentd
// edits a transcript.
func MakeResumable(st *store.Store, id string) (*Report, error) {
	path, sid, err := TranscriptFor(st, id)
	if err != nil {
		return nil, err
	}
	if err := RequireNotLive(sid); err != nil {
		return nil, err
	}
	backup, nE, nP, err := backupAndRewrite(path, time.Now())
	if err != nil {
		return nil, err
	}
	return &Report{SessionID: sid, Transcript: path,
		RewrittenEntry: nE, RewrittenPrompt: nP, Backup: backup}, nil
}

// maxBackups is how many backups are kept per transcript. Every Terminate of
// a browser-started session can make one, and a transcript is megabytes.
const maxBackups = 3

// backupSuffix separates a transcript's path from its backup's timestamp.
const backupSuffix = ".agentflow-bak-"

// backupAndRewrite backs a transcript up, rewrites it, and prunes old backups.
// Every write goes through atomicWrite, so a crash at any point leaves either
// the old file or the new one, never half of either.
func backupAndRewrite(path string, now time.Time) (backup string, rewrittenEntry, rewrittenPrompt int, err error) {
	backup = path + backupSuffix + strconv.FormatInt(now.UnixNano(), 10)
	if err := copyFileAtomic(path, backup); err != nil {
		return "", 0, 0, fmt.Errorf("backup: %w", err)
	}
	nE, nP, err := rewrite(path)
	if err != nil {
		// rewrite replaces the file only after writing all of it, so the
		// transcript is untouched here; the restore is belt and braces.
		if rerr := copyFileAtomic(backup, path); rerr != nil {
			return "", 0, 0, fmt.Errorf("%w (and restoring the backup failed: %v)", err, rerr)
		}
		return "", 0, 0, err
	}
	pruneBackups(path, maxBackups)
	return backup, nE, nP, nil
}

// Undo restores the newest backup for a session.
func Undo(st *store.Store, id string) (string, error) {
	path, _, err := TranscriptFor(st, id)
	if err != nil {
		return "", err
	}
	return restoreNewest(path)
}

// restoreNewest atomically puts the newest backup back in place.
func restoreNewest(path string) (string, error) {
	backups := listBackups(path)
	if len(backups) == 0 {
		return "", fmt.Errorf("no backup found for %s", path)
	}
	newest := backups[len(backups)-1]
	if err := copyFileAtomic(newest, path); err != nil {
		return "", err
	}
	return newest, nil
}

// listBackups returns a transcript's backups, oldest first. Older backups
// carry a timestamp in seconds and newer ones in nanoseconds, so they are
// ordered by the time they name rather than by their spelling.
func listBackups(path string) []string {
	matches, _ := filepath.Glob(path + backupSuffix + "*")
	at := func(name string) int64 {
		n, err := strconv.ParseInt(strings.TrimPrefix(name, path+backupSuffix), 10, 64)
		if err != nil {
			return 0
		}
		if n < 1e12 { // seconds
			n *= int64(time.Second)
		}
		return n
	}
	sort.SliceStable(matches, func(i, j int) bool { return at(matches[i]) < at(matches[j]) })
	return matches
}

// pruneBackups removes all but the newest keep backups of a transcript.
func pruneBackups(path string, keep int) {
	backups := listBackups(path)
	for len(backups) > keep {
		_ = os.Remove(backups[0])
		backups = backups[1:]
	}
}

// TranscriptFor resolves an id (agentd run id or claude session id) to the
// transcript path + claude session id via the managed-sessions table.
func TranscriptFor(st *store.Store, id string) (path, sid string, err error) {
	ctx := context.Background()
	// an id may be either the agentd run key or the claude session id (both are
	// uuid-shaped) — try the run key first, then the claude session id.
	m, err := st.GetManagedSession(ctx, id)
	if err != nil {
		return "", "", err
	}
	if m == nil {
		m, err = st.GetManagedSessionBySessionID(ctx, id)
		if err != nil {
			return "", "", err
		}
	}
	if m == nil {
		return "", "", fmt.Errorf("no managed session for %q (was it spawned by agentd?)", id)
	}
	if m.CWD == "" {
		return "", "", fmt.Errorf("session %s has no cwd recorded", m.SessionID)
	}
	if m.SessionID == "" {
		return "", "", fmt.Errorf("session %s never captured a claude session id", m.ID)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	p := filepath.Join(home, ".claude", "projects", ProjectSlug(m.CWD), m.SessionID+".jsonl")
	return p, m.SessionID, nil
}

// RequireNotLive errors if a claude process is currently attached.
func RequireNotLive(sid string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(home, ".claude", "sessions"))
	if err != nil {
		return nil // no registry dir → nothing live
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(home, ".claude", "sessions", e.Name()))
		if err != nil {
			continue
		}
		var rec struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &rec) == nil && rec.SessionID == sid {
			return fmt.Errorf("session %s is LIVE (a claude process is attached) — terminate it first", sid)
		}
	}
	return nil
}

// rewrite streams the transcript, rewriting only the launch markers:
// entrypoint sdk-* → cli, and promptSource sdk → typed on user records.
func rewrite(path string) (rewrittenEntry, rewrittenPrompt int, err error) {
	fh, err := os.Open(path) //nolint:gosec // the transcript path resolved for a managed session
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = fh.Close() }()
	info, err := fh.Stat()
	if err != nil {
		return 0, 0, err
	}
	err = atomicWrite(path, info.Mode().Perm(), func(out io.Writer) error {
		rewrittenEntry, rewrittenPrompt, err = rewriteStream(fh, out)
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return rewrittenEntry, rewrittenPrompt, nil
}

// rewriteStream copies a transcript from in to out with the launch markers
// rewritten.
func rewriteStream(in io.Reader, out io.Writer) (rewrittenEntry, rewrittenPrompt int, err error) {
	w := bufio.NewWriter(out)

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20) // transcripts carry multi-MB lines
	for sc.Scan() {
		line := sc.Bytes()
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			if _, werr := w.Write(line); werr != nil {
				return 0, 0, werr
			}
			if _, werr := w.WriteString("\n"); werr != nil {
				return 0, 0, werr
			}
			continue
		}
		changed := false
		if ep, ok := rec["entrypoint"].(string); ok && strings.HasPrefix(ep, "sdk") {
			rec["entrypoint"] = "cli"
			rewrittenEntry++
			changed = true
		}
		if t, _ := rec["type"].(string); t == "user" {
			if ps, ok := rec["promptSource"].(string); ok && ps == "sdk" {
				rec["promptSource"] = "typed"
				rewrittenPrompt++
				changed = true
			}
		}
		if !changed {
			if _, werr := w.Write(line); werr != nil {
				return 0, 0, werr
			}
			if _, werr := w.WriteString("\n"); werr != nil {
				return 0, 0, werr
			}
			continue
		}
		out, merr := json.Marshal(rec)
		if merr != nil {
			return 0, 0, merr
		}
		if _, werr := w.Write(out); werr != nil {
			return 0, 0, werr
		}
		if _, werr := w.WriteString("\n"); werr != nil {
			return 0, 0, werr
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if err := w.Flush(); err != nil {
		return 0, 0, err
	}
	return rewrittenEntry, rewrittenPrompt, nil
}

// atomicWrite replaces path with what write produces: into a temp file in the
// same directory, fsynced, renamed over path, and the directory fsynced so
// the rename itself survives a crash. A failed write leaves path untouched.
func atomicWrite(path string, perm os.FileMode, write func(w io.Writer) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".agentflow-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := write(tmp); err != nil {
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	d, err := os.Open(dir) //nolint:gosec // the directory of a path we just wrote
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// ProjectSlug mirrors Claude Code's transcript directory slug: the NFC-normalized
// absolute cwd with every non-alphanumeric rune replaced by '-' (verified
// against real corpus dirs like -home-user-code-myproject).
func ProjectSlug(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// copyFileAtomic copies src over dst through atomicWrite. The copy is
// owner-only: a transcript holds a whole conversation.
func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // a transcript or its backup
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return atomicWrite(dst, 0o600, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}
