//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The daemon autostarts from the per-user Run key — no admin required (unlike a
// Scheduled Task registered via Register-ScheduledTask, which needs elevation to
// write the root task folder). It runs in the user's session, so child TUIs see
// the user's profile and Claude/OpenCode auth. It stops at logout.
const (
	winRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	winRunValue   = "agentflow"
)

type windowsRunKeyService struct {
	run runner
}

func newServiceManager() serviceManager { return &windowsRunKeyService{run: execRunner} }

func (*windowsRunKeyService) Supported() bool { return true }

// runCommandLine is the autostart command: `"<exe>" serve`.
func runCommandLine(exe string) string { return fmt.Sprintf("%q serve", exe) }

func (s *windowsRunKeyService) Install(ctx context.Context, spec serviceSpec) (bool, error) {
	desired := runCommandLine(spec.ExecPath)
	changed := true
	if cur, err := readRunValue(); err == nil {
		changed = !strings.EqualFold(cur, desired)
	}
	if changed {
		if err := writeRunValue(desired); err != nil {
			return false, fmt.Errorf(`set autostart (HKCU\...\Run): %w`, err)
		}
	}
	// Start the daemon now if it isn't already running. The single-instance lock
	// makes a redundant launch exit immediately, so this is safe.
	if !s.running(ctx) {
		if err := launchDetached(spec.ExecPath); err != nil {
			return changed, fmt.Errorf("launch agentflow serve: %w", err)
		}
	}
	return changed, nil
}

func (s *windowsRunKeyService) Stop(ctx context.Context) error {
	for _, pid := range s.daemonPIDs(ctx) {
		//nolint:gosec // pid came from tasklist for our own image.
		_, _ = s.run(ctx, "taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	}
	return nil
}

func (s *windowsRunKeyService) Restart(ctx context.Context) error {
	_ = s.Stop(ctx)
	exe, err := resolveExecutable()
	if err != nil {
		return err
	}
	return launchDetached(exe)
}

func (s *windowsRunKeyService) Remove(ctx context.Context) error {
	_ = s.Stop(ctx)
	return deleteRunValue()
}

func (s *windowsRunKeyService) Query(ctx context.Context) serviceState {
	st := serviceState{}
	if _, err := readRunValue(); err == nil {
		st.Installed = true
	}
	if pids := s.daemonPIDs(ctx); len(pids) > 0 {
		st.Running = true
		st.PID = pids[0]
	}
	return st
}

// ServicePATH returns "" — the daemon inherits the user's environment PATH,
// which is where npm/nvm put claude and opencode.
func (*windowsRunKeyService) ServicePATH() string { return "" }

func (*windowsRunKeyService) LogHint() string {
	return "agentflow autostarts from your user Run key; run `agentflow serve` in a terminal to watch live logs"
}

// StaysUpAfterLogout reports false: a per-user autostart stops at logout.
func (*windowsRunKeyService) StaysUpAfterLogout(context.Context) bool { return false }

func (s *windowsRunKeyService) running(ctx context.Context) bool {
	return len(s.daemonPIDs(ctx)) > 0
}

// daemonPIDs returns running agentflow.exe PIDs other than this process (which
// is itself agentflow.exe when a CLI command is running).
func (s *windowsRunKeyService) daemonPIDs(ctx context.Context) []int {
	out, err := s.run(ctx, "tasklist", "/FO", "CSV", "/NH", "/FI", "IMAGENAME eq agentflow.exe")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), `"agentflow.exe"`) {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.Trim(fields[1], `" `))
		if err != nil || pid == self {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// launchDetached starts `agentflow serve` as a background process with no
// console window, surviving the CLI's exit.
func launchDetached(exe string) error {
	//nolint:gosec // G204: our own binary.
	cmd := exec.Command(exe, "serve")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release() // the daemon runs independently
}

func readRunValue() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, winRunKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue(winRunValue)
	return v, err
}

func writeRunValue(cmdline string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, winRunKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	return k.SetStringValue(winRunValue, cmdline)
}

func deleteRunValue() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, winRunKeyPath, registry.SET_VALUE)
	if err != nil {
		return nil // key absent — nothing to remove
	}
	defer func() { _ = k.Close() }()
	if err := k.DeleteValue(winRunValue); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}
