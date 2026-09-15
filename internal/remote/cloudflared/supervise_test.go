package cloudflared

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as a fake cloudflared: with AF_FAKE_CLOUDFLARED set
// it records its arguments and token, serves /ready on a free port, logs the
// lines the real one does, and then exits or waits to be killed. No real
// cloudflared is ever started.
func TestMain(m *testing.M) {
	if mode := os.Getenv("AF_FAKE_CLOUDFLARED"); mode != "" {
		fakeCloudflared(mode)
	}
	os.Exit(m.Run())
}

func fakeCloudflared(mode string) {
	if f, err := os.OpenFile(os.Getenv("AF_FAKE_CLOUDFLARED_RUNS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = fmt.Fprintf(f, "%s token=%s\n", strings.Join(os.Args[1:], " "), os.Getenv("TUNNEL_TOKEN"))
		_ = f.Close()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(3)
	}
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/ready" && mode != "unready" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
	}()
	fmt.Fprintf(os.Stderr, "2026-09-15T10:00:00Z INF Starting metrics server on %s/metrics\n", ln.Addr())
	fmt.Fprintln(os.Stderr, "2026-09-15T10:00:00Z INF Registered tunnel connection connIndex=0 connection=abc event=0 ip=198.41.200.1 location=lhr01 protocol=quic")
	if mode == "exit" {
		os.Exit(1)
	}
	time.Sleep(time.Hour)
	os.Exit(0)
}

func fakeSupervisor(t *testing.T, mode string) (*Supervisor, string) {
	t.Helper()
	runs := t.TempDir() + "/runs"
	t.Setenv("AF_FAKE_CLOUDFLARED", mode)
	t.Setenv("AF_FAKE_CLOUDFLARED_RUNS", runs)
	return &Supervisor{
		Bin:         os.Args[0],
		Args:        RunArgs(),
		Env:         []string{TokenEnv("secret-token")},
		MinBackoff:  10 * time.Millisecond,
		MaxBackoff:  20 * time.Millisecond,
		StableAfter: time.Hour,
	}, runs
}

func countRuns(path string) int {
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), "\n")
}

func TestSupervisorRestartsWithTokenInEnv(t *testing.T) {
	s, runs := fakeSupervisor(t, "exit")
	var mu sync.Mutex
	exits := 0
	s.OnExit = func(error) { mu.Lock(); exits++; mu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	deadline := time.Now().Add(10 * time.Second)
	for countRuns(runs) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("cloudflared ran %d times, want restarts", countRuns(runs))
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	b, _ := os.ReadFile(runs)
	if !strings.HasPrefix(string(b), "tunnel --no-autoupdate --metrics 127.0.0.1:0 run token=secret-token\n") {
		t.Fatalf("runs = %q", b)
	}
	mu.Lock()
	defer mu.Unlock()
	if exits < 2 {
		t.Fatalf("OnExit called %d times", exits)
	}
}

func TestSupervisorStopsChildOnCancel(t *testing.T) {
	s, runs := fakeSupervisor(t, "wait")
	r := &Readiness{}
	s.OnLine = r.Line
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	deadline := time.Now().Add(10 * time.Second)
	for !r.Ready(ctx, nil) {
		if time.Now().After(deadline) {
			t.Fatal("the fake tunnel never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr, _ := r.snapshot(); addr == "" {
		t.Fatal("metrics address not read from the log")
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor didn't stop the child")
	}
	if time.Since(start) > 6*time.Second {
		t.Fatalf("stopping took %s", time.Since(start))
	}
	if n := countRuns(runs); n != 1 {
		t.Fatalf("a long-running child was started %d times", n)
	}
}

// /ready answering 503 wins over a registered connection in the log.
func TestReadinessPrefersMetrics(t *testing.T) {
	s, _ := fakeSupervisor(t, "unready")
	r := &Readiness{}
	s.OnLine = r.Line
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if addr, conns := r.snapshot(); addr != "" && conns > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("log lines never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Ready(ctx, nil) {
		t.Fatal("ready although /ready answers 503")
	}
}
