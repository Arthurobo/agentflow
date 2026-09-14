package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

var errFakeDenied = errors.New("fake: access denied")
var errFakeStale = errors.New("fake: etag mismatch")

func init() {
	// The fake LocalAPI answers with its own error values.
	accessDenied = func(err error) bool { return errors.Is(err, errFakeDenied) }
	preconditionFailed = func(err error) bool { return errors.Is(err, errFakeStale) }
}

// fakeLocalAPI is a scripted tailscaled. Tests change its fields while the
// transport polls it.
type fakeLocalAPI struct {
	mu        sync.Mutex
	statusErr error
	backend   string
	authURL   string
	dnsName   string
	caps      []tailcfg.NodeCapability
	domains   []string
	qf        tailcfg.QueryFeatureResponse
	features  []string
	serve     *ipn.ServeConfig
	etag      int
	denySet   bool
	sets      int
}

func runningLocalAPI() *fakeLocalAPI {
	return &fakeLocalAPI{
		backend: ipn.Running.String(),
		dnsName: "desk.tail1234.ts.net.",
		caps:    []tailcfg.NodeCapability{tailcfg.CapabilityHTTPS, tailcfg.NodeAttrFunnel},
		domains: []string{"desk.tail1234.ts.net"},
		serve:   &ipn.ServeConfig{},
	}
}

func (f *fakeLocalAPI) set(fn func(f *fakeLocalAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeLocalAPI) StatusWithoutPeers(context.Context) (*ipnstate.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	capMap := tailcfg.NodeCapMap{}
	for _, c := range f.caps {
		capMap[c] = nil
	}
	return &ipnstate.Status{
		BackendState: f.backend,
		AuthURL:      f.authURL,
		Self:         &ipnstate.PeerStatus{DNSName: f.dnsName, CapMap: capMap},
		CertDomains:  append([]string(nil), f.domains...),
	}, nil
}

func (f *fakeLocalAPI) QueryFeature(_ context.Context, feature string) (*tailcfg.QueryFeatureResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.features = append(f.features, feature)
	qf := f.qf
	return &qf, nil
}

func (f *fakeLocalAPI) GetServeConfig(context.Context) (*ipn.ServeConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sc := cloneServeConfig(f.serve)
	sc.ETag = string(rune('a' + f.etag))
	return sc, nil
}

func (f *fakeLocalAPI) SetServeConfig(_ context.Context, sc *ipn.ServeConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.denySet {
		return errFakeDenied
	}
	if sc.ETag != string(rune('a'+f.etag)) {
		return errFakeStale
	}
	f.serve = cloneServeConfig(sc)
	f.etag++
	f.sets++
	return nil
}

func (f *fakeLocalAPI) serveConfig() *ipn.ServeConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneServeConfig(f.serve)
}

var systemTestTimings = systemTimings{
	poll:       5 * time.Millisecond,
	retry:      5 * time.Millisecond,
	minBackoff: 5 * time.Millisecond,
	maxBackoff: 20 * time.Millisecond,
	shutdown:   time.Second,
}

func tailscaleInstalled(string) (string, error) { return "/usr/bin/tailscale", nil }

// startSystem runs the system transport over lc until the test ends (or the
// returned stop is called) and returns it.
func startSystem(t *testing.T, cfg Config, lc *fakeLocalAPI, lookPath func(string) (string, error), h http.Handler) (*systemTailscale, func()) {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	if cfg.LocalAddr == "" {
		cfg.LocalAddr = "127.0.0.1:4344"
	}
	if lookPath == nil {
		lookPath = tailscaleInstalled
	}
	if h == nil {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "public root from "+ClientIP(r))
		})
	}
	s := newSystemTailscale(cfg, systemTestTimings, lc, lookPath)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = s.Run(ctx, h)
		close(done)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("Run did not return after cancel")
			}
		})
	}
	t.Cleanup(stop)
	return s, stop
}

// someoneElsesServe is a serve config the person set up themselves.
func someoneElsesServe() *ipn.ServeConfig {
	return &ipn.ServeConfig{
		TCP: map[uint16]*ipn.TCPPortHandler{443: {HTTPS: true}, 22: {TCPForward: "127.0.0.1:22"}},
		Web: map[ipn.HostPort]*ipn.WebServerConfig{
			"desk.tail1234.ts.net:443": {Handlers: map[string]*ipn.HTTPHandler{"/": {Proxy: "http://127.0.0.1:3000"}}},
		},
		AllowFunnel: map[ipn.HostPort]bool{"desk.tail1234.ts.net:443": true},
	}
}

func TestSystemTailscaleMergesItsEntryAndServes(t *testing.T) {
	lc := runningLocalAPI()
	lc.serve = someoneElsesServe()
	s, _ := startSystem(t, Config{Mode: ModeFunnel}, lc, nil, nil)

	st := waitFor(t, s, "running", func(st Status) bool { return st.State == StateRunning })
	if st.PublicURL != "https://desk.tail1234.ts.net:8443" || st.Transport != TransportTailscale || st.Backend != BackendSystem || st.Mode != ModeFunnel {
		t.Fatalf("status = %+v", st)
	}

	sc := lc.serveConfig()
	hp := ipn.HostPort("desk.tail1234.ts.net:8443")
	web := sc.Web[hp]
	if web == nil || web.Handlers["/"] == nil || !strings.HasPrefix(web.Handlers["/"].Proxy, "http://127.0.0.1:") {
		t.Fatalf("agentflow's entry: %+v", sc.Web)
	}
	if tcp := sc.TCP[SystemServePort]; tcp == nil || !tcp.HTTPS || !sc.AllowFunnel[hp] {
		t.Fatalf("port and funnel for the entry: tcp=%+v funnel=%v", sc.TCP, sc.AllowFunnel)
	}
	// Everything the person had is still there, unchanged.
	mine := someoneElsesServe()
	if sc.TCP[443] == nil || !sc.TCP[443].HTTPS || sc.TCP[22] == nil || sc.TCP[22].TCPForward != "127.0.0.1:22" ||
		sc.Web["desk.tail1234.ts.net:443"].Handlers["/"].Proxy != mine.Web["desk.tail1234.ts.net:443"].Handlers["/"].Proxy ||
		!sc.AllowFunnel["desk.tail1234.ts.net:443"] {
		t.Fatalf("existing entries were changed: %+v", sc)
	}

	// The entry reaches the public root, with the client tailscaled names.
	req, _ := http.NewRequest("GET", web.Handlers["/"].Proxy+"/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "public root from 203.0.113.9" {
		t.Fatalf("through the tunnel listener: %q", body)
	}
}

func TestSystemTailscaleShutdownRemovesOnlyItsEntry(t *testing.T) {
	lc := runningLocalAPI()
	lc.serve = someoneElsesServe()
	dataDir := t.TempDir()
	s, stop := startSystem(t, Config{DataDir: dataDir}, lc, nil, nil)
	waitFor(t, s, "running", func(st Status) bool { return st.State == StateRunning })
	if _, err := os.Stat(filepath.Join(dataDir, serveRecordName)); err != nil {
		t.Fatalf("the entry was not recorded: %v", err)
	}
	stop()

	got, _ := json.Marshal(lc.serveConfig())
	want, _ := json.Marshal(someoneElsesServe())
	if string(got) != string(want) {
		t.Fatalf("after shutdown:\n got %s\nwant %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(dataDir, serveRecordName)); !os.IsNotExist(err) {
		t.Fatalf("the record outlived the entry: %v", err)
	}
}

func TestSystemTailscaleTailnetModeHasNoFunnel(t *testing.T) {
	lc := runningLocalAPI()
	lc.caps = []tailcfg.NodeCapability{tailcfg.CapabilityHTTPS} // no funnel attribute: not needed
	s, _ := startSystem(t, Config{Mode: ModeTailnet}, lc, nil, nil)
	st := waitFor(t, s, "running", func(st Status) bool { return st.State == StateRunning })
	sc := lc.serveConfig()
	if st.Mode != ModeTailnet || len(sc.AllowFunnel) != 0 || sc.Web["desk.tail1234.ts.net:8443"] == nil {
		t.Fatalf("status %+v serve %+v", st, sc)
	}
}

func TestSystemTailscaleWaitsForOperatorPermission(t *testing.T) {
	lc := runningLocalAPI()
	lc.denySet = true
	s, _ := startSystem(t, Config{}, lc, nil, nil)
	st := waitFor(t, s, "needs_permission", func(st Status) bool { return st.State == StateNeedsPermission })
	if st.Command != "sudo tailscale set --operator=$USER" || st.PublicURL != "" {
		t.Fatalf("status = %+v", st)
	}
	lc.set(func(f *fakeLocalAPI) { f.denySet = false })
	waitFor(t, s, "running once permitted", func(st Status) bool { return st.State == StateRunning })
}

func TestSystemTailscaleNotInstalled(t *testing.T) {
	notFound := func(string) (string, error) { return "", errors.New("not found") }
	s, _ := startSystem(t, Config{}, runningLocalAPI(), notFound, nil)
	st := waitFor(t, s, "needs_install", func(st Status) bool { return st.State == StateNeedsInstall })
	if st.InstallURL != "https://tailscale.com/download" {
		t.Fatalf("status = %+v", st)
	}

	// Installed, but tailscaled isn't answering.
	lc := runningLocalAPI()
	lc.statusErr = errors.New("dial unix /var/run/tailscale/tailscaled.sock: connect: no such file or directory")
	s2, _ := startSystem(t, Config{}, lc, nil, nil)
	st = waitFor(t, s2, "needs_install", func(st Status) bool { return st.State == StateNeedsInstall })
	if st.InstallURL == "" || !strings.Contains(st.Error, "not answering") {
		t.Fatalf("status = %+v", st)
	}
	lc.set(func(f *fakeLocalAPI) { f.statusErr = nil })
	waitFor(t, s2, "running once tailscaled answers", func(st Status) bool { return st.State == StateRunning })
}

func TestSystemTailscaleLoggedOut(t *testing.T) {
	lc := runningLocalAPI()
	lc.backend = ipn.NeedsLogin.String()
	lc.authURL = "https://login.tailscale.com/a/xyz"
	s, _ := startSystem(t, Config{}, lc, nil, nil)
	st := waitFor(t, s, "needs_login", func(st Status) bool { return st.State == StateNeedsLogin })
	if st.AuthURL != "https://login.tailscale.com/a/xyz" {
		t.Fatalf("status = %+v", st)
	}
	lc.set(func(f *fakeLocalAPI) { f.authURL = "" })
	st = waitFor(t, s, "needs_login without a link", func(st Status) bool { return st.State == StateNeedsLogin && st.AuthURL == "" })
	if st.Command != "tailscale up" {
		t.Fatalf("status = %+v", st)
	}
	lc.set(func(f *fakeLocalAPI) { f.backend = ipn.Stopped.String() })
	if st := waitFor(t, s, "stopped", func(st Status) bool { return st.Command == "tailscale up" }); st.State != StateNeedsLogin {
		t.Fatalf("stopped: %+v", st)
	}
	if got := lc.serveConfig(); len(got.Web) != 0 {
		t.Fatalf("configured serving while signed out: %+v", got)
	}
	if err := s.Logout(context.Background()); err == nil {
		t.Fatal("Logout signed the computer's Tailscale out")
	}
}

func TestSystemTailscaleAsksForFunnelApproval(t *testing.T) {
	lc := runningLocalAPI()
	lc.caps = []tailcfg.NodeCapability{tailcfg.CapabilityHTTPS}
	lc.qf = tailcfg.QueryFeatureResponse{URL: "https://login.tailscale.com/f/funnel?node=n1"}
	s, _ := startSystem(t, Config{}, lc, nil, nil)
	st := waitFor(t, s, "needs_funnel_approval", func(st Status) bool { return st.State == StateNeedsFunnelApproval })
	if st.ApproveURL != "https://login.tailscale.com/f/funnel?node=n1" {
		t.Fatalf("status = %+v", st)
	}
	lc.set(func(f *fakeLocalAPI) {
		f.caps = []tailcfg.NodeCapability{tailcfg.CapabilityHTTPS, tailcfg.NodeAttrFunnel}
	})
	waitFor(t, s, "running once approved", func(st Status) bool { return st.State == StateRunning })
}

func TestSystemTailscaleLeavesAPortSomeoneElseServes(t *testing.T) {
	lc := runningLocalAPI()
	lc.serve = &ipn.ServeConfig{
		TCP: map[uint16]*ipn.TCPPortHandler{8443: {HTTPS: true}},
		Web: map[ipn.HostPort]*ipn.WebServerConfig{
			"desk.tail1234.ts.net:8443": {Handlers: map[string]*ipn.HTTPHandler{"/": {Proxy: "http://127.0.0.1:9999"}}},
		},
	}
	before, _ := json.Marshal(lc.serve)
	s, _ := startSystem(t, Config{}, lc, nil, nil)
	st := waitFor(t, s, "error", func(st Status) bool { return st.State == StateError })
	if !strings.Contains(st.Error, "already served by something else") {
		t.Fatalf("status = %+v", st)
	}
	after, _ := json.Marshal(lc.serveConfig())
	if string(before) != string(after) {
		t.Fatalf("someone else's entry was changed:\n%s\n%s", before, after)
	}
}

func TestSystemTailscaleReplacesItsOwnEntryFromAnEarlierRun(t *testing.T) {
	dataDir := t.TempDir()
	old := serveRecord{HostPort: "desk.tail1234.ts.net:8443", Proxy: "http://127.0.0.1:41234", Funnel: true}
	data, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dataDir, serveRecordName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lc := runningLocalAPI()
	lc.serve = &ipn.ServeConfig{
		TCP:         map[uint16]*ipn.TCPPortHandler{8443: {HTTPS: true}},
		Web:         map[ipn.HostPort]*ipn.WebServerConfig{old.HostPort: {Handlers: map[string]*ipn.HTTPHandler{"/": {Proxy: old.Proxy}}}},
		AllowFunnel: map[ipn.HostPort]bool{old.HostPort: true},
	}
	s, _ := startSystem(t, Config{DataDir: dataDir}, lc, nil, nil)
	waitFor(t, s, "running", func(st Status) bool { return st.State == StateRunning })
	if proxy := lc.serveConfig().Web[old.HostPort].Handlers["/"].Proxy; proxy == old.Proxy {
		t.Fatal("the stale entry from the crashed run still points at its old port")
	}
}

func TestSystemTailscalePutsItsEntryBackAfterAReset(t *testing.T) {
	lc := runningLocalAPI()
	s, _ := startSystem(t, Config{}, lc, nil, nil)
	waitFor(t, s, "running", func(st Status) bool { return st.State == StateRunning })
	lc.set(func(f *fakeLocalAPI) { f.serve = &ipn.ServeConfig{}; f.etag++ })
	deadline := time.Now().Add(5 * time.Second)
	for lc.serveConfig().Web["desk.tail1234.ts.net:8443"] == nil {
		if time.Now().After(deadline) {
			t.Fatal("the entry was not restored after `tailscale serve reset`")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSystemTunnelNeverUsesTheLocalListener(t *testing.T) {
	s, _ := startSystem(t, Config{TunnelAddr: "127.0.0.1:4344", LocalAddr: "127.0.0.1:4344"}, runningLocalAPI(), nil, nil)
	st := waitFor(t, s, "error", func(st Status) bool { return st.State == StateError })
	if !strings.Contains(st.Error, "local listener") {
		t.Fatalf("status = %+v", st)
	}
}

func TestSelectUsesTheSystemTailscaleUnlessEmbedded(t *testing.T) {
	sys, _ := Select(SelectConfig{Transport: TransportTailscale, Tailscale: Config{DataDir: t.TempDir()}})
	if _, ok := sys.(*systemTailscale); !ok || sys.Status().Backend != BackendSystem {
		t.Fatalf("default tailscale transport: %T %+v", sys, sys.Status())
	}
	_ = sys.Close()
	emb, _ := Select(SelectConfig{Transport: TransportTailscale, Tailscale: Config{DataDir: t.TempDir(), Embedded: true}})
	if _, ok := emb.(*tsnetRemote); !ok || emb.Status().Backend != BackendEmbedded {
		t.Fatalf("AF_TAILSCALE=embedded: %T %+v", emb, emb.Status())
	}
	_ = emb.Close()
}

func TestSystemTunnelRefusesRequestsTailscaledDidNotForward(t *testing.T) {
	lc := runningLocalAPI()
	s, _ := startSystem(t, Config{}, lc, nil, nil)
	waitFor(t, s, "running", func(st Status) bool { return st.State == StateRunning })
	proxy := lc.serveConfig().Web["desk.tail1234.ts.net:8443"].Handlers["/"].Proxy
	resp, err := http.Get(proxy + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("a request without X-Forwarded-For: %d, want 421", resp.StatusCode)
	}
}

func TestEmbeddedFallbackOnlyForAnEarlierNodeWithoutAWorkingSystemTailscale(t *testing.T) {
	withState := t.TempDir()
	if err := os.MkdirAll(filepath.Join(withState, "tailscale"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(withState, "tailscale", "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	notInstalled := func(string) (string, error) { return "", errors.New("not found") }
	answering := func(context.Context) error { return nil }
	silent := func(context.Context) error { return errors.New("connection refused") }
	probes := 0
	counting := func(ctx context.Context) error { probes++; return silent(ctx) }

	cases := []struct {
		name     string
		dataDir  string
		lookPath func(string) (string, error)
		answers  func(context.Context) error
		want     bool
	}{
		{"earlier node, no Tailscale installed", withState, notInstalled, silent, true},
		{"earlier node, Tailscale installed but silent", withState, tailscaleInstalled, counting, true},
		{"earlier node, Tailscale answering", withState, tailscaleInstalled, answering, false},
		{"no earlier node, no Tailscale", t.TempDir(), notInstalled, silent, false},
		{"no data dir", "", notInstalled, silent, false},
	}
	for _, c := range cases {
		if got := useEmbeddedFallback(Config{DataDir: c.dataDir}, c.lookPath, c.answers, 30*time.Millisecond); got != c.want {
			t.Errorf("%s: fallback = %v, want %v", c.name, got, c.want)
		}
	}
	if probes < 2 {
		t.Fatalf("a silent tailscaled got %d probe(s); it should be given a moment to start", probes)
	}
}
