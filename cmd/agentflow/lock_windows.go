//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Lock the entire (small) file: the maximum byte range, like the standard
// "lock whole file" idiom.
const (
	lockBytesLow  = ^uint32(0)
	lockBytesHigh = ^uint32(0)
)

// lockFileExclusive takes a non-blocking exclusive lock via LockFileEx. Windows
// releases the lock when the handle is closed / the process exits.
func lockFileExclusive(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, lockBytesLow, lockBytesHigh, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockBusy
	}
	return err
}

// tryLockShared attempts a non-blocking shared lock; acquired is false (with a
// nil error) when an exclusive holder currently owns the file.
func tryLockShared(f *os.File) (acquired bool, err error) {
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_FAIL_IMMEDIATELY, // no EXCLUSIVE flag => shared
		0, lockBytesLow, lockBytesHigh, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// unlockFile releases any lock held on f.
func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockBytesLow, lockBytesHigh, new(windows.Overlapped))
}
