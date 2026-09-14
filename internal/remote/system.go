package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

// The system transport publishes the daemon through the Tailscale already
// installed and signed in on this computer, instead of starting a node of its
// own. tailscaled terminates HTTPS for this machine's name on port 8443 and
// forwards to a loopback listener agentflow opens for it (Funnel makes that
// port public; in tailnet mode it stays inside the tailnet).
//
// The node's serve config belongs to the person: agentflow merges one entry
// into it, never replaces or removes anything else, and takes its own entry
// out again on a clean shutdown.

// SystemServePort is the HTTPS port agentflow serves on through the system
// Tailscale. 443 is left for the person's own `tailscale serve`; Funnel
// allows 443, 8443 and 10000.
const SystemServePort = 8443

// OperatorCommand lets a non-root Linux user change tailscaled's serve
// config.
const OperatorCommand = "sudo tailscale set --operator=$USER"

// serveRecordName is the file, in the data directory, that remembers the
// serve entry agentflow added, so a later start recognizes it as its own
// (after a crash, with a different tunnel port) rather than as a conflict.
const serveRecordName = "tailscale-serve.json"

// localAPI is the part of tailscaled's LocalAPI the system transport uses.
// *local.Client implements it; tests use a fake.
type localAPI interface {
	StatusWithoutPeers(ctx context.Context) (*ipnstate.Status, error)
	QueryFeature(ctx context.Context, feature string) (*tailcfg.QueryFeatureResponse, error)
	GetServeConfig(ctx context.Context) (*ipn.ServeConfig, error)
	SetServeConfig(ctx context.Context, config *ipn.ServeConfig) error
}

// accessDenied and preconditionFailed classify LocalAPI errors; tests swap
// them for their own error types.
var (
	accessDenied       = local.IsAccessDeniedError
	preconditionFailed = local.IsPreconditionsFailedError
)

type systemTimings struct {
	poll       time.Duration // status while waiting or serving
	retry      time.Duration // install and permission checks
	minBackoff time.Duration
	maxBackoff time.Duration
	shutdown   time.Duration
}

var defaultSystemTimings = systemTimings{
	poll:       2 * time.Second,
	retry:      5 * time.Second,
	minBackoff: time.Second,
	maxBackoff: time.Minute,
	shutdown:   5 * time.Second,
}

type systemTailscale struct {
	cfg      Config
	log      *slog.Logger
	bus      *statusBus
	tm       systemTimings
	lc       localAPI
	lookPath func(string) (string, error)

	mu      sync.Mutex
	closed  bool
	cancel  context.CancelFunc
	ln      net.Listener
	srv     *http.Server
	entry   *serveRecord // the entry currently in the serve config
	stopped chan struct{}
}

// NewSystem returns the transport that uses this computer's Tailscale.
func NewSystem(cfg Config) Transport {
	return newSystemTailscale(cfg, defaultSystemTimings, &local.Client{}, exec.LookPath)
}

func newSystemTailscale(cfg Config, tm systemTimings, lc localAPI, lookPath func(string) (string, error)) *systemTailscale {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeFunnel
	}
	s := &systemTailscale{cfg: cfg, log: cfg.Log, tm: tm, lc: lc, lookPath: lookPath, stopped: make(chan struct{})}
	s.bus = newStatusBus(s.stamp(Status{State: StateStarting}), statusFileMirror(cfg.DataDir, cfg.Log))
	return s
}

func (s *systemTailscale) stamp(st Status) Status {
	st.Transport = TransportTailscale
	st.Backend = BackendSystem
	st.Mode = s.cfg.Mode
	return st
}

func (s *systemTailscale) publish(st Status) {
	st = s.stamp(st)
	st.UpdatedAt = time.Now()
	s.bus.publish(st)
}

func (s *systemTailscale) Status() Status                     { return s.bus.snapshot() }
func (s *systemTailscale) Subscribe() (<-chan Status, func()) { return s.bus.subscribe() }

// Logout refuses: signing this computer's Tailscale out would cut off every
// other use of it, which isn't agentflow's to decide.
func (s *systemTailscale) Logout(context.Context) error {
	return errors.New("agentflow uses this computer's Tailscale; sign it out with `tailscale logout` if that is what you want")
}

// errWait ends an attempt that published what it is waiting for; the run
// loop starts over after the attempt's own pause, without backing off.
var errWait = errors.New("remote: waiting")

// Run keeps the daemon published until ctx is done or Close is called.
func (s *systemTailscale) Run(ctx context.Context, h http.Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return nil
	}
	s.cancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.stop()
	}()

	backoff := s.tm.minBackoff
	for {
		err := s.attempt(ctx, h)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case err == nil, errors.Is(err, errWait):
			backoff = s.tm.minBackoff
			continue
		}
		s.log.Warn("remote: retrying", "err", err, "in", backoff)
		s.publish(Status{State: StateError, Error: err.Error()})
		if sleepCtx(ctx, backoff) != nil {
			return nil
		}
		backoff = nextBackoff(backoff, s.tm.maxBackoff)
	}
}

// attempt walks detection, sign-in, approval and serve config, then keeps
// the entry in place while the node stays up.
func (s *systemTailscale) attempt(ctx context.Context, h http.Handler) error {
	st, err := s.detect(ctx)
	if err != nil {
		return err
	}
	host := strings.TrimSuffix(st.Self.DNSName, ".")
	if err := s.ensureAllowed(ctx, st); err != nil {
		return err
	}
	ln, err := s.listener(h)
	if err != nil {
		return err
	}
	hp := ipn.HostPort(net.JoinHostPort(host, strconv.Itoa(SystemServePort)))
	want := serveRecord{HostPort: hp, Proxy: "http://" + ln.Addr().String(), Funnel: s.cfg.Mode == ModeFunnel}
	if err := s.applyServe(ctx, want); err != nil {
		return err
	}
	publicURL := "https://" + string(hp)
	s.publish(Status{State: StateRunning, PublicURL: publicURL})
	s.log.Info("remote: serving through the system Tailscale", "url", publicURL, "mode", s.cfg.Mode)
	return s.watch(ctx, host, want)
}

// detect waits for an installed, reachable, signed-in Tailscale and returns
// its status. Every state it can't get past is published and ends the
// attempt with errWait after a pause.
func (s *systemTailscale) detect(ctx context.Context) (*ipnstate.Status, error) {
	if _, err := s.lookPath("tailscale"); err != nil {
		s.publish(Status{State: StateNeedsInstall, InstallURL: TailscaleInstallURL})
		return nil, s.pause(ctx, s.tm.retry)
	}
	st, err := s.lc.StatusWithoutPeers(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.publish(Status{State: StateNeedsInstall, InstallURL: TailscaleInstallURL,
			Error: "Tailscale is installed but not answering: " + err.Error()})
		return nil, s.pause(ctx, s.tm.retry)
	}
	switch st.BackendState {
	case ipn.Running.String():
		if st.Self == nil || strings.TrimSuffix(st.Self.DNSName, ".") == "" {
			s.publish(Status{State: StateStarting})
			return nil, s.pause(ctx, s.tm.poll)
		}
		return st, nil
	case ipn.NeedsLogin.String():
		if st.AuthURL != "" {
			s.publish(Status{State: StateNeedsLogin, AuthURL: st.AuthURL})
		} else {
			s.publish(Status{State: StateNeedsLogin, Command: "tailscale up"})
		}
	case ipn.NeedsMachineAuth.String():
		s.publish(Status{State: StateNeedsLogin, AuthURL: MachineAuthURL})
	case ipn.Stopped.String():
		s.publish(Status{State: StateNeedsLogin, Command: "tailscale up"})
	default:
		s.publish(Status{State: StateStarting})
	}
	return nil, s.pause(ctx, s.tm.poll)
}

// ensureAllowed checks that the tailnet lets this node serve HTTPS (and
// Funnel, in funnel mode), publishing the approval link while it doesn't.
func (s *systemTailscale) ensureAllowed(ctx context.Context, st *ipnstate.Status) error {
	ready := st.Self.HasCap(tailcfg.CapabilityHTTPS) && len(st.CertDomains) > 0
	feature := "serve"
	if s.cfg.Mode == ModeFunnel {
		ready = funnelReady(st)
		feature = "funnel"
	}
	if ready {
		return nil
	}
	qf, err := s.lc.QueryFeature(ctx, feature)
	if err != nil {
		return fmt.Errorf("query %s: %w", feature, err)
	}
	switch {
	case qf.URL != "":
		s.publish(Status{State: StateNeedsFunnelApproval, ApproveURL: qf.URL})
	case qf.Complete:
		// Approved; the node's netmap hasn't caught up yet.
		if s.Status().State != StateNeedsFunnelApproval {
			s.publish(Status{State: StateStarting})
		}
	default:
		msg := strings.TrimSpace(qf.Text)
		if msg == "" {
			msg = "Tailscale does not allow " + feature + " for this machine and gave no approval link"
		}
		s.publish(Status{State: StateError, Error: msg})
	}
	return s.pause(ctx, s.tm.retry)
}

// listener returns the tunnel listener, opening it and starting to serve h
// on it the first time. It stays open across attempts so the serve entry's
// port stays valid.
func (s *systemTailscale) listener(h http.Handler) (net.Listener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("remote: closed")
	}
	if s.ln != nil {
		return s.ln, nil
	}
	addr := s.cfg.TunnelAddr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := ListenTunnel(addr, s.cfg.LocalAddr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		// tailscaled's serve proxy sets X-Forwarded-For to the real client
		// on every request, replacing whatever the client sent.
		Handler:           RequireForwardedClient(h, LastForwardedFor),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	s.ln, s.srv = ln, srv
	go func() { _ = srv.Serve(ln) }()
	return ln, nil
}

// watch keeps the entry in place while the node stays up under the same
// name. It returns nil to start a new attempt when anything changes.
func (s *systemTailscale) watch(ctx context.Context, host string, want serveRecord) error {
	for {
		if err := sleepCtx(ctx, s.tm.poll); err != nil {
			return err
		}
		st, err := s.lc.StatusWithoutPeers(ctx)
		if err != nil || st.BackendState != ipn.Running.String() || st.Self == nil ||
			strings.TrimSuffix(st.Self.DNSName, ".") != host {
			return nil
		}
		// Someone may have reset the serve config; put the entry back.
		if err := s.applyServe(ctx, want); err != nil {
			return err
		}
	}
}

// pause publishes nothing and waits d, ending the attempt with errWait.
func (s *systemTailscale) pause(ctx context.Context, d time.Duration) error {
	if err := sleepCtx(ctx, d); err != nil {
		return err
	}
	return errWait
}

// serveRecord is the entry agentflow adds to the serve config.
type serveRecord struct {
	HostPort ipn.HostPort `json:"hostPort"`
	Proxy    string       `json:"proxy"`
	Funnel   bool         `json:"funnel"`
}

func (s *systemTailscale) recordPath() string {
	if s.cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.DataDir, serveRecordName)
}

func (s *systemTailscale) loadRecord() *serveRecord {
	path := s.recordPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // our own file in the data directory
	if err != nil {
		return nil
	}
	var r serveRecord
	if json.Unmarshal(data, &r) != nil || r.HostPort == "" {
		return nil
	}
	return &r
}

func (s *systemTailscale) saveRecord(r *serveRecord) {
	path := s.recordPath()
	if path == "" {
		return
	}
	if r == nil {
		_ = os.Remove(path)
		return
	}
	data, _ := json.Marshal(r)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		_ = os.WriteFile(path, data, 0o600)
	}
}

// applyServe makes the serve config contain want, and no older entry of
// agentflow's, touching nothing else.
func (s *systemTailscale) applyServe(ctx context.Context, want serveRecord) error {
	for range 5 {
		sc, err := s.lc.GetServeConfig(ctx)
		if err != nil {
			return s.serveError(ctx, "read", err)
		}
		changed, err := mergeServeEntry(sc, want, s.ownEntries(want))
		if err != nil {
			return err
		}
		if !changed {
			s.remember(&want)
			return nil
		}
		err = s.lc.SetServeConfig(ctx, sc)
		if err == nil {
			s.remember(&want)
			return nil
		}
		if !preconditionFailed(err) {
			return s.serveError(ctx, "update", err)
		}
		// Changed underneath us; read it again.
	}
	return errors.New("the Tailscale serve config kept changing while agentflow was updating it")
}

// ownEntries are the entries agentflow may replace: the one it wants, the
// one it set earlier in this run, and the one a previous run recorded.
func (s *systemTailscale) ownEntries(want serveRecord) []serveRecord {
	own := []serveRecord{want}
	s.mu.Lock()
	if s.entry != nil {
		own = append(own, *s.entry)
	}
	s.mu.Unlock()
	if r := s.loadRecord(); r != nil {
		own = append(own, *r)
	}
	return own
}

func (s *systemTailscale) remember(r *serveRecord) {
	s.mu.Lock()
	same := s.entry != nil && r != nil && *s.entry == *r
	if r == nil {
		s.entry = nil
	} else {
		cp := *r
		s.entry = &cp
	}
	s.mu.Unlock()
	if !same {
		s.saveRecord(r)
	}
}

// serveError turns a LocalAPI failure into the state that explains it. A
// permission problem is waited out: whoever fixes it shouldn't also have to
// restart agentflow.
func (s *systemTailscale) serveError(ctx context.Context, what string, err error) error {
	if accessDenied(err) {
		s.publish(Status{State: StateNeedsPermission, Command: OperatorCommand,
			Error: "this user may not change Tailscale's serve settings"})
		return s.pause(ctx, s.tm.retry)
	}
	return fmt.Errorf("%s the Tailscale serve config: %w", what, err)
}

// errServePortTaken means something other than agentflow serves the port.
var errServePortTaken = fmt.Errorf("port %d on this computer's Tailscale is already served by something else (see `tailscale serve status`); agentflow leaves it alone", SystemServePort)

// mergeServeEntry adds want to sc, replacing any of own (agentflow's entries)
// that are there, and reports whether sc changed. Anything else already
// using the port is a conflict and sc is left as it was.
func mergeServeEntry(sc *ipn.ServeConfig, want serveRecord, own []serveRecord) (bool, error) {
	isOwn := func(hp ipn.HostPort, web *ipn.WebServerConfig) bool {
		for _, r := range own {
			if r.HostPort == hp && webIsProxy(web, r.Proxy) {
				return true
			}
		}
		return false
	}
	port := uint16(SystemServePort)
	var stale []ipn.HostPort
	for hp, web := range sc.Web {
		if p, err := hp.Port(); err != nil || p != port {
			continue
		}
		if !isOwn(hp, web) {
			return false, errServePortTaken
		}
		if hp != want.HostPort || !webIsProxy(web, want.Proxy) {
			stale = append(stale, hp)
		}
	}
	if tcp := sc.TCP[port]; tcp != nil && (!tcp.HTTPS || tcp.HTTP || tcp.TCPForward != "") {
		return false, errServePortTaken
	}

	before := cloneServeConfig(sc)
	for _, hp := range stale {
		delete(sc.Web, hp)
		delete(sc.AllowFunnel, hp)
	}
	if sc.TCP == nil {
		sc.TCP = map[uint16]*ipn.TCPPortHandler{}
	}
	sc.TCP[port] = &ipn.TCPPortHandler{HTTPS: true}
	if sc.Web == nil {
		sc.Web = map[ipn.HostPort]*ipn.WebServerConfig{}
	}
	sc.Web[want.HostPort] = &ipn.WebServerConfig{Handlers: map[string]*ipn.HTTPHandler{"/": {Proxy: want.Proxy}}}
	if want.Funnel {
		if sc.AllowFunnel == nil {
			sc.AllowFunnel = map[ipn.HostPort]bool{}
		}
		sc.AllowFunnel[want.HostPort] = true
	} else {
		delete(sc.AllowFunnel, want.HostPort)
	}
	return !sameServeConfig(before, sc), nil
}

// removeServeEntry takes entry out of sc if it is still exactly agentflow's,
// and reports whether sc changed.
func removeServeEntry(sc *ipn.ServeConfig, entry serveRecord) bool {
	web, ok := sc.Web[entry.HostPort]
	if !ok || !webIsProxy(web, entry.Proxy) {
		return false
	}
	delete(sc.Web, entry.HostPort)
	delete(sc.AllowFunnel, entry.HostPort)
	port := uint16(SystemServePort)
	for hp := range sc.Web {
		if p, err := hp.Port(); err == nil && p == port {
			return true // still in use by another name
		}
	}
	delete(sc.TCP, port)
	return true
}

func webIsProxy(web *ipn.WebServerConfig, proxy string) bool {
	if web == nil || len(web.Handlers) != 1 {
		return false
	}
	h := web.Handlers["/"]
	return h != nil && h.Proxy == proxy && h.Path == "" && h.Text == "" && h.Redirect == ""
}

func cloneServeConfig(sc *ipn.ServeConfig) *ipn.ServeConfig {
	data, _ := json.Marshal(sc)
	out := &ipn.ServeConfig{}
	_ = json.Unmarshal(data, out)
	out.ETag = sc.ETag
	return out
}

func sameServeConfig(a, b *ipn.ServeConfig) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// Close stops serving and takes agentflow's entry out of the serve config.
func (s *systemTailscale) Close() error {
	s.mu.Lock()
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.stop()
	return nil
}

func (s *systemTailscale) stop() {
	s.mu.Lock()
	select {
	case <-s.stopped:
		s.mu.Unlock()
		return
	default:
	}
	close(s.stopped)
	s.closed = true
	srv, entry := s.srv, s.entry
	s.srv, s.ln = nil, nil
	s.mu.Unlock()

	if entry != nil {
		ctx, cancel := context.WithTimeout(context.Background(), s.tm.shutdown)
		if err := s.removeServe(ctx, *entry); err != nil {
			s.log.Warn("remote: could not remove agentflow's entry from the Tailscale serve config", "err", err)
		} else {
			s.remember(nil)
		}
		cancel()
	}
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), s.tm.shutdown)
		_ = srv.Shutdown(ctx)
		cancel()
	}
	if s.cfg.CloseHijacked != nil {
		s.cfg.CloseHijacked()
	}
	s.bus.close()
}

func (s *systemTailscale) removeServe(ctx context.Context, entry serveRecord) error {
	for range 5 {
		sc, err := s.lc.GetServeConfig(ctx)
		if err != nil {
			return err
		}
		if !removeServeEntry(sc, entry) {
			return nil
		}
		err = s.lc.SetServeConfig(ctx, sc)
		if err == nil || !preconditionFailed(err) {
			return err
		}
	}
	return errors.New("the Tailscale serve config kept changing")
}
