//go:build unix

package spawner

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// startTTYProcess starts bin+args attached to a new pty sized cols x rows, in
// dir, with env, and returns the master side plus the child handle. This is the
// exact creack/pty path the tty spawn used inline before the Windows split.
func startTTYProcess(bin string, args []string, dir string, env []string, cols, rows uint16) (*ttyPTY, error) {
	//nolint:gosec // G204: the spawner intentionally launches the user-configured engine binary with a built argv.
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return &ttyPTY{
		master:  &unixPTYMaster{f: master},
		process: cmd.Process,
		wait:    cmd.Wait,
	}, nil
}

// unixPTYMaster wraps a creack/pty master *os.File.
type unixPTYMaster struct{ f *os.File }

func (m *unixPTYMaster) Read(p []byte) (int, error)  { return m.f.Read(p) }
func (m *unixPTYMaster) Write(p []byte) (int, error) { return m.f.Write(p) }
func (m *unixPTYMaster) Close() error                { return m.f.Close() }

func (m *unixPTYMaster) resize(cols, rows uint16) error {
	return pty.Setsize(m.f, &pty.Winsize{Cols: cols, Rows: rows})
}

func (m *unixPTYMaster) size() (cols, rows uint16, err error) {
	r, c, err := pty.Getsize(m.f) // creack returns (rows, cols)
	if err != nil {
		return 0, 0, err
	}
	return uint16(c), uint16(r), nil
}
