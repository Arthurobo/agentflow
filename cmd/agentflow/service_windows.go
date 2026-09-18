//go:build windows

package main

import (
	"context"
	"fmt"
	"strings"
)

// winTaskName is the Scheduled Task that runs the daemon.
const winTaskName = "agentflow"

// windowsTaskService runs the daemon as a per-user Scheduled Task that starts at
// logon (schtasks). It needs no administrator rights and runs in the user's own
// session, so the child TUIs see the user's profile and Claude/OpenCode auth.
// The tradeoff versus a true Windows service is that it stops at logout
// (StaysUpAfterLogout reports false so `start` can print a hint).
type windowsTaskService struct {
	run runner
}

func newServiceManager() serviceManager { return &windowsTaskService{run: execRunner} }

func (*windowsTaskService) Supported() bool { return true }

// psSingleQuote wraps s as a PowerShell single-quoted literal (internal single
// quotes doubled), so a path is passed verbatim with no interpretation.
func psSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// createTask registers (or replaces) the logon task via PowerShell's
// Register-ScheduledTask. Unlike `schtasks /Create /TR "..."`, the program and
// its argument are separate parameters, so there is no fragile command-line
// quoting to get wrong. -AllowStartIfOnBatteries / -DontStopIfGoingOnBatteries
// matter on laptops, where tasks otherwise don't run on battery.
func (s *windowsTaskService) createTask(ctx context.Context, exe string) error {
	ps := "$ErrorActionPreference='Stop';" +
		"$a=New-ScheduledTaskAction -Execute " + psSingleQuote(exe) + " -Argument 'serve';" +
		"$t=New-ScheduledTaskTrigger -AtLogOn;" +
		"$s=New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -ExecutionTimeLimit ([TimeSpan]::Zero);" +
		"Register-ScheduledTask -TaskName '" + winTaskName + "' -Action $a -Trigger $t -Settings $s -Force | Out-Null"
	out, err := s.run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func (s *windowsTaskService) Install(ctx context.Context, spec serviceSpec) (bool, error) {
	prev, hadPrev := s.currentTaskCommand(ctx)
	changed := !hadPrev || !strings.Contains(prev, spec.ExecPath)
	if err := s.createTask(ctx, spec.ExecPath); err != nil {
		return false, fmt.Errorf("register scheduled task: %w", err)
	}
	// A freshly (re)registered logon task is not running yet: start it now so the
	// daemon (and the tunnel) come up without waiting for the next logon. If it
	// was already running and the binary path changed, restart onto the new one.
	st := s.Query(ctx)
	if !st.Running {
		if _, err := s.run(ctx, "schtasks", "/Run", "/TN", winTaskName); err != nil {
			return changed, fmt.Errorf("schtasks /Run: %w", err)
		}
	} else if changed {
		_ = s.Restart(ctx)
	}
	return changed, nil
}

func (s *windowsTaskService) Stop(ctx context.Context) error {
	_, err := s.run(ctx, "schtasks", "/End", "/TN", winTaskName)
	return err
}

func (s *windowsTaskService) Restart(ctx context.Context) error {
	_, _ = s.run(ctx, "schtasks", "/End", "/TN", winTaskName)
	_, err := s.run(ctx, "schtasks", "/Run", "/TN", winTaskName)
	return err
}

func (s *windowsTaskService) Remove(ctx context.Context) error {
	_, _ = s.run(ctx, "schtasks", "/End", "/TN", winTaskName)
	_, err := s.run(ctx, "schtasks", "/Delete", "/TN", winTaskName, "/F")
	return err
}

func (s *windowsTaskService) Query(ctx context.Context) serviceState {
	out, err := s.run(ctx, "schtasks", "/Query", "/TN", winTaskName, "/FO", "LIST", "/V")
	if err != nil {
		return serviceState{}
	}
	st := serviceState{Installed: true}
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Status":
			if strings.EqualFold(strings.TrimSpace(val), "Running") {
				st.Running = true
			}
		case "Scheduled Task State":
			if strings.EqualFold(strings.TrimSpace(val), "Enabled") {
				st.Enabled = true
			}
		}
	}
	return st
}

// currentTaskCommand returns the command the installed task runs, if any.
func (s *windowsTaskService) currentTaskCommand(ctx context.Context) (string, bool) {
	out, err := s.run(ctx, "schtasks", "/Query", "/TN", winTaskName, "/FO", "LIST", "/V")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "Task To Run" {
			return strings.TrimSpace(val), true
		}
	}
	return "", false
}

// ServicePATH returns "" — the task inherits the user's environment PATH at
// logon, which is where npm/nvm put claude and opencode.
func (*windowsTaskService) ServicePATH() string { return "" }

func (*windowsTaskService) LogHint() string {
	return "the \"agentflow\" task runs `agentflow serve` at logon (see Task Scheduler / taskschd.msc); run `agentflow serve` in a terminal to watch live logs"
}

// StaysUpAfterLogout reports false: a per-user logon task stops when the user
// logs out.
func (*windowsTaskService) StaysUpAfterLogout(context.Context) bool { return false }
