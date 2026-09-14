package agentapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Bounds for GET /cwds.
const (
	maxCwdItems        = 50
	maxProjectFolders  = 500
	cwdProbeBytes      = 64 << 10
	cwdProbeLines      = 50
	managedCwdsToMerge = 200
)

type cwdItem struct {
	Cwd        string `json:"cwd"`
	LastUsedAt int64  `json:"lastUsedAt"`
}

// handleCwds lists working directories worth offering when starting a run:
// the ones managed runs have used, plus the projects Claude Code itself has
// sessions for on this machine. Most recent first, at most 50.
//
// It only ever reads under ~/.claude: files are opened read-only and nothing
// there is created, renamed or touched.
func (s *Server) handleCwds(w http.ResponseWriter, r *http.Request) {
	latest := map[string]int64{}
	add := func(cwd string, at int64) {
		if cwd == "" {
			return
		}
		if cur, ok := latest[cwd]; !ok || at > cur {
			latest[cwd] = at
		}
	}
	if s.st != nil {
		rows, err := s.st.RecentManagedCwds(r.Context(), managedCwdsToMerge)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		for _, row := range rows {
			add(row.Cwd, row.LastUsedAt)
		}
	}
	if root := claudeProjectsRoot(); root != "" {
		for _, it := range claudeProjectCwds(root) {
			add(it.Cwd, it.LastUsedAt)
		}
	}

	items := make([]cwdItem, 0, len(latest))
	for cwd, at := range latest {
		items = append(items, cwdItem{Cwd: cwd, LastUsedAt: at})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].LastUsedAt != items[j].LastUsedAt {
			return items[i].LastUsedAt > items[j].LastUsedAt
		}
		return items[i].Cwd < items[j].Cwd
	})
	if len(items) > maxCwdItems {
		items = items[:maxCwdItems]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// claudeProjectCwds recovers the real directories behind the folders in
// root. A folder name is a lossy encoding of the path, so the path is read
// from a transcript's own cwd field and kept only when it still exists and
// encodes back to that folder's name.
func claudeProjectCwds(root string) []cwdItem {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	type folder struct {
		name  string
		mtime int64
	}
	folders := make([]folder, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		folders = append(folders, folder{name: e.Name(), mtime: info.ModTime().UnixMilli()})
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].mtime > folders[j].mtime })
	if len(folders) > maxProjectFolders {
		folders = folders[:maxProjectFolders]
	}

	out := []cwdItem{}
	for _, f := range folders {
		dir := filepath.Join(root, f.name)
		transcript, at := newestTranscript(dir)
		if transcript == "" {
			continue
		}
		cwd := transcriptCwd(transcript)
		if cwd == "" || !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
			continue
		}
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			continue
		}
		if encodeProjectName(cwd) != f.name && claudeProjectDirName(cwd) != f.name {
			continue
		}
		out = append(out, cwdItem{Cwd: cwd, LastUsedAt: at})
	}
	return out
}

// newestTranscript returns the most recently modified regular *.jsonl file in
// dir and its mtime (unix ms), or "" when there is none.
func newestTranscript(dir string) (string, int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0
	}
	best, bestAt := "", int64(0)
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if at := info.ModTime().UnixMilli(); best == "" || at > bestAt {
			best, bestAt = filepath.Join(dir, e.Name()), at
		}
	}
	return best, bestAt
}

// transcriptCwd reads the first cwd field from the head of a transcript.
func transcriptCwd(path string) string {
	f, err := os.Open(path) // read-only
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(io.LimitReader(f, cwdProbeBytes))
	sc.Buffer(make([]byte, 0, 4096), cwdProbeBytes)
	for i := 0; i < cwdProbeLines && sc.Scan(); i++ {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"cwd"`)) {
			continue
		}
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Cwd != "" {
			return rec.Cwd
		}
	}
	return ""
}
