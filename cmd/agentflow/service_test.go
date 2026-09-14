package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordRunner records service-manager calls. fail maps a joined command
// line prefix to an error.
type recordRunner struct {
	calls []string
	fail  map[string]error
	out   map[string]string
}

func (r *recordRunner) run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, line)
	for prefix, err := range r.fail {
		if strings.HasPrefix(line, prefix) {
			return "", err
		}
	}
	return r.out[line], nil
}

const goldenUnit = `[Unit]
Description=agentflow (coding-agent session daemon)
After=default.target

[Service]
Type=simple
# Settings live in ~/.config/agentflow/agentflow.env, which agentflow reads
# itself when it starts. This unit is world-readable; that file is not.
# The PATH captured when ` + "`agentflow start`" + ` ran, so the claude and opencode
# found in your shell are found by the service too. Run agentflow start again
# after installing an engine somewhere new.
Environment="PATH=/home/u/.nvm/versions/node/v22/bin:/usr/bin"
ExecStart="/home/u/.local/bin/agentflow" serve
# On stop, agentflow asks every agent it runs to exit (up to 10 s) before it
# exits itself. mixed sends SIGTERM to agentflow only, then SIGKILL to anything
# left in the group once TimeoutStopSec runs out.
KillMode=mixed
TimeoutStopSec=20
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
`

func TestRenderSystemdUnitGolden(t *testing.T) {
	got := renderSystemdUnit(serviceSpec{ExecPath: "/home/u/.local/bin/agentflow", PATH: "/home/u/.nvm/versions/node/v22/bin:/usr/bin"})
	if got != goldenUnit {
		t.Fatalf("unit mismatch:\n%s", got)
	}
}

func TestRenderSystemdUnitEscapes(t *testing.T) {
	unit := renderSystemdUnit(serviceSpec{ExecPath: `/opt/a b/$x%y/agentflow`, PATH: `/p%1:/q"r:/s\t`})
	if !strings.Contains(unit, `Environment="PATH=/p%%1:/q\"r:/s\\t"`) {
		t.Fatalf("PATH not escaped:\n%s", unit)
	}
	if !strings.Contains(unit, `ExecStart="/opt/a b/$$x%%y/agentflow" serve`) {
		t.Fatalf("ExecStart not escaped:\n%s", unit)
	}
	if got := unitPATH(unit); got != `/p%1:/q"r:/s\t` {
		t.Fatalf("unitPATH round trip = %q", got)
	}
	if got := unitPATH("[Service]\nEnvironment=PATH=/usr/bin:/bin\n"); got != "/usr/bin:/bin" {
		t.Fatalf("unquoted PATH = %q", got)
	}
}

func TestSystemdInstallRestartsOnlyWhenUnitChanged(t *testing.T) {
	home := t.TempDir()
	rr := &recordRunner{}
	s := &systemdService{home: home, run: rr.run}
	spec := serviceSpec{ExecPath: "/home/u/.local/bin/agentflow", PATH: "/usr/bin"}

	changed, err := s.Install(context.Background(), spec)
	if err != nil || !changed {
		t.Fatalf("first install: changed=%v err=%v", changed, err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable agentflow.service",
		"systemctl --user restart agentflow.service",
	}
	if strings.Join(rr.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v", rr.calls)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "agentflow.service"))
	if err != nil || string(data) != renderSystemdUnit(spec) {
		t.Fatalf("unit file not written: %v", err)
	}
	if s.ServicePATH() != "/usr/bin" {
		t.Fatalf("ServicePATH = %q", s.ServicePATH())
	}

	rr.calls = nil
	changed, err = s.Install(context.Background(), spec)
	if err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}
	if strings.Join(rr.calls, "|") != "systemctl --user enable --now agentflow.service" {
		t.Fatalf("unchanged calls = %v", rr.calls)
	}

	rr.calls = nil
	spec.PATH = "/usr/local/bin:/usr/bin"
	if changed, _ := s.Install(context.Background(), spec); !changed {
		t.Fatal("PATH change not detected")
	}
	if rr.calls[len(rr.calls)-1] != "systemctl --user restart agentflow.service" {
		t.Fatalf("changed unit not restarted: %v", rr.calls)
	}
}

func TestSystemdInstallReportsSystemctlFailure(t *testing.T) {
	rr := &recordRunner{fail: map[string]error{"systemctl --user daemon-reload": errors.New("no user bus")}}
	s := &systemdService{home: t.TempDir(), run: rr.run}
	if _, err := s.Install(context.Background(), serviceSpec{ExecPath: "/x", PATH: "/y"}); err == nil || !strings.Contains(err.Error(), "no user bus") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderLaunchdPlist(t *testing.T) {
	plist := renderLaunchdPlist(serviceSpec{ExecPath: "/Users/u/.local/bin/agentflow", PATH: "/opt/homebrew/bin:/usr/bin&<x>"}, "/Users/u")
	for _, want := range []string{
		"<string>com.arthurobo.agentflow</string>",
		"<string>/Users/u/.local/bin/agentflow</string>\n\t\t<string>serve</string>",
		"<key>PATH</key>\n\t\t<string>/opt/homebrew/bin:/usr/bin&amp;&lt;x&gt;</string>",
		"<key>WorkingDirectory</key>\n\t<string>/Users/u</string>",
		"<string>/Users/u/Library/Logs/agentflow/agentflow.log</string>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<key>RunAtLoad</key>\n\t<true/>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if strings.Contains(plist, "$HOME") || strings.Contains(plist, "AF_ENV_FILE") {
		t.Fatalf("plist has unexpanded or unused values:\n%s", plist)
	}
	if got := plistPATH([]byte(plist)); got != "/opt/homebrew/bin:/usr/bin&<x>" {
		t.Fatalf("plistPATH = %q", got)
	}
}

func TestLaunchdInstallBootstrapsOrReloads(t *testing.T) {
	home := t.TempDir()
	rr := &recordRunner{fail: map[string]error{"launchctl print": errors.New("not loaded")}}
	s := &launchdService{home: home, uid: "501", run: rr.run}
	spec := serviceSpec{ExecPath: "/Users/u/.local/bin/agentflow", PATH: "/usr/bin"}

	if changed, err := s.Install(context.Background(), spec); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	want := "launchctl enable gui/501/com.arthurobo.agentflow|launchctl print gui/501/com.arthurobo.agentflow|launchctl bootstrap gui/501 " +
		filepath.Join(home, "Library", "LaunchAgents", "com.arthurobo.agentflow.plist")
	if strings.Join(rr.calls, "|") != want {
		t.Fatalf("calls = %v", rr.calls)
	}
	if fi, err := os.Stat(filepath.Join(home, "Library", "Logs", "agentflow")); err != nil || !fi.IsDir() {
		t.Fatal("log directory not created")
	}

	// Loaded and unchanged: just make sure it runs.
	rr.calls, rr.fail = nil, nil
	if changed, err := s.Install(context.Background(), spec); err != nil || changed {
		t.Fatalf("reinstall: %v %v", changed, err)
	}
	if rr.calls[len(rr.calls)-1] != "launchctl kickstart gui/501/com.arthurobo.agentflow" {
		t.Fatalf("calls = %v", rr.calls)
	}

	// Loaded and changed: reload the definition.
	rr.calls = nil
	spec.PATH = "/opt/homebrew/bin:/usr/bin"
	if changed, err := s.Install(context.Background(), spec); err != nil || !changed {
		t.Fatalf("changed install: %v %v", changed, err)
	}
	joined := strings.Join(rr.calls, "|")
	if !strings.Contains(joined, "launchctl bootout gui/501/com.arthurobo.agentflow|launchctl bootstrap gui/501 ") {
		t.Fatalf("calls = %v", rr.calls)
	}
	if s.ServicePATH() != "/opt/homebrew/bin:/usr/bin" {
		t.Fatalf("ServicePATH = %q", s.ServicePATH())
	}
}

func TestParseLaunchctlPrint(t *testing.T) {
	out := "gui/501/com.arthurobo.agentflow = {\n\tactive count = 1\n\tstate = running\n\tpid = 4242\n}\n"
	if running, pid := parseLaunchctlPrint(out); !running || pid != 4242 {
		t.Fatalf("running=%v pid=%d", running, pid)
	}
	if running, _ := parseLaunchctlPrint("state = not running\n"); running {
		t.Fatal("not running parsed as running")
	}
}

func TestLookPathIn(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "claude"), []byte("x"), 0o644); err != nil { // not executable
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := lookPathIn("claude", a+string(os.PathListSeparator)+b); got != filepath.Join(b, "claude") {
		t.Fatalf("got %q", got)
	}
	if got := lookPathIn("opencode", a+string(os.PathListSeparator)+b); got != "" {
		t.Fatalf("got %q", got)
	}
}
