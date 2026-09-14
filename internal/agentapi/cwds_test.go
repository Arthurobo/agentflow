package agentapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

type cwdsResponse struct {
	Items []cwdItem `json:"items"`
}

func getCwds(t *testing.T, srv *Server, token string) (int, cwdsResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agentd/cwds", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var out cwdsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// projectFolder writes a Claude project folder whose newest transcript
// records cwd, with that transcript's mtime at unix ms `at`.
func projectFolder(t *testing.T, home, name, recordedCwd string, at int64) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(dir, "older.jsonl")
	writeLines(t, older, `{"type":"user","cwd":"/somewhere/else"}`)
	_ = os.Chtimes(older, time.UnixMilli(at-500), time.UnixMilli(at-500))
	newest := filepath.Join(dir, "newest.jsonl")
	writeLines(t, newest,
		`{"type":"summary","summary":"no cwd on this line"}`,
		fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":"s"}`, recordedCwd))
	if err := os.Chtimes(newest, time.UnixMilli(at), time.UnixMilli(at)); err != nil {
		t.Fatal(err)
	}
}

func TestCwdsMergesManagedRunsAndClaudeProjects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv, st := newTestServer(t)
	ctx := context.Background()

	work := t.TempDir()
	valid := filepath.Join(work, "app_one.v2")
	other := filepath.Join(work, "other")
	for _, d := range []string{valid, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for i, m := range []struct {
		cwd string
		at  int64
	}{
		{"/managed/a", 5000},
		{"/managed/b", 1000},
		{valid, 2000},
		{"/managed/a", 4000},
	} {
		if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
			ID: fmt.Sprintf("run-%d", i), Kind: "tty", State: "finished",
			CWD: m.cwd, StartedAt: m.at, UpdatedAt: m.at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A live project, newer than its managed run: the later time wins.
	projectFolder(t, home, claudeProjectDirName(valid), valid, 3000)
	// A project whose directory is gone.
	gone := filepath.Join(work, "deleted")
	projectFolder(t, home, encodeProjectName(gone), gone, 9000)
	// A folder with no transcript at all.
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects", "-no-transcripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(home, ".claude", "projects", "-no-transcripts", "notes.txt"), `{"cwd":"/"}`)
	// A folder whose transcript names a directory that does not encode to it.
	projectFolder(t, home, "-not-the-right-name", other, 8000)

	code, out := getCwds(t, srv, "secret-token")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	want := []cwdItem{
		{Cwd: "/managed/a", LastUsedAt: 5000},
		{Cwd: valid, LastUsedAt: 3000},
		{Cwd: "/managed/b", LastUsedAt: 1000},
	}
	if !reflect.DeepEqual(out.Items, want) {
		t.Fatalf("items = %+v\nwant   %+v", out.Items, want)
	}
}

func TestCwdsIsCappedAndNeedsADevice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, st := newTestServer(t)
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
			ID: fmt.Sprintf("run-%d", i), Kind: "tty", State: "finished",
			CWD: fmt.Sprintf("/w/%02d", i), StartedAt: int64(1000 + i), UpdatedAt: int64(1000 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := getCwds(t, srv, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", code)
	}
	code, out := getCwds(t, srv, "secret-token")
	if code != http.StatusOK || len(out.Items) != maxCwdItems {
		t.Fatalf("status %d, %d items; want 200 and %d", code, len(out.Items), maxCwdItems)
	}
	if out.Items[0].Cwd != "/w/59" || out.Items[49].Cwd != "/w/10" {
		t.Fatalf("want newest first: first %q last %q", out.Items[0].Cwd, out.Items[49].Cwd)
	}
}

type fileState struct {
	Size  int64
	Mode  fs.FileMode
	MTime time.Time
}

func snapshotTree(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[p] = fileState{Size: info.Size(), Mode: info.Mode(), MTime: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// Listing directories and titles reads ~/.claude and must never change it:
// a past bug renamed the owner's live sessions from exactly this corpus.
func TestCwdsAndTitlesLeaveClaudeDirectoryUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv, st := newTestServer(t)
	cwd := t.TempDir()
	projectFolder(t, home, claudeProjectDirName(cwd), cwd, time.Now().Add(-time.Hour).UnixMilli())
	folder := filepath.Join(home, ".claude", "projects", claudeProjectDirName(cwd))
	writeLines(t, filepath.Join(folder, "sid-x.jsonl"), customTitle("keep me"))
	past := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(filepath.Join(folder, "sid-x.jsonl"), past, past)
	if err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: "run-x", SessionID: "sid-x", Kind: "tty", State: "finished", CWD: cwd,
		StartedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	claudeDir := filepath.Join(home, ".claude")
	before := snapshotTree(t, claudeDir)
	if code, out := getCwds(t, srv, "secret-token"); code != http.StatusOK || len(out.Items) == 0 {
		t.Fatalf("cwds: %d %+v", code, out)
	}
	rec := doGet(t, srv, "/api/v1/agentd/sessions", http.StatusOK)
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("sessions: %s", rec.Body.String())
	}
	var list struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Items[0].Title != "keep me" {
		t.Fatalf("title lookup did not run: %s", rec.Body.String())
	}
	after := snapshotTree(t, claudeDir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("~/.claude changed:\nbefore %+v\nafter  %+v", before, after)
	}
}
