//go:build unix

package spawner

import (
	"os"
	"syscall"
)

// deliverSignal sends sig to the live child handle and, when the child leads a
// known process group, to that group (negative pgid) so the signal reaches the
// child's own descendants. pgid 0 means the group is unknown. This is the exact
// behavior signal() had inline before the Windows split.
func deliverSignal(proc *os.Process, pgid int, sig syscall.Signal) error {
	err := proc.Signal(sig)
	if pgid > 0 {
		if kerr := syscall.Kill(-pgid, sig); kerr != nil && err == nil { //nolint:gosec // pgid is a group we lead
			err = kerr
		}
	}
	return err
}

// syscallSignalTarget signals an orphan by target, which is either a positive
// pid or a negative pgid the identity check has vouched for.
func syscallSignalTarget(target int, sig syscall.Signal) error {
	return syscall.Kill(target, sig) //nolint:gosec // target is a pid or negative pgid
}
