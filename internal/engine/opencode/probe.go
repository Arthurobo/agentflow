package opencode

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
)

// ProbeModels asks a throwaway `opencode serve` which models this install can
// actually run, and returns the PROJECTION of that answer.
//
// It exists because the set is a property of the install rather than of
// OpenCode: which providers are authenticated and what the config enables.
// Verified on this machine, opencode 1.18.29: a serve answers GET /api/model
// with 66 entries across two providers once it has finished coming up. A
// static list in the registry would therefore be a guess, which is why
// store/tools.go still reports opencode as unprobed.
//
// The catalogue is NOT ready when health is: it fills about three seconds
// later. An earlier reading of this, that the answer depended on the working
// directory, was wrong; a cold serve returns none in any directory and then
// fills. Hence WaitModels below rather than a single ask.
//
// The process is short-lived and loopback-only, started in its own process
// group so the whole group can be killed rather than just the parent, and
// bounded by the caller's context. Nothing about it is left running.
func ProbeModels(ctx context.Context, binary, cwd string, allocPort func() (int, error)) ([]engine.Model, error) {
	if binary == "" {
		binary = "opencode"
	}
	if allocPort == nil {
		allocPort = ephemeralPort
	}
	port, err := allocPort()
	if err != nil {
		return nil, fmt.Errorf("opencode: probe: allocate port: %w", err)
	}
	if err := ValidatePort(port); err != nil {
		return nil, err
	}
	//nolint:gosec // G204: the configured opencode binary, with a built argv.
	cmd := exec.Command(binary, "serve", "--port", fmt.Sprint(port), "--hostname", "127.0.0.1")
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("opencode: probe: start serve: %w", err)
	}
	defer func() {
		// Kill the GROUP: serve is a node process that spawns children, and
		// killing only the parent leaves a listener holding the port.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	c := NewControl(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err := c.WaitReady(ctx, 15*time.Second); err != nil {
		return nil, fmt.Errorf("opencode: probe: %w", err)
	}
	// WaitModels, not Models: serve answers health several seconds before its
	// catalogue is populated, so asking once here returned an empty list with
	// no error and the picker showed no models at all.
	return c.WaitModels(ctx, 12*time.Second)
}
