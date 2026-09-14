package resumable

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectSlug(t *testing.T) {
	cases := map[string]string{
		"/home/user/Desktop/code/webapp": "-home-user-Desktop-code-webapp",
		"/home/user/Desktop/code/myapp":  "-home-user-Desktop-code-myapp",
		"/tmp/opencode/phase4ws":         "-tmp-opencode-phase4ws",
	}
	for in, want := range cases {
		if got := ProjectSlug(in); got != want {
			t.Errorf("projectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRewriteResumable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deadbeef.jsonl")
	lines := []string{
		`{"type":"queue-operation","operation":"enqueue","content":"<task/>"}`,
		`{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"deadbeef","entrypoint":"sdk-cli","promptSource":"sdk"}`,
		`{"type":"user","message":{"role":"user","content":"typed later"},"sessionId":"deadbeef","entrypoint":"cli","promptSource":"typed"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]},"sessionId":"deadbeef","entrypoint":"sdk-cli"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	nE, nP, err := rewrite(path)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if nE != 2 || nP != 1 {
		t.Fatalf("expected 2 entrypoint + 1 promptSource rewrites, got %d/%d", nE, nP)
	}

	out := mustRead(t, path)
	seen := []struct{ ep, ps string }{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("output not valid JSON: %v", err)
		}
		seen = append(seen, struct{ ep, ps string }{
			ep: firstString(rec["entrypoint"]), ps: firstString(rec["promptSource"]),
		})
	}
	if seen[0].ep != "" { // queue-op untouched (no entrypoint)
		t.Errorf("record 0 unexpectedly gained entrypoint=%q", seen[0].ep)
	}
	if seen[1].ep != "cli" || seen[1].ps != "typed" {
		t.Errorf("record 1 not rewritten: %+v", seen[1])
	}
	if seen[2].ep != "cli" || seen[2].ps != "typed" {
		t.Errorf("record 2 (already typed) mutated: %+v", seen[2])
	}
	if seen[3].ep != "cli" {
		t.Errorf("record 3 not rewritten: %+v", seen[3])
	}
}

func firstString(v any) string {
	s, _ := v.(string)
	return s
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func writeTranscript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "deadbeef.jsonl")
	body := `{"type":"user","message":{"role":"user","content":"hi"},"sessionId":"deadbeef","entrypoint":"sdk-cli","promptSource":"sdk"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, ".agentflow-tmp-*"))
	return matches
}

// Terminate can make a backup every time; a transcript is megabytes. Only the
// newest three are kept, and "newest" is by the time a backup names, not by
// how its name sorts.
func TestBackupsAreCappedAtThree(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir)
	// A backup from an older release, named in seconds.
	legacy := path + backupSuffix + "1700000000"
	if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_800_000_000, 0)
	var last string
	for i := 0; i < 5; i++ {
		b, _, _, err := backupAndRewrite(path, base.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		last = b
	}
	backups := listBackups(path)
	if len(backups) != maxBackups {
		t.Fatalf("want %d backups, got %d: %v", maxBackups, len(backups), backups)
	}
	if backups[len(backups)-1] != last {
		t.Fatalf("the newest backup must survive: %v, want %s last", backups, last)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("the oldest backup (a seconds-named one) must be the first to go")
	}
	if left := tempLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// A write that fails part way leaves the original file exactly as it was.
func TestAtomicWriteLeavesTheOriginalOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir)
	before := mustRead(t, path)
	err := atomicWrite(path, 0o600, func(w io.Writer) error {
		_, _ = w.Write([]byte("half a transcr"))
		return errors.New("disk full")
	})
	if err == nil {
		t.Fatal("the failure must be reported")
	}
	if got := mustRead(t, path); got != before {
		t.Fatalf("the original was touched: %q", got)
	}
	if left := tempLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// Undo puts the newest backup back, byte for byte, keeping the file
// owner-only.
func TestUndoRestoresTheNewestBackup(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir)
	original := mustRead(t, path)
	if _, _, _, err := backupAndRewrite(path, time.Now()); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if mustRead(t, path) == original {
		t.Fatal("the rewrite must change the transcript")
	}
	if _, err := restoreNewest(path); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := mustRead(t, path); got != original {
		t.Fatalf("undo must restore the original: %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("the restored transcript must stay owner-only: %v", info.Mode())
	}
	if left := tempLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}
