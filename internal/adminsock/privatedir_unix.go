//go:build unix

package adminsock

import (
	"fmt"
	"os"
	"syscall"
)

// checkPrivateDir reports an error unless dir is a directory (not a symlink)
// owned by the current user with no permissions for group or others.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible to other users (mode %o)", dir, fi.Mode().Perm())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot read its owner", dir)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by another user (uid %d)", dir, st.Uid)
	}
	return nil
}
