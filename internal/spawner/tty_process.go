package spawner

import (
	"io"
	"os"
)

// ptyMasterFile is the master side of a pty: raw duplex byte I/O for the tty
// hub, plus the terminal resize/size operations. creack/pty backs it on Unix
// (tty_pty_unix.go); ConPTY backs it on Windows (tty_pty_windows.go).
type ptyMasterFile interface {
	io.ReadWriteCloser
	resize(cols, rows uint16) error
	size() (cols, rows uint16, err error)
}

// ttyPTY is a child process started attached to a pty. master is the terminal
// master side; process is the child handle the spawner drives (kill/identity);
// wait blocks until the child exits and returns its wait error.
type ttyPTY struct {
	master  ptyMasterFile
	process *os.Process
	wait    func() error
}
