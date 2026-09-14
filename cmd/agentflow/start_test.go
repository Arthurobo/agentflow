package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/adminsock"
	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/remote"
	"github.com/arthurobo/agentflow/internal/store"
)

// fakeService is a serviceManager that records what the CLI asked for.
type fakeService struct {
	mu        sync.Mutex
	installed []serviceSpec
	restarts  int
	removed   int
	state     serviceState
	path      string
}

func (f *fakeService) Supported() bool { return true }
func (f *fakeService) Install(_ context.Context, spec serviceSpec) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = append(f.installed, spec)
	f.state = serviceState{Installed: true, Enabled: true, Running: true}
	return len(f.installed) == 1, nil
}
func (f *fakeService) Stop(context.Context) error { return nil }
func (f *fakeService) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	return nil
}
func (f *fakeService) Remove(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed++
	return nil
}
func (f *fakeService) Query(context.Context) serviceState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}
func (f *fakeService) ServicePATH() string                     { return f.path }
func (f *fakeService) LogHint() string                         { return "journalctl --user -u agentflow" }
func (f *fakeService) StaysUpAfterLogout(context.Context) bool { return true }

// syncBuffer is a bytes.Buffer safe for the test to read while the flow
// writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeDaemon is an admin socket backed by a remote.Fake.
type fakeDaemon struct {
	remote  *remote.Fake
	mints   int
	revoked []string
	mu      sync.Mutex
	// requests are the pending access requests; decisions records
	// "approve <id>" / "deny <id>" in order.
	requests  []adminsock.PairRequest
	decisions []string
	reloads   int
}

func (d *fakeDaemon) addRequest(r adminsock.PairRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, r)
}

func (d *fakeDaemon) decided() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.decisions...)
}

// shortDataDir returns a data directory short enough for a unix socket path.
func shortDataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "af")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startFakeDaemon(t *testing.T, dataDir string, initial remote.Status) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{remote: remote.NewFake(initial)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := adminsock.Start(ctx, adminsock.Config{
		Dir:    adminsock.Dir(dataDir),
		Status: func() any { return d.remote.Status() },
		Logout: d.remote.Logout,
		Revoke: func(_ context.Context, id string) (int, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			if id == "missing" {
				return 0, adminsock.ErrNotFound
			}
			d.revoked = append(d.revoked, id)
			return 3, nil
		},
		MintPair: func(context.Context) (string, int64, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.mints++
			return "tok123", time.Now().Add(15 * time.Minute).UnixMilli(), nil
		},
		Health: func() any { return map[string]any{"status": "ok", "version": "test"} },
		PairRequests: func() []adminsock.PairRequest {
			d.mu.Lock()
			defer d.mu.Unlock()
			return append([]adminsock.PairRequest(nil), d.requests...)
		},
		DecidePairRequest: func(_ context.Context, idOrCode string, approve bool) (adminsock.PairRequest, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			for i, r := range d.requests {
				if r.ID != idOrCode && r.MatchCode != idOrCode {
					continue
				}
				d.requests = append(d.requests[:i], d.requests[i+1:]...)
				verb := "deny"
				if approve {
					verb = "approve"
				}
				d.decisions = append(d.decisions, verb+" "+r.ID)
				return r, nil
			}
			return adminsock.PairRequest{}, adminsock.ErrNotFound
		},
		ReloadAccount: func() any {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.reloads++
			return accountState{Reporting: true}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return d
}

func newTestStartFlow(t *testing.T, cfg config, out *syncBuffer) (*startFlow, *fakeService, *[]string) {
	t.Helper()
	svc := &fakeService{}
	var opened []string
	var mu sync.Mutex
	f := &startFlow{
		out:     out,
		cfg:     cfg,
		svc:     svc,
		admin:   adminClient(cfg),
		spec:    serviceSpec{ExecPath: "/home/u/.local/bin/agentflow", PATH: "/usr/bin"},
		envFile: filepath.Join(t.TempDir(), "agentflow.env"),
		open: func(u string) error {
			mu.Lock()
			defer mu.Unlock()
			opened = append(opened, u)
			return nil
		},
		probe:      func(context.Context, string) error { return nil },
		poll:       5 * time.Millisecond,
		socketWait: 2 * time.Second,
		statusWait: 5 * time.Second,
		healthWait: 2 * time.Second,
	}
	return f, svc, &opened
}

func testConfig(dataDir string) config {
	return config{
		dbPath:     filepath.Join(dataDir, "agentflow.db"),
		dataDir:    dataDir,
		addr:       "127.0.0.1:4344",
		machineID:  "test-machine",
		remoteMode: "tailscale",
	}
}

func TestStartWalksSignInApprovalAndPairsOnPublicURL(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	d := startFakeDaemon(t, dataDir, remote.Status{State: remote.StateStarting})
	out := &syncBuffer{}
	f, svc, opened := newTestStartFlow(t, cfg, out)

	probes := 0
	f.probe = func(_ context.Context, u string) error {
		if u != "https://agentflow-3fa9c1.tail1234.ts.net" {
			t.Errorf("probed %q", u)
		}
		probes++
		if probes < 3 {
			return errors.New("no such host")
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.remote.Script(ctx, 30*time.Millisecond,
		remote.Status{State: remote.StateNeedsLogin},
		remote.Status{State: remote.StateNeedsLogin, AuthURL: "https://login.tailscale.com/a/abc123"},
		remote.Status{State: remote.StateNeedsFunnelApproval, ApproveURL: "https://login.tailscale.com/f/funnel?node=n1"},
		remote.Status{State: remote.StateError, Error: "listen: funnel not yet enabled"},
		remote.Status{State: remote.StateRunning, PublicURL: "https://agentflow-3fa9c1.tail1234.ts.net"},
	)

	if err := f.run(ctx); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	text := out.String()
	order := []string{
		"Installed the agentflow service",
		"Waiting for Tailscale to provide a sign-in link",
		"Sign in to Tailscale",
		"https://login.tailscale.com/a/abc123",
		"One click to allow public HTTPS",
		"https://login.tailscale.com/f/funnel?node=n1",
		"will retry: listen: funnel not yet enabled",
		"Remote URL: https://agentflow-3fa9c1.tail1234.ts.net (Tailscale funnel)",
		"Waiting for the address to become reachable",
		"It's reachable.",
		"Scan this with your phone camera",
		"https://agentflow-3fa9c1.tail1234.ts.net/pair/#token=tok123&expires=",
	}
	pos := 0
	for _, want := range order {
		i := strings.Index(text[pos:], want)
		if i < 0 {
			t.Fatalf("output lacks %q after position %d:\n%s", want, pos, text)
		}
		pos += i + len(want)
	}
	if !strings.Contains(text, "█") && !strings.Contains(text, "▀") {
		t.Fatalf("no terminal QR code in output:\n%s", text)
	}
	if len(*opened) != 2 || (*opened)[0] != "https://login.tailscale.com/a/abc123" || (*opened)[1] != "https://login.tailscale.com/f/funnel?node=n1" {
		t.Fatalf("opened = %v", *opened)
	}
	if len(svc.installed) != 1 || svc.installed[0].ExecPath != "/home/u/.local/bin/agentflow" {
		t.Fatalf("installed = %+v", svc.installed)
	}
	if d.mints != 1 {
		t.Fatalf("mints = %d", d.mints)
	}
	if _, err := os.Stat(f.envFile); err != nil {
		t.Fatalf("env file not created: %v", err)
	}
}

func TestStartWithRemoteOffPairsOnLocalURL(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	cfg.remoteMode = "off"
	startFakeDaemon(t, dataDir, remote.Status{State: remote.StateOff})
	out := &syncBuffer{}
	f, _, opened := newTestStartFlow(t, cfg, out)
	f.probe = func(context.Context, string) error {
		t.Error("probed a public URL with remote access off")
		return nil
	}
	if err := f.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "Remote access is off") || !strings.Contains(text, "http://127.0.0.1:4344/pair/#token=tok123&expires=") {
		t.Fatalf("output:\n%s", text)
	}
	if strings.Contains(text, "only works on this computer") {
		t.Fatalf("remote-off pairing should not warn about remote state:\n%s", text)
	}
	if len(*opened) != 0 {
		t.Fatalf("opened %v", *opened)
	}
}

func TestStartFailsClearlyWhenDaemonNeverAnswers(t *testing.T) {
	dataDir := shortDataDir(t)
	out := &syncBuffer{}
	f, _, _ := newTestStartFlow(t, testConfig(dataDir), out)
	f.socketWait = 50 * time.Millisecond
	err := f.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not answer") || !strings.Contains(err.Error(), "journalctl") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartGivesUpWaitingButKeepsServiceRunning(t *testing.T) {
	dataDir := shortDataDir(t)
	d := startFakeDaemon(t, dataDir, remote.Status{State: remote.StateNeedsLogin, AuthURL: "https://login.tailscale.com/a/x"})
	out := &syncBuffer{}
	f, _, _ := newTestStartFlow(t, testConfig(dataDir), out)
	f.statusWait = 60 * time.Millisecond
	if err := f.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "still isn't ready (needs_login)") || d.mints != 0 {
		t.Fatalf("mints=%d output:\n%s", d.mints, out.String())
	}
}

func TestStartStopsOnCtrlC(t *testing.T) {
	dataDir := shortDataDir(t)
	startFakeDaemon(t, dataDir, remote.Status{State: remote.StateStarting})
	out := &syncBuffer{}
	f, _, _ := newTestStartFlow(t, testConfig(dataDir), out)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := f.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestPairFallsBackToStoreWhenDaemonStopped(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	var out bytes.Buffer
	watch, err := pairFlow(context.Background(), &out, cfg, adminClient(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if watch {
		t.Fatal("asked to watch for access requests with no daemon to send them")
	}
	text := out.String()
	const prefix = "http://127.0.0.1:4344/pair/#token="
	i := strings.Index(text, prefix)
	if i < 0 || !strings.Contains(text, "agentflow isn't running") || !strings.Contains(text, "Open this on this computer") ||
		!strings.Contains(text, "To pair a phone, run `agentflow pair` again once remote access is running") ||
		strings.Contains(text, "Scan this") || strings.Contains(text, "█") {
		t.Fatalf("output:\n%s", text)
	}
	token, _, _ := strings.Cut(text[i+len(prefix):], "&")
	db, err := store.Open(cfg.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	dev, err := db.VerifyDevice(context.Background(), token)
	if err != nil || dev == nil || dev.Kind != "pairing" || dev.Status != "pending" {
		t.Fatalf("token %q not stored as a pending pairing row: %+v %v", token, dev, err)
	}
}

func TestPairOnlyMintsWhenALinkWillOpen(t *testing.T) {
	hasQR := func(s string) bool { return strings.Contains(s, "█") || strings.Contains(s, "▀") }
	cases := []struct {
		name   string
		status remote.Status
		mints  int
		want   []string
		reject []string
		qr     bool
	}{
		{
			name:   "off",
			status: remote.Status{State: remote.StateOff},
			mints:  1,
			want:   []string{"Open this on this computer", "http://127.0.0.1:4344/pair/#token=tok123&expires="},
			reject: []string{"phone"},
		},
		{
			name:   "needs_login",
			status: remote.Status{State: remote.StateNeedsLogin, AuthURL: "https://login.tailscale.com/a/abc123"},
			want:   []string{"isn't running yet (needs_login)", "Sign in to Tailscale", "https://login.tailscale.com/a/abc123", "Run `agentflow pair` again once it's running"},
			reject: []string{"127.0.0.1", "token=", "Scan this with your phone"},
			qr:     true,
		},
		{
			name:   "needs_funnel_approval",
			status: remote.Status{State: remote.StateNeedsFunnelApproval, ApproveURL: "https://login.tailscale.com/f/funnel?node=n1"},
			want:   []string{"isn't running yet (needs_funnel_approval)", "https://login.tailscale.com/f/funnel?node=n1", "Run `agentflow pair` again once it's running"},
			reject: []string{"127.0.0.1", "token=", "Scan this with your phone"},
			qr:     true,
		},
		{
			name:   "error",
			status: remote.Status{State: remote.StateError, Error: "listen: boom"},
			want:   []string{"isn't running yet (error)", "will retry: listen: boom", "Run `agentflow pair` again once it's running"},
			reject: []string{"127.0.0.1", "token=", "Scan this with your phone"},
		},
		{
			name:   "needs_install",
			status: remote.Status{State: remote.StateNeedsInstall, InstallURL: remote.TailscaleInstallURL},
			want:   []string{"isn't running yet (needs_install)", "Tailscale isn't installed", "https://tailscale.com/download", "AF_TAILSCALE=embedded"},
			reject: []string{"127.0.0.1", "token="},
			qr:     true,
		},
		{
			name:   "needs_permission",
			status: remote.Status{State: remote.StateNeedsPermission, Command: remote.OperatorCommand},
			want:   []string{"isn't running yet (needs_permission)", "sudo tailscale set --operator=$USER", "continues on its own"},
			reject: []string{"127.0.0.1", "token="},
		},
		{
			name:   "system tailscale signed out",
			status: remote.Status{State: remote.StateNeedsLogin, Command: "tailscale up"},
			want:   []string{"signed out or stopped", "tailscale up"},
			reject: []string{"127.0.0.1", "token=", "Waiting for Tailscale to provide"},
		},
		{
			name:   "starting",
			status: remote.Status{State: remote.StateStarting},
			want:   []string{"isn't running yet (starting)", "Run `agentflow pair` again once it's running"},
			reject: []string{"127.0.0.1", "token="},
		},
		{
			name:   "running",
			status: remote.Status{State: remote.StateRunning, PublicURL: "https://agentflow-3fa9c1.tail1234.ts.net"},
			mints:  1,
			want: []string{"Scan this with your phone camera", "https://agentflow-3fa9c1.tail1234.ts.net/pair/#token=tok123&expires=",
				"Open https://agentflow-3fa9c1.tail1234.ts.net/pair/ on the phone, tap Request access"},
			reject: []string{"127.0.0.1", "isn't running"},
			qr:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := shortDataDir(t)
			cfg := testConfig(dataDir)
			d := startFakeDaemon(t, dataDir, tc.status)
			var out bytes.Buffer
			watch, err := pairFlow(context.Background(), &out, cfg, adminClient(cfg))
			if err != nil {
				t.Fatal(err)
			}
			// Access requests are worth watching for exactly when a link
			// was shown.
			if watch != (tc.mints == 1) {
				t.Errorf("watch = %v with %d mints", watch, tc.mints)
			}
			text := out.String()
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("output lacks %q", w)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(text, r) {
					t.Errorf("output contains %q", r)
				}
			}
			if hasQR(text) != tc.qr {
				t.Errorf("QR code printed = %v, want %v", hasQR(text), tc.qr)
			}
			if d.mints != tc.mints {
				t.Errorf("mints = %d, want %d", d.mints, tc.mints)
			}
			if t.Failed() {
				t.Logf("output:\n%s", text)
			}
		})
	}
}

func TestPairingURLAndLocalBase(t *testing.T) {
	if got := pairingURL("https://a.ts.net/", "abc", 1757000000000); got != "https://a.ts.net/pair/#token=abc&expires=1757000000000" {
		t.Fatalf("pairingURL = %q", got)
	}
	cases := map[string]string{
		"127.0.0.1:4344":    "http://127.0.0.1:4344",
		":5000":             "http://127.0.0.1:5000",
		"0.0.0.0:4344":      "http://127.0.0.1:4344",
		"[::1]:4344":        "http://127.0.0.1:4344",
		"192.168.1.20:4344": "http://192.168.1.20:4344",
		"garbage":           "http://127.0.0.1:4344",
	}
	for addr, want := range cases {
		if got := localBaseURL(addr); got != want {
			t.Errorf("localBaseURL(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestRevokeViaDaemonAndFallback(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	ctx := context.Background()

	// Daemon stopped: revoke in the database.
	db, err := store.Open(cfg.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertDevice(ctx, &store.Device{ID: "dev-1", Name: "phone", Kind: "device", Status: "active", TokenHash: store.HashToken("t")}, 0); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	var out bytes.Buffer
	if err := revokeDevice(ctx, &out, cfg, adminClient(cfg), "dev-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no live sessions were open") {
		t.Fatalf("output: %s", out.String())
	}
	db, _ = store.Open(cfg.dbPath)
	d, _ := db.GetDeviceByID(ctx, "dev-1")
	_ = db.Close()
	if d == nil || d.Status != "revoked" {
		t.Fatalf("device = %+v", d)
	}
	if err := revokeDevice(ctx, &out, cfg, adminClient(cfg), "nope"); err == nil {
		t.Fatal("unknown device revoked")
	}

	// Daemon running but not answering on its socket: don't claim it is
	// stopped; its token re-check closes the sessions.
	db, _ = store.Open(cfg.dbPath)
	_ = db.UpsertDevice(ctx, &store.Device{ID: "dev-3", Name: "tablet", Kind: "device", Status: "active", TokenHash: store.HashToken("t3")}, 0)
	_ = db.Close()
	held, err := acquireInstanceLock(cfg.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := revokeDevice(ctx, &out, cfg, adminClient(cfg), "dev-3"); err != nil {
		t.Fatal(err)
	}
	held.Release()
	if !strings.Contains(out.String(), "did not answer") || strings.Contains(out.String(), "isn't running") {
		t.Fatalf("output with an unreachable running daemon: %s", out.String())
	}

	// Daemon running: through the socket, which closes live sessions.
	dd := startFakeDaemon(t, dataDir, remote.Status{State: remote.StateOff})
	out.Reset()
	if err := revokeDevice(ctx, &out, cfg, adminClient(cfg), "dev-2"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "closed 3 live connection(s)") || len(dd.revoked) != 1 {
		t.Fatalf("output %q revoked %v", out.String(), dd.revoked)
	}
	if err := revokeDevice(ctx, &out, cfg, adminClient(cfg), "missing"); err == nil || !strings.Contains(err.Error(), "no device") {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoteCommands(t *testing.T) {
	dataDir := shortDataDir(t)
	cfg := testConfig(dataDir)
	ctx := context.Background()
	envFile := filepath.Join(t.TempDir(), "agentflow.env")
	svc := &fakeService{state: serviceState{Installed: true, Running: true}}

	var out bytes.Buffer
	if err := remoteCommand(ctx, &out, cfg, adminClient(cfg), svc, envFile, "status"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("status with daemon stopped: %v", err)
	}

	d := startFakeDaemon(t, dataDir, remote.Status{State: remote.StateRunning, Backend: remote.BackendSystem, Mode: remote.ModeTailnet, PublicURL: "https://agentflow-abc123.t.ts.net"})
	if err := remoteCommand(ctx, &out, cfg, adminClient(cfg), svc, envFile, "status"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"remote    running", "transport tailscale", "tailscale system", "mode      tailnet", "url       https://agentflow-abc123.t.ts.net"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status output lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := remoteCommand(ctx, &out, cfg, adminClient(cfg), svc, envFile, "logout"); err != nil {
		t.Fatal(err)
	}
	if d.remote.Logouts() != 1 || !strings.Contains(out.String(), "login.tailscale.com/admin/machines") {
		t.Fatalf("logouts=%d output:\n%s", d.remote.Logouts(), out.String())
	}

	if err := remoteCommand(ctx, &out, cfg, adminClient(cfg), svc, envFile, "off"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(envFile)
	if !strings.Contains(string(data), "\nAF_REMOTE=off\n") || svc.restarts != 1 {
		t.Fatalf("restarts=%d env:\n%s", svc.restarts, data)
	}
	if err := remoteCommand(ctx, &out, cfg, adminClient(cfg), svc, envFile, "on"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(envFile)
	if strings.Count(string(data), "\nAF_REMOTE=") != 1 || !strings.Contains(string(data), "\nAF_REMOTE=tailscale\n") || svc.restarts != 2 {
		t.Fatalf("restarts=%d env:\n%s", svc.restarts, data)
	}
}

func TestRemoteDoctorCheck(t *testing.T) {
	ok := func(context.Context, string) error { return nil }
	bad := func(context.Context, string) error { return errors.New("dns") }
	ctx := context.Background()
	cases := []struct {
		st     remote.Status
		probe  func(context.Context, string) error
		pass   bool
		substr string
	}{
		{remote.Status{State: remote.StateOff}, ok, true, "off"},
		{remote.Status{State: remote.StateRunning, Transport: remote.TransportTailscale, Mode: remote.ModeFunnel, PublicURL: "https://a"}, ok, true, "Tailscale funnel https://a (reachable"},
		{remote.Status{State: remote.StateRunning, Mode: remote.ModeFunnel, PublicURL: "https://a"}, bad, false, "not reachable"},
		{remote.Status{State: remote.StateNeedsLogin, AuthURL: "https://login/a"}, ok, false, "https://login/a"},
		{remote.Status{State: remote.StateNeedsFunnelApproval, ApproveURL: "https://approve"}, ok, false, "https://approve"},
		{remote.Status{State: remote.StateError, Transport: remote.TransportTailscale, Error: "boom"}, ok, false, "Tailscale error (retrying): boom"},
		{remote.Status{State: remote.StateNeedsInstall, InstallURL: remote.TailscaleInstallURL}, ok, false, "https://tailscale.com/download (or set AF_TAILSCALE=embedded)"},
		{remote.Status{State: remote.StateNeedsPermission, Command: remote.OperatorCommand}, ok, false, "run: sudo tailscale set --operator=$USER"},
		{remote.Status{State: remote.StateNeedsLogin, Command: "tailscale up"}, ok, false, "signed out or stopped; run: tailscale up"},
	}
	for _, c := range cases {
		pass, info := remoteCheck(ctx, c.st, c.probe)
		if pass != c.pass || !strings.Contains(info, c.substr) {
			t.Errorf("%+v: pass=%v info=%q", c.st, pass, info)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	env := map[string]string{"AF_DB": "/data/af.db"}
	get := func(k string) string { return env[k] }
	cfg, err := loadConfig(invocation{Name: "pair"}, get)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.remoteMode != "tailscale" || cfg.remoteTunnel != remote.ModeFunnel || cfg.addr != defaultAddr || cfg.dataDir != "/data" {
		t.Fatalf("cfg = %+v", cfg)
	}
	env["AF_REMOTE"] = "tailscal"
	env["AF_REMOTE_MODE"] = "public"
	cfg, _ = loadConfig(invocation{Name: "serve", Addr: "127.0.0.1:1", DB: "/x/y.db"}, get)
	if cfg.remoteMode != "off" || cfg.remoteTunnel != remote.ModeTailnet {
		t.Fatalf("typos must fail closed: %+v", cfg)
	}
	if cfg.addr != "127.0.0.1:1" || cfg.dbPath != "/x/y.db" || cfg.dataDir != "/x" {
		t.Fatalf("serve flags ignored: %+v", cfg)
	}
	if cfg.transport() != "" || cfg.remoteOn() {
		t.Fatalf("off has no transport: %+v", cfg)
	}
	if !cfg.tsEmbedded {
		t.Fatal("the embedded Tailscale node is the default")
	}
	env["AF_TAILSCALE"] = "embedded"
	if cfg, _ = loadConfig(invocation{Name: "pair"}, get); !cfg.tsEmbedded {
		t.Fatal("AF_TAILSCALE=embedded ignored")
	}
	env["AF_TAILSCALE"] = "system"
	if cfg, _ = loadConfig(invocation{Name: "pair"}, get); cfg.tsEmbedded {
		t.Fatal("AF_TAILSCALE=system must use the installed Tailscale, not the embedded node")
	}
	env["AF_TAILSCALE"] = "sytem"
	if cfg, _ = loadConfig(invocation{Name: "pair"}, get); !cfg.tsEmbedded {
		t.Fatal("a typo must fall back to the embedded (default) node")
	}
}

func TestNewRemoteUsesTheSystemTailscaleUnlessEmbedded(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for embedded, want := range map[bool]string{false: remote.BackendSystem, true: remote.BackendEmbedded} {
		cfg := testConfig(t.TempDir())
		cfg.tsEmbedded = embedded
		rem := newRemote(cfg, agentapi.New(nil, nil, log, "m"), log)
		if got := rem.Status().Backend; got != want {
			t.Errorf("embedded=%v: backend %q, want %q", embedded, got, want)
		}
		_ = rem.Close()
	}
}

func TestNewRemoteFollowsTheConfiguredTransport(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for mode, want := range map[string]string{"off": "", "tailscale": remote.TransportTailscale} {
		cfg := testConfig(t.TempDir())
		cfg.remoteMode = mode
		rem := newRemote(cfg, agentapi.New(nil, nil, log, "m"), log)
		if got := rem.Status().Transport; got != want {
			t.Errorf("AF_REMOTE=%s: transport %q, want %q", mode, got, want)
		}
		st, err := remote.ReadStatusFile(filepath.Join(cfg.dataDir, remote.StatusFileName))
		if err != nil || st.Transport != want {
			t.Errorf("AF_REMOTE=%s: remote.json = %+v %v", mode, st, err)
		}
		_ = rem.Close()
	}
}

func TestTunnelAddrMustBeItsOwnLoopbackPort(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:4345":  "127.0.0.1:4345",
		"[::1]:9000":      "[::1]:9000",
		"127.0.0.1:4344":  "", // the local listener
		"0.0.0.0:4345":    "", // reachable from the network
		"example.com:443": "",
		"4345":            "",
	} {
		cfg, err := loadConfig(invocation{Name: "serve", Addr: defaultAddr}, func(k string) string {
			return map[string]string{"AF_DB": "/d/af.db", "AF_TUNNEL_ADDR": addr}[k]
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.tunnelAddr != want {
			t.Errorf("AF_TUNNEL_ADDR=%q: tunnelAddr = %q, want %q", addr, cfg.tunnelAddr, want)
		}
	}
}

func TestAddrIsNonLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:4344": false, "[::1]:4344": false, "localhost:4344": false,
		":4344": true, "0.0.0.0:4344": true, "192.168.1.2:4344": true, "example.com:1": true,
	}
	for addr, want := range cases {
		if got := addrIsNonLoopback(addr); got != want {
			t.Errorf("%s: got %v", addr, got)
		}
	}
}
