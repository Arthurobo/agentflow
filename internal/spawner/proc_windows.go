//go:build windows

package spawner

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// childSysProcAttr — Windows has no process groups; the child tree is torn down
// with taskkill /T instead (see killTree). There is no parent-death signal
// either; a restarted agentd's reaper covers a hard crash.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// ReadProcIdentity records no identity on Windows (no /proc analog is wired up),
// so a run from a previous agentd life can never be proven to still be the
// process at its pid; the reaper marks it gone without signalling rather than
// risk killing whatever reused the pid. Live children are unaffected — they are
// torn down through the handle agentd holds.
func ReadProcIdentity(int) (pgid int, startTicks int64) { return 0, 0 }

// ProcessAlive reports whether pid is still running.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}

// waitExited cannot observe an unreaped exit here, so it never fires and the
// runner waits for the child's stdout to close instead.
func waitExited(int) <-chan struct{} { return nil }

// deliverSignal terminates the child and everything it spawned. Windows has no
// signals or process groups; SIGKILL maps to a forced tree kill, anything else
// (SIGTERM) to a graceful tree kill.
func deliverSignal(proc *os.Process, _ int, sig syscall.Signal) error {
	return killTree(proc.Pid, sig == syscall.SIGKILL)
}

// syscallSignalTarget signals an orphan by target. Only a positive pid is
// meaningful on Windows (there are no negative pgids).
func syscallSignalTarget(target int, sig syscall.Signal) error {
	if target <= 0 {
		return nil
	}
	return killTree(target, sig == syscall.SIGKILL)
}

// killTree kills pid and its descendants. taskkill /T walks the child tree; /F
// forces termination (the SIGKILL analog).
func killTree(pid int, force bool) error {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	//nolint:gosec // pid is an int we own.
	return exec.Command("taskkill", args...).Run()
}
