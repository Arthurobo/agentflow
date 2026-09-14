package spawner

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// childSysProcAttr makes a headless child the leader of its own process
// group, so one kill(-pgid) reaches everything it spawned (subagents, bash,
// MCP servers). Pdeathsig is the belt to that: if agentd dies hard, the
// kernel still kills the child.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// ReadProcIdentity extracts the kill identity (pgid + start-ticks) from
// /proc/<pid>/stat. Returns zeros if the read fails (the row will be
// missing kill identity, the reconciler will treat it as "no live
// process" and reap it as an orphan — still safe, just less precise).
//
// /proc/<pid>/stat format (one line, space-separated): pid (comm) state ppid
// pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt utime stime
// cutime cstime priority nice num_threads itrealvalue starttime vsize rss
// rsslim startcode endcode startstack kstkesp kstkeip signals blocked
// sigignore sigcatch wchan nswap cnswap exit_signal processor.
//
// Field indices (1-based per man proc(5)):
//
//	1: pid 5: pgrp (= pgid) 22: starttime (clock ticks since boot)
func ReadProcIdentity(pid int) (pgid int, startTicks int64) {
	rest, ok := procStatFields(pid)
	// rest[0] is field 3. So field N lives at rest[N-3]:
	// field 3 state -> rest[0]
	// field 4 ppid -> rest[1]
	// field 5 pgrp -> rest[2] <- the pgid we kill with
	// field 22 starttime -> rest[19] <- the identity that survives PID reuse
	// Getting these wrong is catastrophic, not cosmetic: rest[1] is the
	// PARENT pid (agentd itself), so kill(-rest[1]) would SIGKILL agentd's
	// own process group, and rest[21] is rss (which changes as the process
	// runs) so every identity check would read as PID reuse and refuse to
	// signal real orphans. Pinned by TestReadProcIdentityMatchesKernel.
	if !ok || len(rest) < 20 {
		return 0, 0
	}
	if n, err := strconv.Atoi(rest[2]); err == nil {
		pgid = n
	}
	if n, err := strconv.ParseInt(rest[19], 10, 64); err == nil {
		startTicks = n
	}
	return pgid, startTicks
}

// ProcessAlive reports whether pid is a live (non-zombie) process.
//
// os.FindProcess is NOT usable for this on Unix: it never returns an error,
// it just wraps the integer. signal 0 is the portable liveness probe, but on
// its own it still answers "alive" for a zombie (killed, not yet reaped) —
// which is how a confirmed kill can look like a survivor. So we also read
// the process state from /proc and treat Z as gone.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false // ESRCH (gone) or EPERM (not ours — not our problem)
	}
	rest, ok := procStatFields(pid)
	if !ok || len(rest) == 0 {
		return false
	}
	return rest[0] != "Z" // zombie == dead for our purposes
}

// procStatFields returns /proc/<pid>/stat split after the command name, so
// rest[0] is field 3 (state).
func procStatFields(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) //nolint:gosec // pid is process-scoped
	if err != nil {
		return nil, false
	}
	// split on the LAST ')' so the comm (which can contain spaces) is
	// safely separated from the rest of the fields
	s := string(data)
	end := strings.LastIndex(s, ")")
	if end < 0 || end+2 >= len(s) {
		return nil, false
	}
	return strings.Fields(s[end+2:]), true
}

// waitExited returns a channel closed once the child pid has exited, without
// reaping it: waitid with WNOWAIT leaves the zombie for os/exec's own Wait.
// It lets the headless runner tell "the process is gone but something it
// started still holds stdout" from "the process is still writing".
func waitExited(pid int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		const pPID = 1
		var info [128]byte // siginfo_t
		for {
			//nolint:gosec // G103: waitid(WNOWAIT) has no wrapper in syscall; info outlives the call
			_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid),
				uintptr(unsafe.Pointer(&info[0])), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
			if errno != syscall.EINTR {
				return // exited, or already reaped (ECHILD): either way it is gone
			}
		}
	}()
	return ch
}
