package cloudflared

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Supervisor runs cloudflared as a child process of the daemon and restarts
// it with backoff until ctx is done. A named tunnel keeps its hostname across
// restarts, so the tunnel lives and dies with the daemon.
type Supervisor struct {
	Bin  string
	Args []string
	// Env is added to the daemon's environment for the child. Secrets go
	// here rather than in Args, which any local user can read in ps.
	Env []string
	// OnLine receives every output line.
	OnLine func(line string)
	// OnExit, when set, is called after every exit with its error.
	OnExit func(err error)
	Log    *slog.Logger

	MinBackoff time.Duration // default 1 s
	MaxBackoff time.Duration // default 1 min
	// StableAfter resets the backoff when a run lasted at least this long
	// (default 1 min).
	StableAfter time.Duration
}

// Run blocks until ctx is done. The child is killed when ctx ends.
func (s *Supervisor) Run(ctx context.Context) {
	log := s.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	minB, maxB, stable := s.MinBackoff, s.MaxBackoff, s.StableAfter
	if minB <= 0 {
		minB = time.Second
	}
	if maxB <= 0 {
		maxB = time.Minute
	}
	if stable <= 0 {
		stable = time.Minute
	}
	backoff := minB
	for ctx.Err() == nil {
		started := time.Now()
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if s.OnExit != nil {
			s.OnExit(err)
		}
		if time.Since(started) >= stable {
			backoff = minB
		}
		log.Warn("cloudflared exited; restarting", "err", err, "in", backoff)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, maxB)
	}
}

func (s *Supervisor) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, s.Bin, s.Args...) //nolint:gosec // the verified cloudflared with arguments agentflow builds
	cmd.WaitDelay = 5 * time.Second
	if len(s.Env) > 0 {
		cmd.Env = append(os.Environ(), s.Env...)
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	var wg sync.WaitGroup
	wg.Go(func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			if s.OnLine != nil {
				s.OnLine(sc.Text())
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	})
	err := cmd.Run()
	_ = pw.Close()
	wg.Wait()
	return err
}
