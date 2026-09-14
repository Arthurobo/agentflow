package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

// fakeNode is a scripted tsnet node. Tests change its fields while the run
// loop polls it.
type fakeNode struct {
	mu          sync.Mutex
	startErr    error
	statusErr   error
	backend     string
	authURL     string
	caps        []tailcfg.NodeCapability
	domains     []string
	qf          tailcfg.QueryFeatureResponse
	qfErr       error
	qfCalls     int
	listenFails int
	listens     int
	lnAddr      string
	logouts     int
	startLogins int
	closed      bool
	events      *[]string
}

func (n *fakeNode) set(fn func(n *fakeNode)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fn(n)
}

func (n *fakeNode) Start() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.startErr
}

func (n *fakeNode) Status(context.Context) (*ipnstate.Status, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.statusErr != nil {
		return nil, n.statusErr
	}
	capMap := tailcfg.NodeCapMap{}
	for _, c := range n.caps {
		capMap[c] = nil
	}
	return &ipnstate.Status{
		BackendState: n.backend,
		AuthURL:      n.authURL,
		Self:         &ipnstate.PeerStatus{CapMap: capMap},
		CertDomains:  append([]string(nil), n.domains...),
	}, nil
}

func (n *fakeNode) QueryFunnel(context.Context) (*tailcfg.QueryFeatureResponse, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.qfCalls++
	if n.qfErr != nil {
		return nil, n.qfErr
	}
	qf := n.qf
	return &qf, nil
}

func (n *fakeNode) StartLogin(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.startLogins++
	return nil
}

func (n *fakeNode) Logout(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.logouts++
	n.backend = ipn.NeedsLogin.String()
	n.authURL = ""
	return nil
}

func (n *fakeNode) CertDomains() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.domains...)
}

func (n *fakeNode) PrewarmCert(context.Context, string) error { return nil }

func (n *fakeNode) Listen(Mode) (net.Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.listenFails > 0 {
		n.listenFails--
		return nil, errors.New("listener not ready")
	}
	n.listens++
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		n.lnAddr = ln.Addr().String()
	}
	return ln, err
}

func (n *fakeNode) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	if n.events != nil {
		*n.events = append(*n.events, "node.Close")
	}
	return nil
}

var testTimings = timings{
	poll:       5 * time.Millisecond,
	funnelPoll: 5 * time.Millisecond,
	minBackoff: 5 * time.Millisecond,
	maxBackoff: 20 * time.Millisecond,
	shutdown:   time.Second,
	loginNudge: 3,
}

func readyNode() *fakeNode {
	return &fakeNode{
		backend: ipn.Running.String(),
		caps:    []tailcfg.NodeCapability{tailcfg.CapabilityHTTPS, tailcfg.NodeAttrFunnel},
		domains: []string{"agentflow-3fa9c1.tail1234.ts.net"},
	}
}

// startRemote runs a tsnetRemote over the given nodes (one per Start attempt;
// the last is reused) and returns it with a stop func.
func startRemote(t *testing.T, cfg Config, h http.Handler, nodes ...*fakeNode) (*tsnetRemote, *int) {
	t.Helper()
	var mu sync.Mutex
	created := 0
	r := newTSNetRemote(cfg, testTimings, func(string, string) node {
		mu.Lock()
		defer mu.Unlock()
		i := created
		if i >= len(nodes) {
			i = len(nodes) - 1
		}
		created++
		return nodes[i]
	})
	r.setenv = func(string, string) error { return nil }
	if cfg.DataDir == "" {
		r.cfg.DataDir = t.TempDir()
	}
	if h == nil {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx, h)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return r, &created
}

func waitFor(t *testing.T, r Transport, what string, ok func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := r.Status(); ok(st) {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; last status %+v", what, r.Status())
	return Status{}
}

func TestRunWalksLoginAndFunnelApprovalBeforeServing(t *testing.T) {
	n := &fakeNode{backend: ipn.NeedsLogin.String(), authURL: "https://login.tailscale.com/a/abc"}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.WriteString(w, "public handler from "+ClientIP(req))
	})
	r, _ := startRemote(t, Config{Mode: ModeFunnel}, h, n)

	st := waitFor(t, r, "needs_login", func(s Status) bool { return s.State == StateNeedsLogin })
	if st.Transport != TransportTailscale {
		t.Fatalf("transport = %q", st.Transport)
	}
	if st.AuthURL != "https://login.tailscale.com/a/abc" {
		t.Fatalf("auth url = %q", st.AuthURL)
	}

	// Logged in, but the tailnet hasn't enabled HTTPS + Funnel for the node.
	n.set(func(n *fakeNode) {
		n.backend = ipn.Running.String()
		n.authURL = ""
		n.qf = tailcfg.QueryFeatureResponse{URL: "https://login.tailscale.com/f/funnel?node=x"}
	})
	st = waitFor(t, r, "needs_funnel_approval", func(s Status) bool { return s.State == StateNeedsFunnelApproval })
	if st.ApproveURL != "https://login.tailscale.com/f/funnel?node=x" {
		t.Fatalf("approve url = %q", st.ApproveURL)
	}
	n.mu.Lock()
	listens := n.listens
	n.mu.Unlock()
	if listens != 0 {
		t.Fatal("listened before Funnel was approved")
	}

	n.set(func(n *fakeNode) {
		n.caps = []tailcfg.NodeCapability{tailcfg.CapabilityHTTPS, tailcfg.NodeAttrFunnel}
		n.domains = []string{"agentflow-3fa9c1.tail1234.ts.net"}
		n.qf = tailcfg.QueryFeatureResponse{Complete: true}
	})
	st = waitFor(t, r, "running", func(s Status) bool { return s.State == StateRunning })
	if st.PublicURL != "https://agentflow-3fa9c1.tail1234.ts.net" {
		t.Fatalf("public url = %q", st.PublicURL)
	}

	// The handler is really served on the remote listener.
	r.mu.Lock()
	srv := r.httpSrv
	r.mu.Unlock()
	if srv == nil {
		t.Fatal("no http server while running")
	}
	n.mu.Lock()
	addr := n.lnAddr
	n.mu.Unlock()
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "public handler from 127.0.0.1" {
		t.Fatalf("body = %q", body)
	}
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 120*time.Second || srv.MaxHeaderBytes != 64<<10 {
		t.Fatalf("server timeouts: header=%v idle=%v maxHeader=%d", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes)
	}
}

func TestRunNeverPublishesRunningWithoutPublicURL(t *testing.T) {
	n := readyNode()
	n.domains = nil // tailnet without HTTPS certificates
	r, _ := startRemote(t, Config{Mode: ModeTailnet}, nil, n)
	st := waitFor(t, r, "error", func(s Status) bool { return s.State == StateError })
	if !strings.Contains(st.Error, "certificate") {
		t.Fatalf("error = %q", st.Error)
	}
	n.mu.Lock()
	listens, qf := n.listens, n.qfCalls
	n.mu.Unlock()
	if listens != 0 {
		t.Fatal("listened without a certificate domain")
	}
	if qf != 0 {
		t.Fatal("tailnet mode queried Funnel")
	}
	if r.Status().State == StateRunning {
		t.Fatal("running without a public URL")
	}
}

func TestRunRetriesStartStatusAndListenFailures(t *testing.T) {
	broken := &fakeNode{startErr: errors.New("state dir locked")}
	good := readyNode()
	good.statusErr = errors.New("localapi not up")
	good.listenFails = 2

	var sawErrors []string
	var mu sync.Mutex
	r, created := startRemote(t, Config{}, nil, broken, good)
	ch, cancel := r.Subscribe()
	defer cancel()
	go func() {
		for st := range ch {
			if st.State == StateError {
				mu.Lock()
				sawErrors = append(sawErrors, st.Error)
				mu.Unlock()
			}
		}
	}()

	waitFor(t, r, "error from status", func(s Status) bool {
		return s.State == StateError && strings.Contains(s.Error, "localapi not up")
	})
	good.set(func(n *fakeNode) { n.statusErr = nil })
	waitFor(t, r, "running", func(s Status) bool { return s.State == StateRunning })

	if !broken.closed {
		t.Fatal("node whose Start failed was not closed")
	}
	if *created != 2 {
		t.Fatalf("nodes created = %d, want 2 (a failed Start must be replaced)", *created)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(sawErrors, "|")
	if !strings.Contains(joined, "listen") {
		t.Fatalf("listen failure never surfaced as error state: %q", joined)
	}
}

func TestFunnelWaitsForCapabilitiesEvenAfterApproval(t *testing.T) {
	n := readyNode()
	n.caps = []tailcfg.NodeCapability{tailcfg.CapabilityHTTPS} // funnel attr not in the netmap yet
	n.qf = tailcfg.QueryFeatureResponse{Complete: true}
	r, _ := startRemote(t, Config{}, nil, n)

	time.Sleep(50 * time.Millisecond)
	if st := r.Status(); st.State == StateRunning {
		t.Fatal("running before the node had the funnel attribute")
	}
	n.set(func(n *fakeNode) { n.caps = append(n.caps, tailcfg.NodeAttrFunnel) })
	waitFor(t, r, "running", func(s Status) bool { return s.State == StateRunning })
}

func TestQueryFeatureErrorIsRetried(t *testing.T) {
	n := readyNode()
	n.caps = nil
	n.qfErr = errors.New("control unreachable")
	r, _ := startRemote(t, Config{}, nil, n)
	waitFor(t, r, "error", func(s Status) bool {
		return s.State == StateError && strings.Contains(s.Error, "control unreachable")
	})
	n.set(func(n *fakeNode) {
		n.qfErr = nil
		n.caps = []tailcfg.NodeCapability{tailcfg.CapabilityHTTPS, tailcfg.NodeAttrFunnel}
	})
	waitFor(t, r, "running", func(s Status) bool { return s.State == StateRunning })
}

func TestNeedsMachineAuthPointsAtAdminConsole(t *testing.T) {
	n := &fakeNode{backend: ipn.NeedsMachineAuth.String()}
	r, _ := startRemote(t, Config{}, nil, n)
	st := waitFor(t, r, "needs_login", func(s Status) bool { return s.State == StateNeedsLogin })
	if st.AuthURL != MachineAuthURL {
		t.Fatalf("auth url = %q", st.AuthURL)
	}
}

func TestLogoutGoesBackToNeedsLoginWithNewURL(t *testing.T) {
	n := readyNode()
	r, _ := startRemote(t, Config{}, nil, n)
	waitFor(t, r, "running", func(s Status) bool { return s.State == StateRunning })

	if err := r.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	n.mu.Lock()
	logouts, logins := n.logouts, n.startLogins
	n.mu.Unlock()
	if logouts != 1 || logins < 1 {
		t.Fatalf("logout calls=%d startLogin calls=%d", logouts, logins)
	}
	waitFor(t, r, "needs_login", func(s Status) bool { return s.State == StateNeedsLogin })
	n.set(func(n *fakeNode) { n.authURL = "https://login.tailscale.com/a/new" })
	waitFor(t, r, "new auth url", func(s Status) bool {
		return s.State == StateNeedsLogin && s.AuthURL == "https://login.tailscale.com/a/new"
	})
	n.set(func(n *fakeNode) { n.backend = ipn.Running.String() })
	waitFor(t, r, "running again", func(s Status) bool { return s.State == StateRunning })
}

func TestCloseShutsDownThenClosesHijackedThenNode(t *testing.T) {
	var events []string
	n := readyNode()
	n.events = &events
	var r *tsnetRemote
	cfg := Config{CloseHijacked: func() {
		// The HTTP server must already have stopped accepting.
		n.mu.Lock()
		addr := n.lnAddr
		n.mu.Unlock()
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			_ = c.Close()
			events = append(events, "hijacked-before-shutdown")
			return
		}
		events = append(events, "closeHijacked")
	}}
	r, _ = startRemote(t, cfg, nil, n)
	waitFor(t, r, "running", func(s Status) bool { return s.State == StateRunning })
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	_ = r.Close() // idempotent
	if got := strings.Join(events, ","); got != "closeHijacked,node.Close" {
		t.Fatalf("close order = %q", got)
	}
}

func TestStatusChangesAreMirroredToRemoteJSON(t *testing.T) {
	dir := t.TempDir()
	n := readyNode()
	r, _ := startRemote(t, Config{DataDir: dir}, nil, n)
	waitFor(t, r, "running", func(s Status) bool { return s.State == StateRunning })
	path := filepath.Join(dir, "remote.json")
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := ReadStatusFile(path)
		if err == nil && st.State == StateRunning {
			if st.PublicURL != "https://agentflow-3fa9c1.tail1234.ts.net" {
				t.Fatalf("file public url = %q", st.PublicURL)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote.json never showed running: %+v %v", st, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("remote.json mode = %v", fi.Mode().Perm())
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".remote-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestStatusJSONUsesCamelCaseKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote.json")
	st := Status{State: StateNeedsFunnelApproval, Transport: TransportTailscale, Mode: ModeFunnel, AuthURL: "a", ApproveURL: "b", PublicURL: "c", Error: "d", UpdatedAt: time.Unix(1, 0)}
	if err := WriteStatusFile(path, st); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, key := range []string{`"state"`, `"transport"`, `"mode"`, `"authUrl"`, `"approveUrl"`, `"publicUrl"`, `"error"`, `"updatedAt"`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Fatalf("missing %s in %s", key, raw)
		}
	}
}

func TestTailscaleDirIsPrivateAndHostnamePersisted(t *testing.T) {
	dataDir := t.TempDir()
	tsDir := filepath.Join(dataDir, "tailscale")
	if err := os.MkdirAll(tsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var gotHost, gotDir string
	r := newTSNetRemote(Config{DataDir: dataDir}, testTimings, func(h, d string) node {
		gotHost, gotDir = h, d
		return readyNode()
	})
	r.setenv = func(string, string) error { return nil }
	if _, err := r.ensureNode(); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(tsDir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("tailscale dir mode = %v", fi.Mode().Perm())
	}
	if gotDir != tsDir {
		t.Fatalf("node dir = %q", gotDir)
	}
	if !strings.HasPrefix(gotHost, "agentflow-") || gotHost == "agentflow-000000" || !ValidHostname(gotHost) {
		t.Fatalf("hostname = %q", gotHost)
	}
	again, err := loadOrCreateHostname(tsDir)
	if err != nil || again != gotHost {
		t.Fatalf("hostname not persisted: %q vs %q (%v)", again, gotHost, err)
	}
}

func TestHostnameIsRandomTrimmedAndValidated(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		name, err := loadOrCreateHostname(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if len(name) != len("agentflow-")+6 || !ValidHostname(name) {
			t.Fatalf("bad generated name %q", name)
		}
		seen[name] = true
	}
	if len(seen) < 19 {
		t.Fatalf("generated names are not random: %v", seen)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hostname"), []byte("  agentflow-abc123\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, _ := loadOrCreateHostname(dir); name != "agentflow-abc123" {
		t.Fatalf("whitespace not trimmed: %q", name)
	}
	if err := os.WriteFile(filepath.Join(dir, "hostname"), []byte("Not A Hostname!"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, _ := loadOrCreateHostname(dir)
	if !strings.HasPrefix(name, "agentflow-") || !ValidHostname(name) {
		t.Fatalf("invalid file content not replaced: %q", name)
	}
}

func TestInvalidHostnameOverrideIsAnError(t *testing.T) {
	r := newTSNetRemote(Config{DataDir: t.TempDir(), Hostname: "bad_name"}, testTimings, func(string, string) node {
		t.Fatal("node created with an invalid hostname")
		return nil
	})
	r.setenv = func(string, string) error { return nil }
	if _, err := r.ensureNode(); err == nil || !strings.Contains(err.Error(), "AF_TS_HOSTNAME") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyLogPolicy(t *testing.T) {
	set := map[string]string{}
	setenv := func(k, v string) error { set[k] = v; return nil }

	if err := applyLogPolicy(false, setenv); err != nil {
		t.Fatal(err)
	}
	if set["TS_NO_LOGS_NO_SUPPORT"] != "true" {
		t.Fatalf("logs off did not set the knob: %v", set)
	}

	set = map[string]string{}
	if err := applyLogPolicy(true, setenv); err != nil {
		t.Fatal(err)
	}
	if _, ok := set["TS_NO_LOGS_NO_SUPPORT"]; ok {
		t.Fatalf("AF_TS_LOGS=on still disabled log upload: %v", set)
	}
}

func TestLogPolicyAppliedBeforeNodeStarts(t *testing.T) {
	var order []string
	n := readyNode()
	r := newTSNetRemote(Config{DataDir: t.TempDir()}, testTimings, func(string, string) node {
		order = append(order, "newNode")
		return n
	})
	r.setenv = func(k, v string) error {
		order = append(order, k+"="+v)
		return nil
	}
	if _, err := r.ensureNode(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "TS_NO_LOGS_NO_SUPPORT=true,newNode" {
		t.Fatalf("order = %v", order)
	}
}

func TestNextBackoffDoublesAndCaps(t *testing.T) {
	d := time.Second
	var got []time.Duration
	for i := 0; i < 8; i++ {
		d = nextBackoff(d, 60*time.Second)
		got = append(got, d)
	}
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff[%d] = %v, want %v", i, got[i], want[i]*time.Second)
		}
	}
}

func TestUserLogfDropsLoginReminder(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := userLogf(log)
	f("To start this tsnet server, restart with TS_AUTHKEY set, or go to: %s", "https://login.tailscale.com/a/x")
	if buf.Len() != 0 {
		t.Fatalf("reminder logged: %s", buf.String())
	}
	f("AuthLoop: state is %v", "Running")
	if !strings.Contains(buf.String(), "AuthLoop: state is Running") || !strings.Contains(buf.String(), "level=DEBUG") {
		t.Fatalf("message not routed to debug: %s", buf.String())
	}
}

// pipeAddrConn gives a net.Pipe end a real-looking RemoteAddr.
type pipeAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c pipeAddrConn) RemoteAddr() net.Addr { return c.remote }

func TestClientIPFromFunnelConnUsesSrcNotRelay(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	relay := &net.TCPAddr{IP: net.ParseIP("100.100.1.1"), Port: 443}
	fc := &ipn.FunnelConn{
		Conn: pipeAddrConn{Conn: server, remote: relay},
		Src:  netip.MustParseAddrPort("203.0.113.7:51234"),
	}
	tc := tls.Server(fc, &tls.Config{MinVersion: tls.VersionTLS12})
	if got := ClientIPFromConn(tc); got != "203.0.113.7" {
		t.Fatalf("funnel client ip = %q, want 203.0.113.7", got)
	}
	if got := ClientIPFromConn(fc); got != "203.0.113.7" {
		t.Fatalf("bare funnel conn ip = %q", got)
	}
}

func TestClientIPFromTailnetConnUsesRemoteAddr(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	peer := pipeAddrConn{Conn: server, remote: &net.TCPAddr{IP: net.ParseIP("100.64.0.9"), Port: 40000}}
	tc := tls.Server(peer, &tls.Config{MinVersion: tls.VersionTLS12})
	if got := ClientIPFromConn(tc); got != "100.64.0.9" {
		t.Fatalf("tailnet client ip = %q", got)
	}
	ctx := connContext(context.Background(), tc)
	req, _ := http.NewRequestWithContext(ctx, "GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:1"
	if got := ClientIP(req); got != "100.64.0.9" {
		t.Fatalf("ClientIP from context = %q", got)
	}
	plain, _ := http.NewRequest("GET", "/", nil)
	plain.RemoteAddr = "127.0.0.1:5555"
	if got := ClientIP(plain); got != "127.0.0.1" {
		t.Fatalf("fallback ClientIP = %q", got)
	}
}

func TestOffStatus(t *testing.T) {
	if st := (Off{}).Status(); st.State != StateOff {
		t.Fatalf("state = %s", st.State)
	}
	if err := (Off{}).Logout(context.Background()); err == nil {
		t.Fatal("logout with remote off should fail")
	}
}

func TestStatusBusDedupesAndFansOut(t *testing.T) {
	writes := 0
	bus := newStatusBus(Status{State: StateOff}, func(Status) { writes++ })
	ch, cancel := bus.subscribe()
	defer cancel()
	bus.publish(Status{State: StateRunning, PublicURL: "https://x.ts.net", UpdatedAt: time.Unix(1, 0)})
	bus.publish(Status{State: StateRunning, PublicURL: "https://x.ts.net", UpdatedAt: time.Unix(2, 0)})
	if writes != 2 { // initial + one change
		t.Fatalf("onChange calls = %d, want 2", writes)
	}
	select {
	case st := <-ch:
		if st.State != StateRunning {
			t.Fatalf("got %+v", st)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber saw nothing")
	}
	select {
	case st := <-ch:
		t.Fatalf("duplicate delivered: %+v", st)
	default:
	}
}

func TestFakeServesHandlerAndFollowsScript(t *testing.T) {
	f := NewFake(Status{State: StateStarting})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = f.Run(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "ip="+ClientIP(r))
		}))
	}()
	base, err := f.URL(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f.Script(ctx, time.Millisecond,
		Status{State: StateNeedsLogin, AuthURL: "https://login.tailscale.com/a/1"},
		Status{State: StateRunning},
	)
	st := waitFor(t, f, "running", func(s Status) bool { return s.State == StateRunning })
	if st.PublicURL != base {
		t.Fatalf("public url %q, want listener %q", st.PublicURL, base)
	}
	resp, err := http.Get(st.PublicURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ip=127.0.0.1" {
		t.Fatalf("body = %q", body)
	}
	if err := f.Logout(context.Background()); err != nil || f.Logouts() != 1 || f.Status().State != StateNeedsLogin {
		t.Fatalf("logout: err=%v count=%d state=%s", err, f.Logouts(), f.Status().State)
	}
}
