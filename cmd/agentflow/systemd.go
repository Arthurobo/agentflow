package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// systemdUnitName is both the unit file's name and the systemctl identifier.
const systemdUnitName = "agentflow.service"

// systemdService drives a systemd user unit.
type systemdService struct {
	home string
	run  runner
}

func (s *systemdService) Supported() bool { return true }

func (s *systemdService) unitPath() string {
	return filepath.Join(s.home, ".config", "systemd", "user", systemdUnitName)
}

func (s *systemdService) systemctl(ctx context.Context, args ...string) (string, error) {
	out, err := s.run(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if err != nil {
		return out, fmt.Errorf("systemctl --user %s: %w (%s)", strings.Join(args, " "), err, trimOut(out))
	}
	return out, nil
}

// systemdQuote quotes s for a unit file value: backslashes and double quotes
// are escaped for systemd's C-style unquoting, and % is doubled so it is not
// read as a specifier. $ is left alone by the caller where it is literal.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	return `"` + r.Replace(s) + `"`
}

// renderSystemdUnit builds the user unit.
func renderSystemdUnit(spec serviceSpec) string {
	// ExecStart expands $VAR, so a literal $ in the path is doubled.
	execPath := systemdQuote(strings.ReplaceAll(spec.ExecPath, "$", "$$"))
	return `[Unit]
Description=agentflow (coding-agent session daemon)
After=default.target

[Service]
Type=simple
# Settings live in ~/.config/agentflow/agentflow.env, which agentflow reads
# itself when it starts. This unit is world-readable; that file is not.
# The PATH captured when ` + "`agentflow start`" + ` ran, so the claude and opencode
# found in your shell are found by the service too. Run agentflow start again
# after installing an engine somewhere new.
Environment=` + systemdQuote("PATH="+spec.PATH) + `
ExecStart=` + execPath + ` serve
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
}

func (s *systemdService) Install(ctx context.Context, spec serviceSpec) (bool, error) {
	unit := renderSystemdUnit(spec)
	path := s.unitPath()
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	changed := string(old) != unit
	if !changed {
		_, err := s.systemctl(ctx, "enable", "--now", systemdUnitName)
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, fmt.Errorf("create the user unit directory: %w", err)
	}
	if err := writeFileAtomic(path, []byte(unit), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return true, err
	}
	if _, err := s.systemctl(ctx, "enable", systemdUnitName); err != nil {
		return true, err
	}
	// restart, not start: a service already running the old definition
	// (old binary path, old PATH) must pick up the new one.
	_, err = s.systemctl(ctx, "restart", systemdUnitName)
	return true, err
}

func (s *systemdService) Stop(ctx context.Context) error {
	_, err := s.systemctl(ctx, "disable", "--now", systemdUnitName)
	return err
}

func (s *systemdService) Restart(ctx context.Context) error {
	_, err := s.systemctl(ctx, "restart", systemdUnitName)
	return err
}

func (s *systemdService) Remove(ctx context.Context) error {
	path := s.unitPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	if _, err := s.systemctl(ctx, "disable", "--now", systemdUnitName); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, err := s.systemctl(ctx, "daemon-reload")
	return err
}

func (s *systemdService) Query(ctx context.Context) serviceState {
	var st serviceState
	if _, err := os.Stat(s.unitPath()); err == nil {
		st.Installed = true
	}
	if out, err := s.run(ctx, "systemctl", "--user", "is-enabled", systemdUnitName); err == nil {
		st.Enabled = trimOut(out) == "enabled"
	}
	if out, err := s.run(ctx, "systemctl", "--user", "is-active", systemdUnitName); err == nil {
		st.Running = trimOut(out) == "active"
	}
	if st.Running {
		if out, err := s.run(ctx, "systemctl", "--user", "show", "--property=MainPID", "--value", systemdUnitName); err == nil {
			if pid, perr := strconv.Atoi(trimOut(out)); perr == nil && pid > 0 {
				st.PID = pid
			}
		}
	}
	return st
}

// ServicePATH reads the PATH back out of the installed unit.
func (s *systemdService) ServicePATH() string {
	data, err := os.ReadFile(s.unitPath())
	if err != nil {
		return ""
	}
	return unitPATH(string(data))
}

// unitPATH extracts PATH from an Environment= line written by
// renderSystemdUnit (or a hand-written unquoted one).
func unitPATH(unit string) string {
	for _, line := range strings.Split(unit, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "Environment=")
		if !ok {
			continue
		}
		if strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) && len(v) >= 2 {
			v = strings.NewReplacer(`\\`, `\`, `\"`, `"`, "%%", "%").Replace(v[1 : len(v)-1])
		}
		if p, ok := strings.CutPrefix(v, "PATH="); ok {
			return p
		}
	}
	return ""
}

func (s *systemdService) LogHint() string { return "journalctl --user -u agentflow" }

// StaysUpAfterLogout reports whether linger is on. Without it a user service
// stops when the user's last session ends, which is not what anyone
// installing a background daemon expects. Uncertainty returns false so the
// caller nudges rather than stays quiet.
func (s *systemdService) StaysUpAfterLogout(ctx context.Context) bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	out, err := s.run(ctx, "loginctl", "show-user", u.Username, "--property=Linger", "--value")
	if err != nil {
		return false
	}
	return trimOut(out) == "yes"
}
