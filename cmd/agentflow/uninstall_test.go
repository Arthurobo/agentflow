package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/adminsock"
	"github.com/arthurobo/agentflow/internal/remote"
)

// uninstallFixture lays out a home directory where AF_DB=$HOME/agentflow.db,
// so the data directory is the home directory itself.
type uninstallFixture struct {
	home, dataDir string
	u             *uninstaller
	svc           *fakeService
	out           *bytes.Buffer
}

func newUninstallFixture(t *testing.T, answer string) *uninstallFixture {
	t.Helper()
	home := shortDataDir(t)
	cfg := testConfig(home)
	cfg.dbPath = filepath.Join(home, "agentflow.db")
	cfg.dataDir = home

	write := func(rel, content string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// agentflow's files
	for _, rel := range []string{
		"agentflow.db", "agentflow.db-wal", "agentflow.db-shm", "agentflow.db.lock",
		"hook.secret", "remote.json", "run/agentflow.sock", "tailscale-serve.json",
		"defaults/plays.yml", "defaults/rules.yml",
		"tailscale/hostname", "tailscale/tailscaled.state",
		".config/agentflow/agentflow.env",
		".local/share/agentflow/uploads/proj/sess/shot.png",
		".local/bin/agentflow",
	} {
		write(rel, "x")
	}
	// the person's own files, which must all survive
	for _, rel := range []string{
		"notes.txt", "uploads/holiday.png", "run/my-script.sh",
		"defaults/mine.conf", ".config/agentflow-other/x", ".local/bin/other-tool",
	} {
		write(rel, "keep")
	}

	svc := &fakeService{state: serviceState{Installed: true, Running: true}}
	out := &bytes.Buffer{}
	u := &uninstaller{
		in:          strings.NewReader(answer),
		out:         out,
		cfg:         cfg,
		home:        home,
		exe:         filepath.Join(home, ".local", "bin", "agentflow"),
		svc:         svc,
		admin:       adminsock.NewClient(adminsock.SocketPath(home)),
		uploadsRoot: filepath.Join(home, ".local", "share", "agentflow", "uploads"),
		envFile:     filepath.Join(home, ".config", "agentflow", "agentflow.env"),
	}
	return &uninstallFixture{home: home, dataDir: home, u: u, svc: svc, out: out}
}

func (f *uninstallFixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(f.home, rel))
	return err == nil
}

func TestUninstallDeclinedTouchesNothing(t *testing.T) {
	f := newUninstallFixture(t, "n\n")
	if err := f.u.run(context.Background(), true, false); err != errAborted {
		t.Fatalf("err = %v", err)
	}
	if !f.exists(".local/bin/agentflow") || !f.exists("agentflow.db") || f.svc.removed != 0 {
		t.Fatalf("declined uninstall removed something (service removed %d)", f.svc.removed)
	}
	if !strings.Contains(f.out.String(), "Continue? [y/N]") {
		t.Fatalf("no prompt:\n%s", f.out.String())
	}
}

func TestUninstallWithoutPurgeKeepsData(t *testing.T) {
	f := newUninstallFixture(t, "y\n")
	if err := f.u.run(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if f.exists(".local/bin/agentflow") || f.svc.removed != 1 {
		t.Fatal("binary or service not removed")
	}
	for _, rel := range []string{"agentflow.db", "hook.secret", "tailscale/hostname", ".config/agentflow/agentflow.env"} {
		if !f.exists(rel) {
			t.Fatalf("%s deleted without --purge", rel)
		}
	}
}

func TestPurgeDeletesOnlyKnownPathsWhenDataDirIsHome(t *testing.T) {
	f := newUninstallFixture(t, "")
	if err := f.u.run(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"agentflow.db", "agentflow.db-wal", "agentflow.db-shm", "agentflow.db.lock",
		"hook.secret", "remote.json", "run/agentflow.sock", "tailscale-serve.json", "defaults/plays.yml", "defaults/rules.yml",
		"tailscale", ".config/agentflow", ".local/share/agentflow/uploads", ".local/bin/agentflow",
	} {
		if f.exists(rel) {
			t.Errorf("%s survived --purge", rel)
		}
	}
	for _, rel := range []string{
		"notes.txt", "uploads/holiday.png", "run/my-script.sh", "defaults/mine.conf",
		".config/agentflow-other/x", ".local/bin/other-tool",
	} {
		if !f.exists(rel) {
			t.Errorf("%s was deleted", rel)
		}
	}
	if !f.exists("") {
		t.Fatal("home directory removed")
	}
	if !strings.Contains(f.out.String(), "~/.claude.json") || !strings.Contains(f.out.String(), ".agentflow-bak-") {
		t.Fatalf("leftover note missing:\n%s", f.out.String())
	}
}

func TestPurgeKeepsAPersonalTailscaleFolder(t *testing.T) {
	f := newUninstallFixture(t, "")
	for _, marker := range []string{"hostname", "tailscaled.state"} {
		_ = os.Remove(filepath.Join(f.home, "tailscale", marker))
	}
	if err := os.WriteFile(filepath.Join(f.home, "tailscale", "my-acls.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.u.run(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
	if !f.exists("tailscale/my-acls.json") {
		t.Fatal("a tailscale folder without agentflow's state was deleted")
	}
}

func TestUninstallLeavesBinaryOutsideLocalBin(t *testing.T) {
	f := newUninstallFixture(t, "")
	elsewhere := filepath.Join(f.home, "go", "bin", "agentflow")
	if err := os.MkdirAll(filepath.Dir(elsewhere), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(elsewhere, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.u.exe = elsewhere
	if err := f.u.run(context.Background(), false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatal("binary outside ~/.local/bin was deleted")
	}

	f.u.exe = filepath.Join(f.home, ".local", "bin", "agentflow")
	f.u.home = ""
	if f.u.binaryRemovable() {
		t.Fatal("binary removable with an empty HOME")
	}
	f.u.home = f.home
	f.u.exe = filepath.Join(f.home, ".local", "bin", "sub", "agentflow")
	if f.u.binaryRemovable() {
		t.Fatal("binary in a subdirectory of ~/.local/bin treated as ours")
	}
}

func TestSafeToRemoveTree(t *testing.T) {
	cases := []struct {
		path, home string
		want       bool
	}{
		{"/home/u/.local/share/agentflow/uploads", "/home/u", true},
		{"/home/u", "/home/u", false},
		{"/home", "/home/u", false},
		{"/", "/home/u", false},
		{"relative/uploads", "/home/u", false},
		{"/srv/uploads", "", false},
		{"/srv/uploads", "/home/u", true},
	}
	for _, c := range cases {
		if got := safeToRemoveTree(c.path, c.home); got != c.want {
			t.Errorf("safeToRemoveTree(%q, %q) = %v", c.path, c.home, got)
		}
	}
}

// --purge logs out only agentflow's own Tailscale node. The computer's own
// Tailscale is left alone, and doesn't produce a "could not log out" warning.
func TestPurgeLogsOutOnlyTheEmbeddedNode(t *testing.T) {
	cases := []struct {
		name    string
		status  remote.Status
		logouts int
	}{
		{"embedded node", remote.Status{State: remote.StateRunning, Transport: remote.TransportTailscale, Backend: remote.BackendEmbedded}, 1},
		{"system Tailscale", remote.Status{State: remote.StateRunning, Transport: remote.TransportTailscale, Backend: remote.BackendSystem}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newUninstallFixture(t, "")
			// The fixture leaves a plain file where the socket goes.
			if err := os.Remove(adminsock.SocketPath(f.dataDir)); err != nil {
				t.Fatal(err)
			}
			d := startFakeDaemon(t, f.dataDir, c.status)
			if err := f.u.run(context.Background(), true, true); err != nil {
				t.Fatal(err)
			}
			if got := d.remote.Logouts(); got != c.logouts {
				t.Fatalf("logouts = %d, want %d\n%s", got, c.logouts, f.out.String())
			}
			if c.logouts == 0 && strings.Contains(f.out.String(), "log out of Tailscale") {
				t.Fatalf("warned about a logout that wasn't agentflow's to do:\n%s", f.out.String())
			}
		})
	}
}
