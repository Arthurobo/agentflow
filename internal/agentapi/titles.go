package agentapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
	"github.com/arthurobo/agentflow/internal/claudelog/parse"
	"github.com/arthurobo/agentflow/internal/spawner"
)

// fillSessionTitles gives every listed run the name its transcript knows it
// by.
//
// A terminal run has no prompt, so without this the console listed every
// named session in a repo under the project name and there was no way to
// tell one agent from another. The name lives in the Claude transcript as a
// custom-title (a rename in the TUI), ai-title or summary record, so this
// reads it from there, read-only.
//
// A spawn-time title wins: it is on the row because the engineer typed it,
// and a transcript's ai-title must not rename a run he named himself.
// Failure is silent — a list with plainer labels is better than no list.
func (s *Server) fillSessionTitles(ctx context.Context, sess []*spawner.Session) {
	if len(sess) == 0 {
		return
	}
	root := claudeProjectsRoot()
	if root == "" {
		return
	}
	for _, sc := range sess {
		if ctx.Err() != nil {
			return
		}
		if sc == nil || sc.Title != "" || sc.SessionID == "" || sc.CWD == "" {
			continue
		}
		path, ok := transcriptPath(root, sc.CWD, sc.SessionID)
		if !ok {
			continue
		}
		title, err := transcriptTitles.lookup(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				s.log.Debug("agentd: transcript title", "run", sc.ID, "err", err)
			}
			continue
		}
		sc.Title = title
	}
}

// claudeProjectsRoot is ~/.claude/projects for the daemon's user, resolved per
// call so a changed HOME (tests) is honoured. Empty when HOME is unknown.
func claudeProjectsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// transcriptPath is where Claude keeps sessionID's transcript for a run in
// cwd. A session id that could escape the folder is refused.
func transcriptPath(root, cwd, sessionID string) (string, bool) {
	if strings.ContainsAny(sessionID, `/\`) || strings.Contains(sessionID, "..") {
		return "", false
	}
	return filepath.Join(root, claudeProjectDirName(cwd), sessionID+".jsonl"), true
}

// claudeProjectDirName maps a working directory to the folder name Claude
// uses for it under ~/.claude/projects: every byte that is not an ASCII
// letter or digit becomes "-". Symlinks are resolved first, because Claude
// encodes the physical path its own process sees.
func claudeProjectDirName(cwd string) string {
	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != "" {
		cwd = real
	}
	return encodeProjectName(cwd)
}

func encodeProjectName(p string) string {
	b := make([]byte, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b[i] = c
		} else {
			b[i] = '-'
		}
	}
	return string(b)
}

// transcriptTitles is shared by every Server: entries are keyed by the file
// they describe and validated against it on every lookup.
var transcriptTitles = newTitleCache()

// maxTitleLine bounds how much of one line is held in memory. Title records
// are a few hundred bytes; a longer line is a message or a tool result and
// is skipped without being buffered.
const maxTitleLine = 1 << 20

// maxTitleEntries bounds the cache. It has to hold every transcript the
// all-sessions list pages through, or paging would drop the cache and rescan
// whole files; an entry is a few hundred bytes.
const maxTitleEntries = 20000

type titleEntry struct {
	info   os.FileInfo
	size   int64
	mtime  time.Time
	offset int64 // end of the last complete line scanned
	custom string
	other  string
}

func (e *titleEntry) title() string {
	if e.custom != "" {
		return e.custom
	}
	return e.other
}

// titleCache remembers what each transcript's title was up to the byte it
// had been scanned to. Transcripts are append-only, so a file that only grew
// is scanned from where the last scan stopped.
type titleCache struct {
	mu      sync.Mutex
	entries map[string]*titleEntry
}

func newTitleCache() *titleCache {
	return &titleCache{entries: map[string]*titleEntry{}}
}

// lookup returns the transcript's current title ("" when it has none).
func (c *titleCache) lookup(path string) (string, error) {
	f, err := os.Open(path) // read-only
	if err != nil {
		c.mu.Lock()
		delete(c.entries, path)
		c.mu.Unlock()
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", os.ErrNotExist
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.entries[path]
	if prev != nil && prev.size == info.Size() && prev.mtime.Equal(info.ModTime()) && os.SameFile(prev.info, info) {
		return prev.title(), nil
	}
	e := &titleEntry{}
	if prev != nil && os.SameFile(prev.info, info) && info.Size() >= prev.size {
		// Grown in place: carry what is known and read only the new bytes.
		*e = *prev
	}
	e.info, e.size, e.mtime = info, info.Size(), info.ModTime()
	if err := scanTitles(f, e); err != nil {
		delete(c.entries, path)
		return "", err
	}
	if len(c.entries) >= maxTitleEntries && prev == nil {
		// Runs come and go; forget everything rather than grow forever. The
		// next list rescans what is still listed.
		c.entries = map[string]*titleEntry{}
	}
	c.entries[path] = e
	return e.title(), nil
}

var titleMarkers = [][]byte{[]byte(`"custom-title"`), []byte(`"ai-title"`), []byte(`"summary"`)}

// scanTitles reads complete lines from e.offset to EOF and records the latest
// title records. An unterminated last line is left for the next scan: it is
// most likely still being written.
func scanTitles(f *os.File, e *titleEntry) error {
	if _, err := f.Seek(e.offset, io.SeekStart); err != nil {
		return err
	}
	rd := bufio.NewReaderSize(f, 64<<10)
	var line []byte
	overflow := false
	for {
		chunk, err := rd.ReadSlice('\n')
		if len(chunk) > 0 && !overflow {
			if len(line)+len(chunk) > maxTitleLine {
				overflow = true
				line = line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			e.offset += int64(len(line))
			if overflow {
				// The overflowing bytes were not kept; count them from the
				// reader's position instead.
				pos, serr := f.Seek(0, io.SeekCurrent)
				if serr != nil {
					return serr
				}
				e.offset = pos - int64(rd.Buffered())
			} else {
				applyTitleLine(e, line)
			}
			line = line[:0]
			overflow = false
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}

func applyTitleLine(e *titleEntry, line []byte) {
	candidate := false
	for _, m := range titleMarkers {
		if bytes.Contains(line, m) {
			candidate = true
			break
		}
	}
	if !candidate {
		return
	}
	p, err := parse.Normalize(bytes.TrimSpace(line), eventmodel.SourceBackfill)
	if err != nil || p == nil || len(p.Events) == 0 {
		return
	}
	content := strings.TrimSpace(p.Events[0].Content)
	if content == "" {
		return
	}
	switch p.RawType {
	case "custom-title":
		e.custom = content
	case "ai-title", "summary":
		e.other = content
	}
}
