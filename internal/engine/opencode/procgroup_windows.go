//go:build windows

package opencode

import (
	"os/exec"
	"strconv"
)

// setProcessGroup is a no-op on Windows: there are no POSIX process groups, and
// the child tree is torn down with taskkill /T below.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree kills the probe child and every process it spawned. `taskkill
// /T` walks the child tree; `opencode serve` spawns node children that would
// otherwise keep holding the port.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	//nolint:gosec // G204: PID is an int we just started.
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	_ = cmd.Process.Kill()
}
