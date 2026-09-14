package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		line, k, v string
		ok         bool
	}{
		{"AF_REMOTE=off", "AF_REMOTE", "off", true},
		{"  AF_ADDR = 127.0.0.1:1  ", "AF_ADDR", "127.0.0.1:1", true},
		{`AF_DB="/a b/c.db"`, "AF_DB", "/a b/c.db", true},
		{`AF_DB='x'`, "AF_DB", "x", true},
		{"export AF_TS_LOGS=on", "AF_TS_LOGS", "on", true},
		{"# AF_REMOTE=tailscale", "", "", false},
		{"", "", "", false},
		{"no equals", "", "", false},
		{"BAD KEY=1", "", "", false},
	}
	for _, c := range cases {
		k, v, ok := parseEnvLine(c.line)
		if k != c.k || v != c.v || ok != c.ok {
			t.Errorf("%q: got (%q,%q,%v)", c.line, k, v, ok)
		}
	}
}

func TestExpandHome(t *testing.T) {
	cases := map[string]string{
		"$HOME/.local/share/agentflow/agentflow.db": "/home/u/.local/share/agentflow/agentflow.db",
		"${HOME}/x":    "/home/u/x",
		"$HOMEDIR/x":   "$HOMEDIR/x",
		"a:$HOME:b":    "a:/home/u:b",
		"no vars":      "no vars",
		"$HOME$HOME":   "/home/u/home/u",
		"${HOME}$HOME": "/home/u/home/u",
	}
	for in, want := range cases {
		if got := expandHome(in, "/home/u"); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadEnvFileExpandsHomeAndNeverOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AF_ADDR", "127.0.0.1:9999")
	t.Setenv("AF_DATA_DIR", "$HOME/from-service-manager")
	for _, k := range []string{"AF_DB", "AF_UPLOADS_DIR"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	path := filepath.Join(t.TempDir(), "agentflow.env")
	content := "AF_ADDR=127.0.0.1:1\nAF_DB=$HOME/data/af.db\nAF_UPLOADS_DIR=\"${HOME}/up\"\n# AF_REMOTE=off\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loadEnvFile(path)
	if got := os.Getenv("AF_ADDR"); got != "127.0.0.1:9999" {
		t.Fatalf("file overrode the environment: AF_ADDR=%q", got)
	}
	if got := os.Getenv("AF_DB"); got != home+"/data/af.db" {
		t.Fatalf("AF_DB=%q", got)
	}
	if got := os.Getenv("AF_UPLOADS_DIR"); got != home+"/up" {
		t.Fatalf("AF_UPLOADS_DIR=%q", got)
	}
	if got := os.Getenv("AF_DATA_DIR"); got != home+"/from-service-manager" {
		t.Fatalf("inherited $HOME not expanded: %q", got)
	}
}

func TestWriteEnvValueReplacesAtomicallyAndKeepsComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agentflow.env")
	content := "# my notes\nAF_ADDR=127.0.0.1:5\nAF_REMOTE=tailscale\n# AF_REMOTE=off\nAF_REMOTE=tailscale\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeEnvValue(path, "AF_REMOTE", "off"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "# my notes\nAF_ADDR=127.0.0.1:5\nAF_REMOTE=off\n# AF_REMOTE=off\n"
	if string(got) != want {
		t.Fatalf("file =\n%s\nwant\n%s", got, want)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}

	if err := writeEnvValue(path, "AF_TS_LOGS", "on"); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if !strings.HasSuffix(string(got), "AF_TS_LOGS=on\n") {
		t.Fatalf("append failed: %s", got)
	}
	if err := writeEnvValue(path, "AF_REMOTE", "a\nAF_ADDR=0.0.0.0:1"); err == nil {
		t.Fatal("multi-line value accepted")
	}
}

func TestWriteEnvValueCreatesFileFromDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "agentflow.env")
	if err := writeEnvValue(path, "AF_REMOTE", "off"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(got), "# agentflow settings.") || !strings.HasSuffix(string(got), "AF_REMOTE=off\n") {
		t.Fatalf("file = %s", got)
	}
}

func TestEnsureEnvFileTightensExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentflow.env")
	if err := os.WriteFile(path, []byte("AF_REMOTE=off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureEnvFile(path); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	got, _ := os.ReadFile(path)
	if fi.Mode().Perm() != 0o600 || string(got) != "AF_REMOTE=off\n" {
		t.Fatalf("mode %v content %q", fi.Mode().Perm(), got)
	}
}

// Every AF_ variable the code reads is documented in the default env file,
// with the default the code actually uses for the remote settings.
func TestDefaultEnvFileListsEveryVariable(t *testing.T) {
	re := regexp.MustCompile(`"(AF_[A-Z0-9_]+)"`)
	used := map[string]bool{}
	for _, root := range []string{"../../internal", "."} {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				used[m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var missing []string
	for v := range used {
		if !strings.Contains(defaultEnvFile, "# "+v+"=") {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("variables missing from defaultEnvFile: %v", missing)
	}
	for _, line := range []string{"# AF_REMOTE=tailscale\n", "# AF_REMOTE_MODE=funnel\n", "# AF_TS_LOGS=off\n", "# AF_ADDR=127.0.0.1:4344\n"} {
		if !strings.Contains(defaultEnvFile, line) {
			t.Errorf("defaultEnvFile lacks %q", line)
		}
	}
}

func TestEnvExampleMatchesDefaultEnvFile(t *testing.T) {
	got, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != defaultEnvFile {
		t.Fatal(".env.example differs from defaultEnvFile; regenerate it from envfile.go")
	}
}
