package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// instanceLock is the daemon's single-instance lock, held for its lifetime.
type instanceLock struct {
	f    *os.File
	path string
}

// lockPathFor returns the lock file for a database: next to it, so two
// daemons can never share a database whatever their other settings say.
func lockPathFor(dbPath string) string { return dbPath + ".lock" }

// acquireInstanceLock takes an exclusive, non-blocking flock on dbPath.lock
// and records this process's pid in it. A second daemon on the same database
// gets an error naming the first one's pid: starting anyway would reap every
// live run the first daemon owns.
func acquireInstanceLock(dbPath string) (*instanceLock, error) {
	path := lockPathFor(dbPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("agentflow is already running (pid %d; lock %s)", readLockPID(path), path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &instanceLock{f: f, path: path}, nil
}

// daemonHoldsLock reports whether a daemon currently holds the instance lock
// for dbPath, without creating the lock file. The CLI uses it to tell "the
// daemon is stopped" from "the daemon runs but its admin socket is
// unreachable", which call for different messages.
func daemonHoldsLock(dbPath string) bool {
	f, err := os.Open(lockPathFor(dbPath))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return err == syscall.EWOULDBLOCK
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// Release drops the lock. The kernel also drops it when the process exits.
func (l *instanceLock) Release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// readLockPID returns the pid recorded in a lock file, or 0.
func readLockPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}
