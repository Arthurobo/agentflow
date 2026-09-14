package agentapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

type allSessionsPage struct {
	Items []struct {
		ID        string `json:"id"`
		Engine    string `json:"engine"`
		Title     string `json:"title"`
		Cwd       string `json:"cwd"`
		Project   string `json:"project"`
		UpdatedAt int64  `json:"updatedAt"`
		State     string `json:"state"`
		RunID     string `json:"runId"`
	} `json:"items"`
	NextCursor string `json:"nextCursor"`
}

// newAllSessionsServer builds a server over a store in a fresh temp HOME and
// seeds the device the requests authenticate as.
func newAllSessionsServer(t *testing.T) (string, *store.Store, *agentapi.Server) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	st, err := store.Open(filepath.Join(t.TempDir(), "all.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ingSvc := ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"})
	srv := agentapi.New(st, spawner.New(st, ingSvc, log), log, "test")
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "dev-all", Name: "test", MachineID: "test", Kind: "device", TokenHash: store.HashToken("test-token"),
	}, 0); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	return home, st, srv
}

func getAllSessions(t *testing.T, srv *agentapi.Server, query string) (int, allSessionsPage) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/agentd/all-sessions"+query, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var page allSessionsPage
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v: %s", err, rr.Body.String())
		}
	}
	return rr.Code, page
}

func setMtime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// snapshotTree records every path under root with its mode, size, mtime
// and content hash.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum := ""
		if d.Type().IsRegular() {
			b, err := os.ReadFile(path) //nolint:gosec // a file the test wrote
			if err != nil {
				return err
			}
			h := sha256.Sum256(b)
			sum = hex.EncodeToString(h[:])
		}
		out[path] = fmt.Sprintf("%v %d %d %s", info.Mode(), info.Size(), info.ModTime().UnixNano(), sum)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return out
}

func TestAllSessionsMergesTranscriptsOpenCodeAndRuns(t *testing.T) {
	home, st, srv := newAllSessionsServer(t)
	ctx := context.Background()
	now := time.Now()
	projects := filepath.Join(home, ".claude", "projects")
	cwd := "/home/u/code/webapp"
	folder := filepath.Join(projects, "-home-u-code-webapp")

	writeTranscript(t, home, cwd, "sess-a",
		`{"type":"user","cwd":"/home/u/code/webapp","message":{"role":"user","content":"first ask"},"sessionId":"sess-a"}`,
		`{"type":"custom-title","customTitle":"Named A","sessionId":"sess-a"}`,
	)
	writeTranscript(t, home, cwd, "sess-b",
		`{"type":"user","cwd":"/home/u/code/webapp","message":{"role":"user","content":"fix the login bug"},"sessionId":"sess-b"}`,
	)
	writeTranscript(t, home, "/home/u/code/api", "sess-c",
		`{"type":"user","cwd":"/home/u/code/api","message":{"role":"user","content":"live one"},"sessionId":"sess-c"}`,
	)
	writeTranscript(t, home, cwd, "sess-d",
		`{"type":"user","cwd":"/home/u/code/webapp","isMeta":true,"message":{"role":"user","content":"caveat"},"sessionId":"sess-d"}`,
		`{"type":"user","cwd":"/home/u/code/webapp","message":{"role":"user","content":"<command-name>/model</command-name>"},"sessionId":"sess-d"}`,
		`{"type":"user","cwd":"/home/u/code/webapp","message":{"role":"user","content":"the real prompt"},"sessionId":"sess-d"}`,
	)
	// Subagent transcripts, in the older flat layout and the per-session
	// folder layout: neither is a session.
	writeTranscript(t, home, cwd, "agent-1234",
		`{"type":"user","cwd":"/home/u/code/webapp","isSidechain":true,"message":{"role":"user","content":"sub"}}`,
	)
	if err := os.MkdirAll(filepath.Join(folder, "sess-a", "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "sess-a", "subagents", "agent-x.jsonl"), []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setMtime(t, filepath.Join(folder, "sess-a.jsonl"), now.Add(-1*time.Hour))
	setMtime(t, filepath.Join(folder, "sess-b.jsonl"), now.Add(-2*time.Hour))
	setMtime(t, filepath.Join(projects, "-home-u-code-api", "sess-c.jsonl"), now.Add(-3*time.Hour))
	setMtime(t, filepath.Join(folder, "sess-d.jsonl"), now.Add(-4*time.Hour))

	// An OpenCode session the store has indexed.
	if err := st.InsertBatch(ctx, &store.Batch{
		Events: []store.Incoming{{SessionID: "ses_oc", Seq: 1, Event: "user_message", Content: "opencode prompt",
			TS: now.Add(-90 * time.Minute).UnixMilli(), Source: "backfill"}},
		Session: &store.SessionMeta{SessionID: "ses_oc", CWD: "/home/u/code/mobile", Project: "/home/u/code/mobile",
			FilePath: filepath.Join("opencode", "session", "prj", "ses_oc.json")},
	}); err != nil {
		t.Fatalf("seed opencode: %v", err)
	}

	seedRun := func(m *store.ManagedSession) {
		t.Helper()
		if m.Kind == "" {
			m.Kind = "tty"
		}
		if err := st.UpsertManagedSession(ctx, m); err != nil {
			t.Fatalf("seed run %s: %v", m.ID, err)
		}
	}
	seedRun(&store.ManagedSession{ID: "run_c", SessionID: "sess-c", Engine: "claude", State: "running",
		CWD: "/home/u/code/api", StartedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli()})
	seedRun(&store.ManagedSession{ID: "run_b_old", SessionID: "sess-b", Engine: "claude", State: "stopped",
		CWD: cwd, UpdatedAt: now.Add(-3 * time.Hour).UnixMilli()})
	seedRun(&store.ManagedSession{ID: "run_b", SessionID: "sess-b", Engine: "claude", State: "stopped",
		CWD: cwd, UpdatedAt: now.Add(-30 * time.Minute).UnixMilli()})
	seedRun(&store.ManagedSession{ID: "run_unbound", Engine: "opencode", State: "starting",
		CWD: "/home/u/code/mobile", Title: "Mobile work", UpdatedAt: now.Add(-10 * time.Second).UnixMilli()})
	seedRun(&store.ManagedSession{ID: "run_dead", Engine: "claude", State: "crashed",
		CWD: cwd, UpdatedAt: now.UnixMilli()})

	before := snapshotTree(t, filepath.Join(home, ".claude"))
	code, page := getAllSessions(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	type row struct{ id, engine, title, cwd, project, state, runID string }
	var got []row
	for _, it := range page.Items {
		got = append(got, row{it.ID, it.Engine, it.Title, it.Cwd, it.Project, it.State, it.RunID})
	}
	want := []row{
		{"sess-c", "claude", "live one", "/home/u/code/api", "api", "running", "run_c"},
		{"run_unbound", "opencode", "Mobile work", "/home/u/code/mobile", "mobile", "starting", "run_unbound"},
		{"sess-a", "claude", "Named A", cwd, "webapp", "", ""},
		{"ses_oc", "opencode", "opencode prompt", "/home/u/code/mobile", "mobile", "", ""},
		{"sess-b", "claude", "fix the login bug", cwd, "webapp", "", "run_b"},
		{"sess-d", "claude", "the real prompt", cwd, "webapp", "", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items:\n got %+v\nwant %+v", got, want)
	}
	if page.NextCursor != "" {
		t.Fatalf("nextCursor = %q on the only page", page.NextCursor)
	}
	if page.Items[0].UpdatedAt != now.UnixMilli() {
		t.Fatalf("a live run should lift its session to the run's update time, got %d", page.Items[0].UpdatedAt)
	}
	if page.Items[2].UpdatedAt != now.Add(-1*time.Hour).UnixMilli() {
		t.Fatalf("a transcript's updatedAt is its mtime, got %d", page.Items[2].UpdatedAt)
	}

	// A second, cached listing must agree, and neither may have touched the
	// Claude directory.
	if _, again := getAllSessions(t, srv, ""); !reflect.DeepEqual(again, page) {
		t.Fatalf("cached listing differs:\n%+v\n%+v", again, page)
	}
	if after := snapshotTree(t, filepath.Join(home, ".claude")); !reflect.DeepEqual(before, after) {
		t.Fatalf("~/.claude changed while listing:\nbefore %v\nafter  %v", before, after)
	}
}

func TestAllSessionsPagesByCursor(t *testing.T) {
	home, _, srv := newAllSessionsServer(t)
	now := time.Now()
	folder := filepath.Join(home, ".claude", "projects", "-w")
	for i := range 7 {
		id := fmt.Sprintf("s%02d", i)
		writeTranscript(t, home, "/w", id, `{"type":"user","cwd":"/w","message":{"role":"user","content":"p"}}`)
		// Two pairs share a timestamp, so the id tiebreak is exercised.
		setMtime(t, filepath.Join(folder, id+".jsonl"), now.Add(-time.Duration(i/2)*time.Minute))
	}
	// Newer first; within an equal mtime, ids descending.
	want := []string{"s01", "s00", "s03", "s02", "s05", "s04", "s06"}

	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination does not terminate")
		}
		q := "?limit=2"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		code, page := getAllSessions(t, srv, q)
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if len(page.Items) > 2 {
			t.Fatalf("page of %d items with limit 2", len(page.Items))
		}
		for _, it := range page.Items {
			got = append(got, it.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged ids %v, want %v", got, want)
	}

	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=abc", "?cursor=!!!", "?cursor=e30", "?cursor=bm90anNvbg"} {
		if code, _ := getAllSessions(t, srv, q); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, code)
		}
	}
	if code, page := getAllSessions(t, srv, "?limit=100000"); code != http.StatusOK || len(page.Items) != 7 {
		t.Fatalf("an oversized limit is capped, not refused: %d %d", code, len(page.Items))
	}
}

func TestAllSessionsRequiresADevice(t *testing.T) {
	_, _, srv := newAllSessionsServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/agentd/all-sessions", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rr.Code)
	}
}

// Two thousand transcripts must list quickly once their heads and titles are
// cached: the phone opens this list every time it opens the app.
func TestAllSessionsListsThousandsOfTranscriptsQuickly(t *testing.T) {
	home, _, srv := newAllSessionsServer(t)
	root := filepath.Join(home, ".claude", "projects")
	filler := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`
	for p := range 20 {
		dir := filepath.Join(root, fmt.Sprintf("-home-u-p%02d", p))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range 100 {
			id := fmt.Sprintf("%02d-%03d", p, i)
			body := fmt.Sprintf(`{"type":"user","cwd":"/home/u/p%02d","message":{"role":"user","content":"prompt %s"},"sessionId":"%s"}`+"\n", p, id, id)
			for range 20 {
				body += filler + "\n"
			}
			if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if code, page := getAllSessions(t, srv, "?limit=500"); code != http.StatusOK || len(page.Items) != 500 || page.NextCursor == "" {
		t.Fatalf("cold listing: %d, %d items", code, len(page.Items))
	}
	start := time.Now()
	code, page := getAllSessions(t, srv, "?limit=500")
	elapsed := time.Since(start)
	if code != http.StatusOK || len(page.Items) != 500 {
		t.Fatalf("warm listing: %d, %d items", code, len(page.Items))
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("warm listing of 2000 transcripts took %s", elapsed)
	}
}
