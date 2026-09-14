package store

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestOpenAndMigrate(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	v, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if want := latestMigration(t); v != want {
		t.Fatalf("expected %q (latest migration), got %q", want, v)
	}
	ok, err := s.FTS5Enabled(context.Background())
	if err != nil || !ok {
		t.Fatalf("fts5 not enabled: %v %v", ok, err)
	}
	if _, err := s.Counts(context.Background()); err != nil {
		t.Fatalf("counts: %v", err)
	}
}

// latestMigration returns the lexicographically-greatest migration filename
// shipped in the package (filenames sort the same numerically because every
// name starts with zero-padded 4-digit ids). Used by tests so adding a
// migration doesn't break a hard-coded version assertion.
func latestMigration(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		t.Fatalf("no migrations found")
	}
	sort.Strings(names)
	return names[len(names)-1]
}

func TestInsertAndList(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	ev := func(seq int64, typ string, content string) Incoming {
		return Incoming{
			SessionID: "sess-1", Seq: seq, Event: typ, Content: content,
			TS: seq * 1000, Source: "backfill",
		}
	}
	b := &Batch{
		Events: []Incoming{
			ev(1, "user_message", "hello world"),
			ev(2, "assistant_message", "hi there"),
			ev(3, "session_state", "mode"),
			ev(4, "session_state", "custom-title"),
		},
		Session: &SessionMeta{SessionID: "sess-1", CWD: "/home/u/code/foo", Project: "foo", FilePath: "/p.jsonl"},
	}
	if err := s.InsertBatch(ctx, b); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// idempotent re-run: same batch again must not grow anything
	if err := s.InsertBatch(ctx, b); err != nil {
		t.Fatalf("reinsert: %v", err)
	}

	c, err := s.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if c.Events != 4 {
		t.Fatalf("expected 4 events, got %d", c.Events)
	}
	// identity-less snapshot records must NOT collapse: four distinct rows
	sess, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess == nil || sess.EventCount != 4 {
		t.Fatalf("expected session with 4 events, got %+v", sess)
	}
	if sess.FirstPrompt != "hello world" {
		t.Fatalf("expected first prompt hello world, got %q", sess.FirstPrompt)
	}
	if sess.Project != "foo" {
		t.Fatalf("expected project foo, got %q", sess.Project)
	}

	// key check for the snapshot events
	got := func() string { e := ev(3, "session_state", "mode"); return e.Key() }()
	want := "sess-1,-,session_state|none:3"
	if got != want {
		t.Fatalf("snapshot key: got %q want %q", got, want)
	}
}

func TestEventKey(t *testing.T) {
	base := Incoming{SessionID: "S", Event: "user_message", Seq: 5}
	if base.Key() != "S,-,user_message|none:5" {
		t.Fatalf("identity-less key wrong: %s", base.Key())
	}
	base.UUID = "u1"
	if base.Key() != "S,u1,user_message" {
		t.Fatalf("uuid key wrong: %s", base.Key())
	}
	base.UUID = ""
	base.ToolUseID = "toolu_1"
	if base.Key() != "S,toolu_1,user_message" {
		t.Fatalf("tool key wrong: %s", base.Key())
	}
	base.ToolUseID = ""
	base.ContentHash = "sha256:abc"
	if base.Key() != "S,sha256:abc,user_message" {
		t.Fatalf("hash key wrong: %s", base.Key())
	}
}

func mustOpen(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

// TestSessionTitlePriority pins the sessions-list label contract: claude's own
// session name (ai-title/custom-title/summary snapshot) beats a user-assigned
// name, which beats the first prompt.
func TestSessionTitlePriority(t *testing.T) {
	if got := SessionTitle("first prompt", "Fix the store test", ""); got != "Fix the store test" {
		t.Errorf("state title must win: %q", got)
	}
	named := `The user named this session "my session"`
	if got := SessionTitle("first prompt", "", named); got != "my session" {
		t.Errorf("named hint must win over prompt: %q", got)
	}
	if got := SessionTitle("first prompt", " ", named); got != "my session" {
		t.Errorf("blank state title falls through: %q", got)
	}
	if got := SessionTitle("first prompt", "", ""); got != "first prompt" {
		t.Errorf("prompt is the fallback: %q", got)
	}
	if got := SessionTitle("<command-name>clear", "", ""); got != "" {
		t.Errorf("command noise yields untitled: %q", got)
	}
}
