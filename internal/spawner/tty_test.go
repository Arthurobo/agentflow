// tty_test.go — the session-id discovery contracts: newest-wins INSIDE the
// run's own project folder, never a foreign folder, and the exclusion guard that stops loop members claiming each
// other's transcripts (the identity-collision bug).
package spawner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCorpus(t *testing.T, root, dir, name, sid string, mod time.Time) {
	t.Helper()
	p := filepath.Join(root, dir)
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f := filepath.Join(p, name)
	line := `{"type":"user","sessionId":"` + sid + `"}` + "\n"
	if err := os.WriteFile(f, []byte(line), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(f, mod, mod); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestDiscoverSessionIDExcludesClaimed(t *testing.T) {
	root := t.TempDir()
	cwd := "/tmp/opencode/loop-target"
	now := time.Now()
	// two transcripts in the SAME project dir; bbb is newest
	writeCorpus(t, root, "-tmp-opencode-loop-target", "aaa.jsonl", "SID-A", now.Add(-6*time.Second))
	writeCorpus(t, root, "-tmp-opencode-loop-target", "bbb.jsonl", "SID-B", now.Add(-2*time.Second))
	old := now.Add(-time.Hour)

	// no exclusions: newest wins
	sid, _ := discoverSessionIDIn(root, cwd, old, nil)
	if sid != "SID-B" {
		t.Fatalf("newest-wins broken: got %q", sid)
	}

	// SID-B already claimed by another member: discovery must fall back to A
	sid, _ = discoverSessionIDIn(root, cwd, old, map[string]bool{"SID-B": true})
	if sid != "SID-A" {
		t.Fatalf("exclusion ignored: got %q, want SID-A", sid)
	}

	// both claimed (e.g. a third member spawning): find NOTHING rather than
	// steal someone's transcript
	sid, _ = discoverSessionIDIn(root, cwd, old, map[string]bool{"SID-B": true, "SID-A": true})
	if sid != "" {
		t.Fatalf("claimed transcript stolen: got %q", sid)
	}
}

func TestEncodeProjectDirMatchesClaude(t *testing.T) {
	// real folder names observed under ~/.claude/projects
	cases := map[string]string{
		"/home/user/Desktop/code/myapp":            "-home-user-Desktop-code-myapp",
		"/home/user/Desktop/code/webapp/MobileApp": "-home-user-Desktop-code-webapp-MobileApp",
		"/tmp/x.y/z_w": "-tmp-x-y-z-w",
	}
	for in, want := range cases {
		if got := encodeProjectDir(in); got != want {
			t.Fatalf("encodeProjectDir(%q) = %q, want %q", in, got, want)
		}
	}
}

// A newer transcript in ANOTHER project must never be chosen, and when the
// run's own folder has nothing there is no fallback: not found, poll again.
// This is the bug that renamed five live sessions.
func TestDiscoverSessionIDNeverLeavesOwnProjectDir(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeCorpus(t, root, "-other-project", "new.jsonl", "SID-OTHER", now.Add(-1*time.Second))
	writeCorpus(t, root, "-tmp-opencode-mine", "old.jsonl", "SID-MINE", now.Add(-5*time.Second))

	sid, path := discoverSessionIDIn(root, "/tmp/opencode/mine", now.Add(-time.Hour), nil)
	if sid != "SID-MINE" {
		t.Fatalf("own-folder transcript not chosen: got %q", sid)
	}
	if filepath.Dir(path) != filepath.Join(root, "-tmp-opencode-mine") {
		t.Fatalf("path outside own folder: %q", path)
	}

	// nothing in our folder yet (it does not even exist): NOT the other project
	sid, path = discoverSessionIDIn(root, "/tmp/opencode/unborn", now.Add(-time.Hour), nil)
	if sid != "" || path != "" {
		t.Fatalf("fell back to a foreign transcript: sid=%q path=%q", sid, path)
	}
}

// "/x/webapp" and "/x/webapp/backend" encode to "-x-webapp" and "-x-webapp-backend".
// A substring match treated the second as the first; only an exact folder counts.
func TestDiscoverSessionIDSiblingFolderIsForeign(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeCorpus(t, root, "-x-webapp-backend", "a.jsonl", "SID-BACKEND", now.Add(-1*time.Second))
	if sid, _ := discoverSessionIDIn(root, "/x/webapp", now.Add(-time.Hour), nil); sid != "" {
		t.Fatalf("sibling folder mistaken for ours: got %q", sid)
	}
	writeCorpus(t, root, "-x-webapp", "b.jsonl", "SID-WEBAPP", now.Add(-3*time.Second))
	if sid, _ := discoverSessionIDIn(root, "/x/webapp", now.Add(-time.Hour), nil); sid != "SID-WEBAPP" {
		t.Fatalf("own folder not chosen over sibling: got %q", sid)
	}
}

// A transcript that already existed when the spawn started belongs to another
// session, however recently it was written. The "Flowing PRs" loop member
// renamed a live webapp session because newest-mtime treated that live file as
// its own; the spawn-time snapshot makes that impossible.
func TestDiscoveryScopeNeverBindsAPreexistingTranscript(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeCorpus(t, root, "-x-webapp", "live.jsonl", "SID-LIVE", now.Add(-10*time.Second))
	scope := newDiscoveryScope(root, "/x/webapp", "", true)
	started := now

	// the live session keeps typing: newest mtime in the folder
	writeCorpus(t, root, "-x-webapp", "live.jsonl", "SID-LIVE", now.Add(5*time.Second))
	if sid, _ := scope.find(started, nil); sid != "" {
		t.Fatalf("bound to a live pre-existing transcript: %q", sid)
	}

	// our own transcript appears; older than the live one's last write, still ours
	writeCorpus(t, root, "-x-webapp", "ours.jsonl", "SID-OURS", now.Add(1*time.Second))
	sid, path := scope.find(started, nil)
	if sid != "SID-OURS" || filepath.Base(path) != "ours.jsonl" {
		t.Fatalf("own transcript not chosen: sid=%q path=%q", sid, path)
	}
}

// On --resume the transcript is known by name: it predates the spawn, so the
// mtime rule would reject it, and a newer neighbour must not stand in for it.
func TestDiscoveryScopeResumeIsByName(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeCorpus(t, root, "-x-webapp", "SID-R.jsonl", "SID-R", now.Add(-time.Hour))
	writeCorpus(t, root, "-x-webapp", "other.jsonl", "SID-OTHER", now.Add(time.Second))

	scope := newDiscoveryScope(root, "/x/webapp", "SID-R", true)
	sid, path := scope.find(now, nil)
	if sid != "SID-R" || filepath.Base(path) != "SID-R.jsonl" {
		t.Fatalf("resume did not bind by name: sid=%q path=%q", sid, path)
	}

	// resuming a session whose transcript is not in this folder: nothing, never a substitute
	scope = newDiscoveryScope(root, "/x/webapp", "SID-MISSING", true)
	if sid, _ := scope.find(now, nil); sid != "" {
		t.Fatalf("resume substituted another transcript: %q", sid)
	}
}

// Only claude writes ~/.claude/projects; another engine's run must never be
// bound to a claude transcript that happens to appear in the same cwd.
func TestDiscoveryScopeOtherEngineFindsNothing(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	scope := newDiscoveryScope(root, "/x/webapp", "", false)
	writeCorpus(t, root, "-x-webapp", "new.jsonl", "SID-NEW", now.Add(time.Second))
	if sid, _ := scope.find(now, nil); sid != "" {
		t.Fatalf("non-claude engine bound to a claude transcript: %q", sid)
	}
}

func TestDiscoverSessionIDEmptyRootFindsNothing(t *testing.T) {
	if sid, _ := discoverSessionIDIn("", "/tmp/anything", time.Now().Add(-time.Hour), nil); sid != "" {
		t.Fatalf("empty corpus root must find nothing, got %q", sid)
	}
}

// The title writer is the second lock: even handed a foreign path it refuses.
func TestAppendCustomTitleRefusesForeignPath(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeCorpus(t, root, "-other-project", "live.jsonl", "SID-LIVE", now)
	foreign := filepath.Join(root, "-other-project", "live.jsonl")
	before, _ := os.ReadFile(foreign) //nolint:gosec // test fixture

	appendCustomTitle(foreign, filepath.Join(root, "-mine"), "SID-LIVE", "My named session")
	after, _ := os.ReadFile(foreign) //nolint:gosec // test fixture
	if string(after) != string(before) {
		t.Fatalf("foreign transcript was written to")
	}

	appendCustomTitle(foreign, filepath.Join(root, "-other-project"), "SID-LIVE", "Right folder")
	after, _ = os.ReadFile(foreign) //nolint:gosec // test fixture
	if string(after) == string(before) || !contains(after, `"customTitle":"Right folder"`) {
		t.Fatalf("own-folder write missing: %s", after)
	}
}

func contains(b []byte, s string) bool { return len(b) > 0 && strings.Contains(string(b), s) }

func TestPreTrustCwdBestEffort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// case 1: missing ~/.claude.json → seed with just the trust flags
	preTrustCwdBestEffort("/tmp/projA")
	b, err := os.ReadFile(filepath.Join(home, ".claude.json")) //nolint:gosec // test fixture
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	projects, _ := got["projects"].(map[string]any)
	if projects == nil {
		t.Fatalf("projects missing")
	}
	entry, _ := projects["/tmp/projA"].(map[string]any)
	if entry == nil || entry["hasTrustDialogAccepted"] != true {
		t.Fatalf("expected trust accepted, got %#v", entry)
	}

	// case 2: existing entry with stats — preserve the stats, flip the bits
	existing := `{"numStartups":42,"projects":{"/tmp/projA":{"allowedTools":[],"lastCost":1.5}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	preTrustCwdBestEffort("/tmp/projA")
	b, _ = os.ReadFile(filepath.Join(home, ".claude.json")) //nolint:gosec // test fixture
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["numStartups"] != float64(42) {
		t.Fatalf("expected numStartups preserved, got %v", got["numStartups"])
	}
	entry, _ = got["projects"].(map[string]any)["/tmp/projA"].(map[string]any)
	if entry == nil {
		t.Fatal("entry missing")
	}
	if entry["hasTrustDialogAccepted"] != true {
		t.Errorf("trust flag not set")
	}
	if entry["lastCost"] != 1.5 {
		t.Errorf("lastCost should be preserved, got %v", entry["lastCost"])
	}

	// case 3: corrupt json → swallow, don't crash
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	preTrustCwdBestEffort("/tmp/projA") // must not panic

	// case 4: empty cwd → no-op
	preTrustCwdBestEffort("")
}

// discoverSessionIDIn is find() without spawn-time knowledge: no pre-existing
// snapshot and no resume id. A test helper for the discovery contract tests.
func discoverSessionIDIn(root, cwd string, started time.Time, exclude map[string]bool) (string, string) {
	return discoveryScope{dir: projectDirFor(root, cwd), claude: true}.find(started, exclude)
}
