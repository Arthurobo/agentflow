//go:build unix

package opencode

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the probe child in its own process group so the whole
// tree can be killed at once. `opencode serve` is a node process that spawns
// children; killing only the parent leaves a listener holding the port.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree kills the child and its process group (negative PID).
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
