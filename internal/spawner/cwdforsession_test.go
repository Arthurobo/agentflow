package spawner

import (
	"os"
	"path/filepath"
	"testing"
)

// A resume must find the session's real cwd from its transcript, so it opens in
// the right project folder and binds instead of defaulting to agentd's cwd.
func TestCwdForSessionReadsTranscriptCwd(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-home-user-code-myproj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "abc12345-0000-0000-0000-000000000001"
	// first lines have no cwd (custom-title/mode), then a user line with cwd
	body := `{"type":"custom-title","sessionId":"` + sid + `"}` + "\n" +
		`{"type":"mode","sessionId":"` + sid + `"}` + "\n" +
		`{"type":"user","cwd":"/home/user/code/myproj","sessionId":"` + sid + `","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, sid+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Spawner{}
	s.SetCorpusRoot(root)
	if got := s.CwdForSession(sid); got != "/home/user/code/myproj" {
		t.Fatalf("CwdForSession = %q, want /home/user/code/myproj", got)
	}
	if got := s.CwdForSession("nope-0000"); got != "" {
		t.Fatalf("unknown session must yield empty cwd, got %q", got)
	}
}
