package uploads

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestLegacyUploadsMoveWhenTheNewDirIsAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AF_UPLOADS_DIR", "")
	writeFile(t, filepath.Join(home, ".agentflow", "uploads", "run-1", "a.png"), "old")

	MigrateLegacyDir(t.Logf)

	if got := readFile(t, filepath.Join(Root(), "run-1", "a.png")); got != "old" {
		t.Fatalf("migrated file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentflow", "uploads")); !os.IsNotExist(err) {
		t.Fatalf("old dir still there: %v", err)
	}
}

// A rename cannot replace a non-empty directory, so when the new uploads dir
// already had files the old ones used to stay behind forever.
func TestLegacyUploadsMergeIntoAnExistingDirWithoutOverwriting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AF_UPLOADS_DIR", "")
	old := filepath.Join(home, ".agentflow", "uploads")
	writeFile(t, filepath.Join(old, "run-1", "a.png"), "old run-1")
	writeFile(t, filepath.Join(old, "run-2", "b.png"), "old run-2")
	writeFile(t, filepath.Join(Root(), "run-2", "b.png"), "new run-2")

	MigrateLegacyDir(t.Logf)

	if got := readFile(t, filepath.Join(Root(), "run-1", "a.png")); got != "old run-1" {
		t.Fatalf("run-1 not merged: %q", got)
	}
	if got := readFile(t, filepath.Join(Root(), "run-2", "b.png")); got != "new run-2" {
		t.Fatalf("existing file overwritten: %q", got)
	}
	if got := readFile(t, filepath.Join(old, "run-2", "b.png")); got != "old run-2" {
		t.Fatalf("conflicting old entry should stay in place, got %q", got)
	}
}
