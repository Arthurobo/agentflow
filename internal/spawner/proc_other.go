//go:build unix && !linux

package spawner

import "syscall"

// childSysProcAttr makes a headless child the leader of its own process
// group, so one kill(-pgid) reaches everything it spawned. There is no
// parent-death signal outside Linux; a restarted agentd's reaper covers a
// hard crash instead.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// ReadProcIdentity has no /proc to read outside Linux, so no identity can be
// recorded. A run from a previous agentd life therefore can never be proven
// to be the process at its pid, and the reaper marks it gone without
// signalling rather than risk killing whatever reused the pid. Live children
// are unaffected: they are signalled through the handle agentd holds.
func ReadProcIdentity(int) (pgid int, startTicks int64) { return 0, 0 }

// ProcessAlive reports whether pid exists. Without /proc a zombie still reads
// as alive until its parent reaps it, which only delays a confirmation.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// waitExited cannot observe an unreaped exit here, so it never fires and the
// headless runner waits for the child's stdout to close instead.
func waitExited(int) <-chan struct{} { return nil }
