package agentapi

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
	"github.com/arthurobo/agentflow/internal/claudelog/parse"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// Bounds for GET /all-sessions.
const (
	allSessionsDefaultLimit = 100
	allSessionsMaxLimit     = 500
	// A transcript's head holds its cwd and first prompt; reading further
	// would cost a full read of multi-megabyte files for nothing.
	transcriptHeadBytes = 256 << 10
	transcriptHeadLines = 200
	maxHeadEntries      = 20000
	// A first prompt stands in for a title only when the session has none;
	// a pasted log must not turn one list row into kilobytes.
	maxPromptTitleRunes = 200
)

type allSessionItem struct {
	ID        string `json:"id"`
	Engine    string `json:"engine"`
	Title     string `json:"title"`
	Cwd       string `json:"cwd"`
	Project   string `json:"project"`
	UpdatedAt int64  `json:"updatedAt"`
	State     string `json:"state"`
	RunID     string `json:"runId"`

	// transcript is the Claude transcript behind the item, if any.
	transcript string
	size       int64
	mtime      time.Time
	// titled is set when Title is final (a spawn title) and must not be
	// replaced by what the transcript says.
	titled bool
}

// handleAllSessions lists every session on this machine, newest first:
// Claude Code transcripts under ~/.claude/projects, OpenCode sessions the
// store has indexed, and managed runs, which mark the session they hold as
// live (or stand as their own item until they have a session).
//
// Everything is ordered and paged on cheap metadata (file mtimes and
// store timestamps); titles and working directories are only read for the
// page being returned. Nothing under ~/.claude is ever written.
func (s *Server) handleAllSessions(w http.ResponseWriter, r *http.Request) {
	limit := allSessionsDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		limit = min(n, allSessionsMaxLimit)
	}
	var after *allSessionsCursor
	if v := r.URL.Query().Get("cursor"); v != "" {
		c, ok := decodeAllSessionsCursor(v)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
			return
		}
		after = &c
	}

	byID := map[string]*allSessionItem{}
	if root := claudeProjectsRoot(); root != "" {
		for _, it := range claudeTranscriptItems(root) {
			if cur, ok := byID[it.ID]; !ok || it.UpdatedAt > cur.UpdatedAt {
				byID[it.ID] = it
			}
		}
	}
	if s.st != nil {
		heads, err := s.st.ListOpenCodeSessionHeads(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		for _, h := range heads {
			if _, ok := byID[h.ID]; ok {
				continue
			}
			byID[h.ID] = &allSessionItem{ID: h.ID, Engine: store.EngineOpenCode, Cwd: h.Cwd, UpdatedAt: h.UpdatedAt}
		}
	}
	if s.spawner != nil {
		runs, err := s.spawner.List(r.Context(), "")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		mergeManagedRuns(byID, runs)
	}

	items := make([]*allSessionItem, 0, len(byID))
	for _, it := range byID {
		if after == nil || after.before(it) {
			items = append(items, it)
		}
	}
	sort.Slice(items, func(i, j int) bool { return newerSession(items[i], items[j]) })
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		next = encodeAllSessionsCursor(allSessionsCursor{UpdatedAt: last.UpdatedAt, ID: last.ID})
	}
	for _, it := range items {
		if r.Context().Err() != nil {
			return
		}
		s.describeSessionItem(r, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

// newerSession is the list order: updatedAt descending, then id descending
// so equal timestamps still page deterministically.
func newerSession(a, b *allSessionItem) bool {
	if a.UpdatedAt != b.UpdatedAt {
		return a.UpdatedAt > b.UpdatedAt
	}
	return a.ID > b.ID
}

// mergeManagedRuns folds managed runs into the listed sessions. A run
// holding a listed session gives it its run id and, while the process is
// alive, its state. A live run whose session isn't listed (not written to
// disk yet, or no session id yet) is listed on its own; a finished one
// isn't, since there's nothing left to open.
func mergeManagedRuns(byID map[string]*allSessionItem, runs []*spawner.Session) {
	holder := map[string]*spawner.Session{}
	for _, run := range runs {
		key := run.SessionID
		if key == "" {
			key = run.ID
		}
		// Runs arrive newest first: keep the newest, unless it has ended and
		// an older run of the same session is still alive.
		if cur, ok := holder[key]; ok {
			if cur.State.Terminal() && !run.State.Terminal() {
				holder[key] = run
			}
			continue
		}
		holder[key] = run
	}
	for key, run := range holder {
		live := !run.State.Terminal()
		it, ok := byID[key]
		if !ok {
			if !live {
				continue
			}
			engine := run.Engine
			if engine == "" {
				engine = store.EngineClaude
			}
			it = &allSessionItem{ID: key, Engine: engine, Cwd: run.CWD}
			byID[key] = it
		}
		it.RunID = run.ID
		if live {
			it.State = string(run.State)
			it.UpdatedAt = max(it.UpdatedAt, run.UpdatedAt)
		}
		if t := strings.TrimSpace(run.Title); t != "" {
			it.Title, it.titled = t, true
		} else if it.Title == "" && it.transcript == "" && run.Prompt != "" {
			it.Title = clipTitle(run.Prompt)
		}
		if it.Cwd == "" {
			it.Cwd = run.CWD
		}
	}
}

// describeSessionItem fills in the title, cwd and project of one listed
// session.
func (s *Server) describeSessionItem(r *http.Request, it *allSessionItem) {
	switch {
	case it.transcript != "":
		head := transcriptHeads.lookup(it.transcript, it.size, it.mtime)
		if head.cwd != "" {
			it.Cwd = head.cwd
		}
		if !it.titled {
			title, err := transcriptTitles.lookup(it.transcript)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				s.log.Debug("agentd: transcript title", "session", it.ID, "err", err)
			}
			if title == "" {
				title = head.prompt
			}
			it.Title = title
		}
	case it.Engine == store.EngineOpenCode && !it.titled && s.st != nil:
		if sess, err := s.st.GetSession(r.Context(), it.ID); err == nil && sess != nil {
			it.Title = clipTitle(sess.Title)
			if it.Cwd == "" {
				it.Cwd = sess.Cwd
			}
		}
	}
	it.Project = eventmodel.ProjectFromCWD(it.Cwd)
}

// claudeTranscriptItems lists the session transcripts in root's project
// folders from directory metadata alone. Subagent transcripts live in a
// per-session subfolder (or, in older layouts, as agent-*.jsonl next to the
// sessions) and aren't sessions anyone opens, so they're skipped.
func claudeTranscriptItems(root string) []*allSessionItem {
	folders, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []*allSessionItem
	for _, f := range folders {
		if !f.IsDir() {
			continue
		}
		dir := filepath.Join(root, f.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !e.Type().IsRegular() || !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, "agent-") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if id == "" {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, &allSessionItem{
				ID: id, Engine: store.EngineClaude, UpdatedAt: info.ModTime().UnixMilli(),
				transcript: filepath.Join(dir, name), size: info.Size(), mtime: info.ModTime(),
			})
		}
	}
	return out
}

// allSessionsCursor is the last item of the previous page.
type allSessionsCursor struct {
	UpdatedAt int64  `json:"u"`
	ID        string `json:"i"`
}

// before reports whether it sorts after the cursor, i.e. belongs to a later
// page.
func (c allSessionsCursor) before(it *allSessionItem) bool {
	return newerSession(&allSessionItem{UpdatedAt: c.UpdatedAt, ID: c.ID}, it)
}

func encodeAllSessionsCursor(c allSessionsCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeAllSessionsCursor(v string) (allSessionsCursor, bool) {
	var c allSessionsCursor
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || json.Unmarshal(b, &c) != nil || c.ID == "" {
		return c, false
	}
	return c, true
}

func clipTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxPromptTitleRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxPromptTitleRunes]) + "…"
}

// transcriptHeads caches what a transcript's head says, shared by every
// Server like transcriptTitles.
var transcriptHeads = &headCache{entries: map[string]*headEntry{}}

type headEntry struct {
	size   int64
	mtime  time.Time
	cwd    string
	prompt string
}

// headCache remembers each transcript's cwd and first prompt. Both come
// from the start of an append-only file, so once both are known they stay
// valid however the file grows; until then a changed file is read again.
type headCache struct {
	mu      sync.Mutex
	entries map[string]*headEntry
}

func (c *headCache) lookup(path string, size int64, mtime time.Time) headEntry {
	c.mu.Lock()
	e := c.entries[path]
	c.mu.Unlock()
	if e != nil && ((e.cwd != "" && e.prompt != "" && size >= e.size) || (e.size == size && e.mtime.Equal(mtime))) {
		return *e
	}
	cwd, prompt := readTranscriptHead(path)
	fresh := &headEntry{size: size, mtime: mtime, cwd: cwd, prompt: prompt}
	c.mu.Lock()
	if len(c.entries) >= maxHeadEntries && c.entries[path] == nil {
		c.entries = map[string]*headEntry{}
	}
	c.entries[path] = fresh
	c.mu.Unlock()
	return *fresh
}

// readTranscriptHead reads the first cwd and the first real user prompt
// from the head of a transcript, read-only.
func readTranscriptHead(path string) (cwd, prompt string) {
	f, err := os.Open(path) // read-only
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(io.LimitReader(f, transcriptHeadBytes))
	sc.Buffer(make([]byte, 0, 64<<10), transcriptHeadBytes)
	for i := 0; i < transcriptHeadLines && sc.Scan() && (cwd == "" || prompt == ""); i++ {
		line := sc.Bytes()
		if cwd == "" && bytes.Contains(line, []byte(`"cwd"`)) {
			var rec struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &rec) == nil {
				cwd = rec.Cwd
			}
		}
		if prompt == "" && bytes.Contains(line, []byte(`"user"`)) {
			prompt = firstUserPrompt(line)
		}
	}
	return cwd, prompt
}

// firstUserPrompt returns the prompt a person typed in one transcript
// record, or "" for anything else (tool results, meta records, slash
// command wrappers).
func firstUserPrompt(line []byte) string {
	p, err := parse.Normalize(bytes.TrimSpace(line), eventmodel.SourceBackfill)
	if err != nil || p == nil {
		return ""
	}
	for _, ev := range p.Events {
		if ev.Type != eventmodel.EventUserMessage || ev.IsMeta {
			continue
		}
		if t := store.SessionTitle(ev.Content, "", ""); t != "" {
			return clipTitle(t)
		}
	}
	return ""
}
