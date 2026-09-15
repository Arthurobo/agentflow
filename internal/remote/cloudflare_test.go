package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as a fake cloudflared: with AF_FAKE_CLOUDFLARED_DIR
// set it records its arguments and token in that directory, serves /ready
// (200 unless the directory holds a file named "unready") on a free port,
// logs that port the way cloudflared does, and waits to be killed. No real
// cloudflared is started or downloaded by these tests.
func TestMain(m *testing.M) {
	if dir := os.Getenv("AF_FAKE_CLOUDFLARED_DIR"); dir != "" {
		f, err := os.OpenFile(filepath.Join(dir, "runs"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%s token=%s\n", strings.Join(os.Args[1:], " "), os.Getenv("TUNNEL_TOKEN"))
			_ = f.Close()
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(3)
		}
		go func() {
			_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := os.Stat(filepath.Join(dir, "unready")); err == nil || r.URL.Path != "/ready" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
		}()
		fmt.Fprintf(os.Stderr, "INF Starting metrics server on %s/metrics\n", ln.Addr())
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeService stands in for the account service's provisioning call.
type fakeService struct {
	mu    sync.Mutex
	grant TunnelGrant
	err   error
	calls int
}

func (f *fakeService) set(g TunnelGrant, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grant, f.err = g, err
}

func (f *fakeService) fetch(context.Context) (TunnelGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.grant, f.err
}

type cfFixture struct {
	c   *cloudflareRemote
	svc *fakeService
	dir string // the fake cloudflared's directory
}

func newCFFixture(t *testing.T, cached *TunnelGrant) *cfFixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AF_FAKE_CLOUDFLARED_DIR", dir)
	svc := &fakeService{err: errors.New("account service down")}
	cfg := CloudflareConfig{
		DataDir:    t.TempDir(),
		TunnelAddr: "127.0.0.1:0",
		LocalAddr:  "127.0.0.1:4344",
		Fetch:      svc.fetch,
		Log:        slog.New(slog.DiscardHandler),
	}
	if cached != nil {
		g := *cached
		cfg.Cached = func() (TunnelGrant, bool) { return g, true }
	}
	tr := NewCloudflare(cfg).(*cloudflareRemote)
	tr.install = func(context.Context) (string, error) { return os.Args[0], nil }
	tr.poll, tr.minBackoff, tr.maxBackoff, tr.refresh = 10*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond, 30*time.Millisecond
	return &cfFixture{c: tr, svc: svc, dir: dir}
}

func (f *cfFixture) run(t *testing.T, h http.Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = f.c.Run(ctx, h)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run didn't return after cancel")
		}
	})
}

func (f *cfFixture) runs() []string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "runs"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func waitStatus(t *testing.T, tr Transport, what string, ok func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := tr.Status()
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s; status %+v", what, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func running(url string) func(Status) bool {
	return func(st Status) bool { return st.State == StateRunning && st.PublicURL == url }
}

// With a cached tunnel, a service outage doesn't keep the tunnel down.
func TestCloudflareStartsFromCacheWhileServiceIsDown(t *testing.T) {
	f := newCFFixture(t, &TunnelGrant{Hostname: "brave-otter-0042.useagentflow.xyz", Token: "cached-token"})
	f.run(t, http.NotFoundHandler())

	st := waitStatus(t, f.c, "running from the cache", running("https://brave-otter-0042.useagentflow.xyz"))
	if st.Transport != TransportCloudflare || st.Error != "" {
		t.Fatalf("status %+v", st)
	}
	runs := f.runs()
	if len(runs) != 1 || runs[0] != "tunnel --no-autoupdate --metrics 127.0.0.1:0 run token=cached-token" {
		t.Fatalf("cloudflared runs = %q", runs)
	}
	for _, r := range runs {
		if strings.Contains(strings.TrimSuffix(r, " token=cached-token"), "cached-token") {
			t.Fatal("the token is on the command line")
		}
	}
}

// Without a cache the transport waits for the service, saying why.
func TestCloudflareWaitsForTheServiceWithoutCache(t *testing.T) {
	f := newCFFixture(t, nil)
	f.run(t, http.NotFoundHandler())

	st := waitStatus(t, f.c, "an error naming the account service", func(st Status) bool { return st.State == StateError })
	if !strings.Contains(st.Error, "account service") || !strings.Contains(st.Error, "account service down") {
		t.Fatalf("error = %q", st.Error)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "runs")); err == nil {
		t.Fatal("cloudflared started without a tunnel")
	}

	f.svc.set(TunnelGrant{Hostname: "calm-heron-0001.useagentflow.xyz", Token: "fresh"}, nil)
	waitStatus(t, f.c, "running once the service answers", running("https://calm-heron-0001.useagentflow.xyz"))
}

// A token the service changes restarts cloudflared with it.
func TestCloudflareRestartsOnNewGrant(t *testing.T) {
	f := newCFFixture(t, &TunnelGrant{Hostname: "brave-otter-0042.useagentflow.xyz", Token: "old"})
	f.run(t, http.NotFoundHandler())
	waitStatus(t, f.c, "running on the cached token", running("https://brave-otter-0042.useagentflow.xyz"))

	f.svc.set(TunnelGrant{Hostname: "brave-otter-0042.useagentflow.xyz", Token: "new"}, nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs := f.runs()
		if len(runs) >= 2 && strings.HasSuffix(runs[len(runs)-1], "token=new") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cloudflared not restarted with the new token: %q", runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitStatus(t, f.c, "running on the new token", running("https://brave-otter-0042.useagentflow.xyz"))
	time.Sleep(100 * time.Millisecond) // several refreshes with the same grant
	if n := len(f.runs()); n != 2 {
		t.Fatalf("cloudflared started %d times; an unchanged grant must not restart it", n)
	}
}

// Until cloudflared reports an edge connection the URL isn't published.
func TestCloudflareNotRunningUntilConnected(t *testing.T) {
	f := newCFFixture(t, &TunnelGrant{Hostname: "brave-otter-0042.useagentflow.xyz", Token: "t"})
	if err := os.WriteFile(filepath.Join(f.dir, "unready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.run(t, http.NotFoundHandler())
	deadline := time.Now().Add(10 * time.Second)
	for len(f.runs()) == 0 || f.runs()[0] == "" {
		if time.Now().After(deadline) {
			t.Fatal("cloudflared never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if st := f.c.Status(); st.State != StateStarting || st.PublicURL != "" {
		t.Fatalf("status before the edge connection: %+v", st)
	}
	if err := os.Remove(filepath.Join(f.dir, "unready")); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, f.c, "running once connected", running("https://brave-otter-0042.useagentflow.xyz"))
}

// The tunnel listener answers only what cloudflared forwarded, and hands the
// public root the visitor's address.
func TestCloudflareTunnelListener(t *testing.T) {
	f := newCFFixture(t, &TunnelGrant{Hostname: "brave-otter-0042.useagentflow.xyz", Token: "t"})
	var mu sync.Mutex
	var seen string
	f.run(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = ClientIP(r)
		mu.Unlock()
		_, _ = io.WriteString(w, "public root")
	}))
	waitStatus(t, f.c, "running", running("https://brave-otter-0042.useagentflow.xyz"))
	addr := f.c.listenAddr().String()

	get := func(cfIP string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
		if cfIP != "" {
			req.Header.Set("CF-Connecting-IP", cfIP)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(""); code != http.StatusMisdirectedRequest || !strings.Contains(body, "not_forwarded") {
		t.Fatalf("without CF-Connecting-IP: %d %s", code, body)
	}
	if code, _ := get("not-an-ip"); code != http.StatusMisdirectedRequest {
		t.Fatalf("bad CF-Connecting-IP: %d", code)
	}
	if code, body := get("203.0.113.9"); code != http.StatusOK || body != "public root" {
		t.Fatalf("forwarded: %d %s", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != "203.0.113.9" {
		t.Fatalf("client IP seen by the root = %q", seen)
	}
}

func TestCloudflareLogoutAndSelect(t *testing.T) {
	if _, err := Select(SelectConfig{Transport: TransportCloudflare}); err == nil {
		t.Fatal("cloudflare without a Fetch was accepted")
	}
	tr, err := Select(SelectConfig{Transport: TransportCloudflare, Cloudflare: CloudflareConfig{
		DataDir: t.TempDir(),
		Fetch:   func(context.Context) (TunnelGrant, error) { return TunnelGrant{}, errors.New("x") },
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()
	if st := tr.Status(); st.Transport != TransportCloudflare || st.State != StateStarting {
		t.Fatalf("before Run: %+v", st)
	}
	if err := tr.Logout(context.Background()); err == nil {
		t.Fatal("logout of a Cloudflare tunnel succeeded")
	}
	if TransportLabel(TransportCloudflare) != "Cloudflare" {
		t.Fatal("label")
	}
}
