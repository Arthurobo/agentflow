package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// runner runs an external command and returns its combined output. Every
// systemctl / launchctl / loginctl call goes through one, so tests can record
// the calls instead of touching the real service manager.
type runner func(ctx context.Context, name string, args ...string) (string, error)

func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // fixed service-manager binaries with our own arguments
	return string(out), err
}

// serviceState is what `agentflow status` reports about the service.
type serviceState struct {
	Installed bool
	Enabled   bool
	Running   bool
	PID       int
}

// serviceSpec is what gets installed: the binary to run and the PATH the
// service should see.
type serviceSpec struct {
	ExecPath string
	PATH     string
}

// serviceManager installs and controls the background service.
type serviceManager interface {
	// Supported reports whether this platform has a service manager
	// agentflow knows how to drive.
	Supported() bool
	// Install writes the service definition and makes sure the service is
	// running the current definition: started if it was stopped, restarted
	// if the definition changed. It reports whether the definition changed.
	Install(ctx context.Context, spec serviceSpec) (changed bool, err error)
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	Remove(ctx context.Context) error
	Query(ctx context.Context) serviceState
	// ServicePATH returns the PATH recorded in the installed definition, or
	// "" when nothing is installed.
	ServicePATH() string
	// LogHint tells a person where to look when the service misbehaves.
	LogHint() string
	// StaysUpAfterLogout reports whether the service keeps running when the
	// user logs out (false means a hint is worth printing).
	StaysUpAfterLogout(ctx context.Context) bool
}

var errNoServiceManager = errors.New("background service management is implemented for systemd (Linux) and launchd (macOS) only; run `agentflow serve` in the foreground, or supervise it with your platform's service manager")

// unsupportedService is the service manager for platforms without one.
type unsupportedService struct{}

func (unsupportedService) Supported() bool { return false }
func (unsupportedService) Install(context.Context, serviceSpec) (bool, error) {
	return false, errNoServiceManager
}
func (unsupportedService) Stop(context.Context) error              { return errNoServiceManager }
func (unsupportedService) Restart(context.Context) error           { return errNoServiceManager }
func (unsupportedService) Remove(context.Context) error            { return errNoServiceManager }
func (unsupportedService) Query(context.Context) serviceState      { return serviceState{} }
func (unsupportedService) ServicePATH() string                     { return "" }
func (unsupportedService) LogHint() string                         { return "" }
func (unsupportedService) StaysUpAfterLogout(context.Context) bool { return true }

// resolveExecutable returns the absolute, symlink-free path of the running
// binary. The service runs exactly this file, so a symlink swapped later (or
// a PATH lookup at boot) can't silently run something else.
func resolveExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("executable path %q is not absolute", resolved)
	}
	return resolved, nil
}

// currentPATH is the PATH to hand the service: the one `agentflow start` runs
// with, so claude and opencode installed through nvm, ~/.local/bin or Homebrew
// are found by the service exactly as they are in the shell.
func currentPATH() string {
	if p := os.Getenv("PATH"); p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		return "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
	}
	return "/usr/local/bin:/usr/bin:/bin"
}

// lookPathIn finds an executable named bin in a PATH-style list.
func lookPathIn(bin, pathList string) string {
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, bin)
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		return p
	}
	return ""
}

// servicePATHOrCurrent is the PATH engines will be looked up in by the
// service: the installed definition's, or this shell's when none is installed.
func servicePATHOrCurrent(svc serviceManager) (path, source string) {
	if p := svc.ServicePATH(); p != "" {
		return p, "service PATH"
	}
	return currentPATH(), "current PATH"
}

func trimOut(out string) string { return strings.TrimSpace(out) }
