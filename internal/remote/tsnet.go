package remote

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// Config configures the Tailscale remote.
type Config struct {
	// DataDir is the daemon's data directory. Node state lives in
	// DataDir/tailscale and the status mirror in DataDir/remote.json.
	DataDir string
	// Mode is funnel (default) or tailnet.
	Mode Mode
	// Hostname overrides the generated, persisted node name.
	Hostname string
	// LogsOn allows Tailscale client log upload (AF_TS_LOGS=on).
	LogsOn bool
	// Embedded runs agentflow's own tsnet node (AF_TAILSCALE=embedded)
	// instead of using the Tailscale installed on this computer.
	Embedded bool
	// TunnelAddr is where the system Tailscale forwards public requests
	// (AF_TUNNEL_ADDR); "" picks a free loopback port.
	TunnelAddr string
	// LocalAddr is the local listener's address, which the tunnel listener
	// must never be.
	LocalAddr string
	// CloseHijacked closes connections the HTTP server no longer tracks
	// (WebSockets) on Close, after the server has shut down.
	CloseHijacked func()
	Log           *slog.Logger
}

// node is the part of a tsnet server the run loop uses. It exists so the loop
// (login wait, Funnel approval, retries, logout) can be tested without a
// Tailscale account.
type node interface {
	Start() error
	Status(ctx context.Context) (*ipnstate.Status, error)
	QueryFunnel(ctx context.Context) (*tailcfg.QueryFeatureResponse, error)
	StartLogin(ctx context.Context) error
	Logout(ctx context.Context) error
	CertDomains() []string
	PrewarmCert(ctx context.Context, domain string) error
	Listen(mode Mode) (net.Listener, error)
	Close() error
}

// timings are the run loop's intervals; tests shrink them.
type timings struct {
	poll       time.Duration // login / running wait
	funnelPoll time.Duration // Funnel approval wait
	minBackoff time.Duration
	maxBackoff time.Duration
	shutdown   time.Duration // graceful HTTP shutdown budget
	loginNudge int           // polls of NeedsLogin without a URL before asking for one
}

var defaultTimings = timings{
	poll:       time.Second,
	funnelPoll: 3 * time.Second,
	minBackoff: time.Second,
	maxBackoff: 60 * time.Second,
	shutdown:   10 * time.Second,
	loginNudge: 5,
}

// tsnetRemote runs remote access over an embedded tsnet node.
type tsnetRemote struct {
	cfg     Config
	log     *slog.Logger
	bus     *statusBus
	tm      timings
	newNode func(hostname, dir string) node
	setenv  func(key, value string) error

	mu       sync.Mutex
	node     node
	httpSrv  *http.Server
	cancel   context.CancelFunc
	closed   bool
	relogin  chan struct{}
	stopOnce sync.Once
}

// errRelogin ends a serving attempt after Logout so the loop waits for a new
// login instead of backing off.
var errRelogin = errors.New("remote: logged out")

// New returns the Tailscale remote. It does no I/O until Run.
func New(cfg Config) Transport {
	return newTSNetRemote(cfg, defaultTimings, func(hostname, dir string) node {
		return newTSNetNode(hostname, dir, cfg.Log)
	})
}

func newTSNetRemote(cfg Config, tm timings, newNode func(hostname, dir string) node) *tsnetRemote {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeFunnel
	}
	t := &tsnetRemote{
		cfg:     cfg,
		log:     cfg.Log,
		tm:      tm,
		newNode: newNode,
		setenv:  os.Setenv,
		relogin: make(chan struct{}, 1),
	}
	t.bus = newStatusBus(Status{State: StateStarting, Transport: TransportTailscale, Backend: BackendEmbedded, Mode: cfg.Mode},
		statusFileMirror(cfg.DataDir, t.log))
	return t
}

func (t *tsnetRemote) Status() Status                     { return t.bus.snapshot() }
func (t *tsnetRemote) Subscribe() (<-chan Status, func()) { return t.bus.subscribe() }

func (t *tsnetRemote) publish(st Status) {
	st.Transport = TransportTailscale
	st.Backend = BackendEmbedded
	st.Mode = t.cfg.Mode
	st.UpdatedAt = time.Now()
	t.bus.publish(st)
}

// Run keeps remote access up until ctx is done or Close is called. No failure
// ends it: a node that cannot start, a Funnel check that lags behind an
// approval, a listener that dies all publish "error" and retry with capped
// exponential backoff. The daemon's local listener never depends on this.
func (t *tsnetRemote) Run(ctx context.Context, h http.Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		cancel()
		return nil
	}
	t.cancel = cancel
	t.mu.Unlock()
	defer func() {
		cancel()
		t.stop()
	}()

	backoff := t.tm.minBackoff
	for {
		err := t.attempt(ctx, h)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errRelogin) {
			backoff = t.tm.minBackoff
			continue
		}
		if err == nil {
			// A serving attempt that ended cleanly (listener closed under
			// us) restarts from the top without penalty.
			backoff = t.tm.minBackoff
			continue
		}
		t.log.Warn("remote: retrying", "err", err, "in", backoff)
		t.publish(Status{State: StateError, Error: err.Error()})
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff, t.tm.maxBackoff)
	}
}

// attempt runs the start → login → funnel → listen → serve sequence once.
func (t *tsnetRemote) attempt(ctx context.Context, h http.Handler) error {
	n, err := t.ensureNode()
	if err != nil {
		return err
	}
	if err := t.awaitRunning(ctx, n); err != nil {
		return err
	}
	if t.cfg.Mode == ModeFunnel {
		if err := t.ensureFunnel(ctx, n); err != nil {
			return err
		}
	}
	domains := n.CertDomains()
	if len(domains) == 0 {
		// Tailnet mode needs HTTPS certificates too; Funnel mode already
		// checked, but the netmap can change between the two reads.
		return errors.New("this tailnet has no HTTPS certificate domain for the node yet (enable HTTPS in the Tailscale admin console)")
	}
	ln, err := n.Listen(t.cfg.Mode)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	// Issue the certificate now rather than on the first phone's handshake:
	// a brand-new certificate can take a while and briefly trip browsers'
	// Certificate Transparency checks.
	go func() {
		if err := n.PrewarmCert(ctx, domains[0]); err != nil && ctx.Err() == nil {
			t.log.Debug("remote: certificate pre-warm", "domain", domains[0], "err", err)
		}
	}()
	return t.serve(ctx, h, ln, "https://"+domains[0])
}

// ensureNode returns the running node, creating and starting one if needed.
// A node whose Start failed is discarded: tsnet caches the first Start error
// for the life of the server.
func (t *tsnetRemote) ensureNode() (node, error) {
	t.mu.Lock()
	n := t.node
	t.mu.Unlock()
	if n != nil {
		return n, nil
	}
	if err := applyLogPolicy(t.cfg.LogsOn, t.setenv); err != nil {
		return nil, fmt.Errorf("disable Tailscale log upload: %w", err)
	}
	dir := filepath.Join(t.cfg.DataDir, "tailscale")
	if err := ensurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("tailscale state dir: %w", err)
	}
	hostname := t.cfg.Hostname
	if hostname == "" {
		h, err := loadOrCreateHostname(dir)
		if err != nil {
			return nil, fmt.Errorf("hostname: %w", err)
		}
		hostname = h
	} else if !ValidHostname(hostname) {
		return nil, fmt.Errorf("AF_TS_HOSTNAME=%q is not a valid hostname (lowercase letters, digits and dashes)", hostname)
	}
	t.publish(Status{State: StateStarting})
	n = t.newNode(hostname, dir)
	if err := n.Start(); err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("start tailscale: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = n.Close()
		return nil, errors.New("remote: closed")
	}
	t.node = n
	return n, nil
}

// awaitRunning waits for the node to be logged in. Up() is never used: it
// blocks forever while the node needs a login.
func (t *tsnetRemote) awaitRunning(ctx context.Context, n node) error {
	noURL := 0
	for {
		st, err := n.Status(ctx)
		if err != nil {
			return fmt.Errorf("tailscale status: %w", err)
		}
		switch st.BackendState {
		case ipn.Running.String():
			return nil
		case ipn.NeedsLogin.String():
			if st.AuthURL != "" {
				noURL = 0
				t.publish(Status{State: StateNeedsLogin, AuthURL: st.AuthURL})
			} else {
				// tsnet asks for a login URL when it starts, but not after
				// a logout; nudge it if none shows up.
				noURL++
				if noURL == t.tm.loginNudge {
					if err := n.StartLogin(ctx); err != nil && ctx.Err() == nil {
						t.log.Debug("remote: start login", "err", err)
					}
					noURL = 0
				}
				if cur := t.Status(); cur.State != StateNeedsLogin {
					t.publish(Status{State: StateStarting})
				}
			}
		case ipn.NeedsMachineAuth.String():
			t.publish(Status{State: StateNeedsLogin, AuthURL: MachineAuthURL})
		default:
			if cur := t.Status(); cur.State != StateNeedsLogin {
				t.publish(Status{State: StateStarting})
			}
		}
		if err := sleepCtx(ctx, t.tm.poll); err != nil {
			return err
		}
	}
}

// funnelReady reports whether the node can serve Funnel: HTTPS certificates
// and the funnel attribute granted, and a certificate domain assigned.
func funnelReady(st *ipnstate.Status) bool {
	return st != nil && st.Self != nil &&
		st.Self.HasCap(tailcfg.CapabilityHTTPS) &&
		st.Self.HasCap(tailcfg.NodeAttrFunnel) &&
		len(st.CertDomains) > 0
}

// ensureFunnel waits until the tailnet allows Funnel for this node, publishing
// the one-click approval URL while it does not.
func (t *tsnetRemote) ensureFunnel(ctx context.Context, n node) error {
	for {
		st, err := n.Status(ctx)
		if err != nil {
			return fmt.Errorf("tailscale status: %w", err)
		}
		if st.BackendState != ipn.Running.String() {
			return fmt.Errorf("tailscale is %s, not running", st.BackendState)
		}
		if funnelReady(st) {
			return nil
		}
		qf, err := n.QueryFunnel(ctx)
		if err != nil {
			return fmt.Errorf("query funnel: %w", err)
		}
		switch {
		case qf.URL != "":
			t.publish(Status{State: StateNeedsFunnelApproval, ApproveURL: qf.URL})
		case qf.Complete:
			// Approved, but this node's netmap has not caught up yet.
			if cur := t.Status(); cur.State != StateNeedsFunnelApproval {
				t.publish(Status{State: StateStarting})
			}
		default:
			msg := strings.TrimSpace(qf.Text)
			if msg == "" {
				msg = "Funnel is not available for this node and Tailscale gave no approval link"
			}
			t.publish(Status{State: StateError, Error: msg})
		}
		if err := sleepCtx(ctx, t.tm.funnelPoll); err != nil {
			return err
		}
	}
}

// serve serves h on ln until ctx is done, the listener fails or Logout asks
// for a new login.
func (t *tsnetRemote) serve(ctx context.Context, h http.Handler, ln net.Listener, publicURL string) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ConnContext:       connContext,
		ErrorLog:          slog.NewLogLogger(t.log.Handler(), slog.LevelDebug),
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = ln.Close()
		return errors.New("remote: closed")
	}
	t.httpSrv = srv
	// Drop a stale logout signal from before this listener existed.
	select {
	case <-t.relogin:
	default:
	}
	t.mu.Unlock()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	t.publish(Status{State: StateRunning, PublicURL: publicURL})
	t.log.Info("remote: serving", "url", publicURL, "mode", t.cfg.Mode)

	select {
	case <-ctx.Done():
		return nil
	case <-t.relogin:
		t.detachServer(srv)
		return errRelogin
	case err := <-errCh:
		t.detachServer(srv)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	}
}

// detachServer shuts srv down after the serving attempt ended for a reason
// other than daemon shutdown.
func (t *tsnetRemote) detachServer(srv *http.Server) {
	t.mu.Lock()
	if t.httpSrv == srv {
		t.httpSrv = nil
	}
	t.mu.Unlock()
	sctx, cancel := context.WithTimeout(context.Background(), t.tm.shutdown)
	defer cancel()
	_ = srv.Shutdown(sctx)
}

// Logout logs the node out and sends the run loop back to waiting for a login,
// which publishes needs_login with the new URL.
func (t *tsnetRemote) Logout(ctx context.Context) error {
	t.mu.Lock()
	n := t.node
	t.mu.Unlock()
	if n == nil {
		return errors.New("remote access has not started yet")
	}
	if err := n.Logout(ctx); err != nil {
		return fmt.Errorf("tailscale logout: %w", err)
	}
	// A logged-out tsnet node does not ask for a login URL on its own.
	if err := n.StartLogin(ctx); err != nil {
		t.log.Debug("remote: start login after logout", "err", err)
	}
	t.publish(Status{State: StateNeedsLogin})
	select {
	case t.relogin <- struct{}{}:
	default:
	}
	return nil
}

// Close stops the run loop, shuts the HTTP server down (10 s), closes
// hijacked connections and closes the node.
func (t *tsnetRemote) Close() error {
	t.mu.Lock()
	t.closed = true
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.stop()
	return nil
}

func (t *tsnetRemote) stop() {
	t.stopOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		srv, n := t.httpSrv, t.node
		t.httpSrv, t.node = nil, nil
		t.mu.Unlock()
		if srv != nil {
			sctx, cancel := context.WithTimeout(context.Background(), t.tm.shutdown)
			_ = srv.Shutdown(sctx)
			cancel()
		}
		if t.cfg.CloseHijacked != nil {
			t.cfg.CloseHijacked()
		}
		if n != nil {
			_ = n.Close()
		}
		t.bus.close()
	})
}

// nextBackoff doubles d, capped at limit.
func nextBackoff(d, limit time.Duration) time.Duration {
	d *= 2
	if d > limit {
		return limit
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// tsnetNode is the production node.
type tsnetNode struct {
	srv *tsnet.Server
	lc  *local.Client
}

// startupLine is tsnet's reminder, repeated every few seconds while the node
// needs a login. The CLI shows the login URL itself, so it is noise here.
const startupLine = "To start this tsnet server"

func newTSNetNode(hostname, dir string, log *slog.Logger) node {
	if log == nil {
		log = slog.Default()
	}
	return &tsnetNode{srv: &tsnet.Server{
		Hostname: hostname,
		Dir:      dir,
		UserLogf: userLogf(log),
		// Logf stays nil: tsnet discards its verbose backend logs.
	}}
}

// userLogf routes tsnet's user-facing messages to debug logging, minus the
// repeating login reminder.
func userLogf(log *slog.Logger) func(format string, args ...any) {
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if strings.Contains(msg, startupLine) {
			return
		}
		log.Debug("tsnet", "msg", msg)
	}
}

func (n *tsnetNode) Start() error {
	if err := n.srv.Start(); err != nil {
		return err
	}
	lc, err := n.srv.LocalClient()
	if err != nil {
		return err
	}
	n.lc = lc
	return nil
}

func (n *tsnetNode) Status(ctx context.Context) (*ipnstate.Status, error) {
	return n.lc.StatusWithoutPeers(ctx)
}

func (n *tsnetNode) QueryFunnel(ctx context.Context) (*tailcfg.QueryFeatureResponse, error) {
	return n.lc.QueryFeature(ctx, "funnel")
}

func (n *tsnetNode) StartLogin(ctx context.Context) error { return n.lc.StartLoginInteractive(ctx) }
func (n *tsnetNode) Logout(ctx context.Context) error     { return n.lc.Logout(ctx) }
func (n *tsnetNode) CertDomains() []string                { return n.srv.CertDomains() }

func (n *tsnetNode) PrewarmCert(ctx context.Context, domain string) error {
	_, _, err := n.lc.CertPair(ctx, domain)
	return err
}

func (n *tsnetNode) Listen(mode Mode) (net.Listener, error) {
	if mode == ModeTailnet {
		return n.srv.ListenTLS("tcp", ":443")
	}
	// No FunnelOnly option: the listener also accepts tailnet peers, so
	// people with the Tailscale app reach the same URL.
	return n.srv.ListenFunnel("tcp", ":443", tsnet.FunnelTLSConfig(&tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: n.lc.GetCertificate,
	}))
}

func (n *tsnetNode) Close() error { return n.srv.Close() }
