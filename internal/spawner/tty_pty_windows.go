//go:build windows

package spawner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

// startTTYProcess starts bin+args attached to a Windows pseudoconsole (ConPTY)
// sized cols x rows, in dir, with env. Unlike the Unix path, ConPTY owns process
// creation, so we build a command line and read the child's pid back out.
func startTTYProcess(bin string, args []string, dir string, env []string, cols, rows uint16) (*ttyPTY, error) {
	// Resolve the engine binary honoring PATHEXT (npm installs Claude Code as a
	// .cmd shim, not a .exe). A .cmd/.bat is not a PE image, so CreateProcess
	// cannot run it directly — route those through cmd.exe /c.
	resolved := bin
	if p, err := exec.LookPath(bin); err == nil {
		resolved = p
	}
	full := append([]string{resolved}, args...)
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".cmd", ".bat":
		full = append([]string{"cmd.exe", "/c", resolved}, args...)
	}
	cmdline := windows.ComposeCommandLine(full)

	opts := []conpty.ConPtyOption{conpty.ConPtyDimensions(int(cols), int(rows))}
	if dir != "" {
		opts = append(opts, conpty.ConPtyWorkDir(dir))
	}
	if len(env) > 0 {
		opts = append(opts, conpty.ConPtyEnv(env))
	}
	c, err := conpty.Start(cmdline, opts...)
	if err != nil {
		return nil, err
	}
	proc, err := os.FindProcess(c.Pid())
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return &ttyPTY{
		master:  &winPTYMaster{c: c, cols: cols, rows: rows},
		process: proc,
		wait: func() error {
			_, werr := c.Wait(context.Background())
			return werr
		},
	}, nil
}

// winPTYMaster wraps a ConPTY. ConPTY has no "get size", so the last applied
// size is remembered for size().
type winPTYMaster struct {
	c    *conpty.ConPty
	mu   sync.Mutex
	cols uint16
	rows uint16
}

func (m *winPTYMaster) Read(p []byte) (int, error)  { return m.c.Read(p) }
func (m *winPTYMaster) Write(p []byte) (int, error) { return m.c.Write(p) }
func (m *winPTYMaster) Close() error                { return m.c.Close() }

func (m *winPTYMaster) resize(cols, rows uint16) error {
	if err := m.c.Resize(int(cols), int(rows)); err != nil {
		return err
	}
	m.mu.Lock()
	m.cols, m.rows = cols, rows
	m.mu.Unlock()
	return nil
}

func (m *winPTYMaster) size() (cols, rows uint16, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cols, m.rows, nil
}
