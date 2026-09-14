package agentapi

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/spawner"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(joinLines(lines...)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func appendRaw(t *testing.T, path, raw string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(raw); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func joinLines(lines ...string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func titleOf(t *testing.T, path string) string {
	t.Helper()
	got, err := transcriptTitles.lookup(path)
	if err != nil {
		t.Fatalf("lookup %s: %v", path, err)
	}
	return got
}

const (
	userLine = `{"type":"user","message":{"role":"user","content":"please write a summary"},"sessionId":"s"}`
)

func customTitle(v string) string {
	return `{"type":"custom-title","customTitle":"` + v + `","sessionId":"s"}`
}
func aiTitle(v string) string    { return `{"type":"ai-title","aiTitle":"` + v + `","sessionId":"s"}` }
func summaryRec(v string) string { return `{"type":"summary","summary":"` + v + `","leafUuid":"u"}` }

// A rename in the TUI is the deliberate name; a title Claude generated later
// must not replace it.
func TestTranscriptCustomTitleBeatsALaterAITitle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, p, userLine, customTitle("Mine"), aiTitle("Auto"), summaryRec("Sum"))
	if got := titleOf(t, p); got != "Mine" {
		t.Fatalf("title = %q, want the custom title", got)
	}
}

func TestTranscriptLatestTitleOfEachKindWins(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jsonl")
	writeLines(t, p, customTitle("first"), userLine, customTitle("second"))
	if got := titleOf(t, p); got != "second" {
		t.Fatalf("title = %q, want the latest custom title", got)
	}
	q := filepath.Join(dir, "b.jsonl")
	writeLines(t, q, aiTitle("generated"), userLine, summaryRec("summarised"))
	if got := titleOf(t, q); got != "summarised" {
		t.Fatalf("title = %q, want the latest of ai-title/summary", got)
	}
	r := filepath.Join(dir, "c.jsonl")
	writeLines(t, r, userLine)
	if got := titleOf(t, r); got != "" {
		t.Fatalf("title = %q, want none for a transcript without title records", got)
	}
}

// Transcripts are append-only while a session runs: a rename appended later is
// picked up, and a half-written line is not read until it is complete.
func TestTranscriptTitleAppendIsPickedUp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, p, userLine, aiTitle("first"))
	if got := titleOf(t, p); got != "first" {
		t.Fatalf("title = %q", got)
	}
	full := customTitle("renamed")
	appendRaw(t, p, full[:20])
	if got := titleOf(t, p); got != "first" {
		t.Fatalf("an unterminated line must not be read yet, got %q", got)
	}
	appendRaw(t, p, full[20:]+"\n")
	if got := titleOf(t, p); got != "renamed" {
		t.Fatalf("title = %q, want the appended rename", got)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	transcriptTitles.mu.Lock()
	off := transcriptTitles.entries[p].offset
	transcriptTitles.mu.Unlock()
	if off != info.Size() {
		t.Fatalf("scan offset = %d, want the file size %d", off, info.Size())
	}
}

// A file that shrank or was swapped for another is read from the start again,
// never continued from a stale offset or answered from a stale title.
func TestTranscriptTitleRescansAReplacedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeLines(t, p, userLine, userLine, customTitle("old"))
	if got := titleOf(t, p); got != "old" {
		t.Fatalf("title = %q", got)
	}

	// Replaced by rename with a longer file that has no custom title.
	tmp := filepath.Join(dir, "new.tmp")
	writeLines(t, tmp, userLine, userLine, userLine, aiTitle("fresh"))
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	if got := titleOf(t, p); got != "fresh" {
		t.Fatalf("after replace title = %q, want fresh", got)
	}

	// Truncated in place to something shorter.
	writeLines(t, p, customTitle("short"))
	if got := titleOf(t, p); got != "short" {
		t.Fatalf("after shrink title = %q, want short", got)
	}
}

// A huge line (a tool result) is skipped without being buffered, and the scan
// offset still lands after it so appends keep working.
func TestTranscriptTitleSurvivesAHugeLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	huge := `{"type":"user","summary":"` + strings.Repeat("x", 3*maxTitleLine) + `"}`
	writeLines(t, p, customTitle("before"), huge)
	if got := titleOf(t, p); got != "before" {
		t.Fatalf("title = %q", got)
	}
	appendRaw(t, p, customTitle("after")+"\n")
	if got := titleOf(t, p); got != "after" {
		t.Fatalf("title after a huge line = %q", got)
	}
}

func TestFillSessionTitlesReadsTranscriptsAndKeepsSpawnTitles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	folder := filepath.Join(home, ".claude", "projects", claudeProjectDirName(cwd))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(folder, "sid-1.jsonl"), customTitle("from transcript"))
	writeLines(t, filepath.Join(folder, "sid-2.jsonl"), customTitle("ignored"))

	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	runs := []*spawner.Session{
		{ID: "r1", SessionID: "sid-1", CWD: cwd},
		{ID: "r2", SessionID: "sid-2", CWD: cwd, Title: "spawn title"},
		{ID: "r3", SessionID: "../sid-1", CWD: cwd},
		{ID: "r4", SessionID: "sid-missing", CWD: cwd},
	}
	s.fillSessionTitles(context.Background(), runs)
	want := []string{"from transcript", "spawn title", "", ""}
	for i, r := range runs {
		if r.Title != want[i] {
			t.Fatalf("%s title = %q, want %q", r.ID, r.Title, want[i])
		}
	}
}
