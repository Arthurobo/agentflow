//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

// lockFileExclusive takes a non-blocking exclusive advisory (flock) lock.
func lockFileExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLockBusy
	}
	return err
}

// tryLockShared attempts a non-blocking shared lock; acquired is false (with a
// nil error) when an exclusive holder currently owns the file.
func tryLockShared(f *os.File) (acquired bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// unlockFile releases any lock held on f.
func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
