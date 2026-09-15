package remote

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/remote/cloudflared"
)

// The Cloudflare transport (AF_REMOTE=cloudflare, the default) gives the
// machine a stable https://<slug>.useagentflow.xyz address through a
// Cloudflare named tunnel that the agentflow account service created for it.
// The daemon gets the hostname and tunnel token from the service (keeping the
// last good pair, so a service outage doesn't take the tunnel down), runs a
// verified cloudflared with the token, and serves the public root on its own
// loopback tunnel listener, where the tunnel's ingress points.

// DefaultCloudflareTunnelAddr is where cloudflared forwards public requests.
// The account service configures every tunnel's ingress to this address, so
// it is fixed; AF_TUNNEL_ADDR overrides it only for a tunnel configured to
// match.
const DefaultCloudflareTunnelAddr = "127.0.0.1:4345"

// TunnelGrant is the hostname and token the account service hands out.
type TunnelGrant struct {
	Hostname string
	Token    string
}

// CloudflareConfig configures the Cloudflare transport.
type CloudflareConfig struct {
	// DataDir is the daemon's data directory (status file, cloudflared).
	DataDir string
	// TunnelAddr is the loopback address cloudflared forwards to; ""
	// means DefaultCloudflareTunnelAddr.
	TunnelAddr string
	// LocalAddr is the local listener's address, which the tunnel listener
	// must never be.
	LocalAddr string
	// Cached returns the last tunnel the service granted, if any.
	Cached func() (TunnelGrant, bool)
	// Fetch asks the account service for this machine's tunnel.
	Fetch func(ctx context.Context) (TunnelGrant, error)
	Log   *slog.Logger
}

// Cloudflare timings.
const (
	cloudflarePoll       = 2 * time.Second
	cloudflareMinBackoff = time.Second
	cloudflareMaxBackoff = time.Minute
	// cloudflareRefresh is how often a tunnel started from the cache asks
	// the service again once it has answered.
	cloudflareRefresh = 6 * time.Hour
)

// NewCloudflare returns the Cloudflare transport.
func NewCloudflare(cfg CloudflareConfig) Transport {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.TunnelAddr == "" {
		cfg.TunnelAddr = DefaultCloudflareTunnelAddr
	}
	installer := cloudflared.NewInstaller(cfg.DataDir)
	c := &cloudflareRemote{
		cfg: cfg,
		log: cfg.Log,
		install: func(ctx context.Context) (string, error) {
			path, _, err := installer.Ensure(ctx)
			return path, err
		},
		args:       cloudflared.RunArgs(),
		poll:       cloudflarePoll,
		minBackoff: cloudflareMinBackoff,
		maxBackoff: cloudflareMaxBackoff,
		refresh:    cloudflareRefresh,
	}
	c.bus = newStatusBus(Status{State: StateStarting, Transport: TransportCloudflare}, statusFileMirror(cfg.DataDir, c.log))
	return c
}

// CloudflareHandler wraps the public root for the Cloudflare tunnel listener:
// rate limits, lockouts and audit logs see the visitor's address from
// CF-Connecting-IP, which cloudflared sets on every request it forwards,
// instead of 127.0.0.1 for everyone. A request without the header didn't
// come through the tunnel and is refused. Only the tunnel listener may use
// it (see TrustForwardedClient).
func CloudflareHandler(h http.Handler) http.Handler {
	return RequireForwardedClient(h, cfConnectingIP)
}

func cfConnectingIP(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))
}

type cloudflareRemote struct {
	cfg     CloudflareConfig
	log     *slog.Logger
	bus     *statusBus
	install func(ctx context.Context) (string, error)
	args    []string

	poll, minBackoff, maxBackoff, refresh time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	closed bool
	addr   net.Addr // the tunnel listener, once open
}

func (c *cloudflareRemote) Status() Status                     { return c.bus.snapshot() }
func (c *cloudflareRemote) Subscribe() (<-chan Status, func()) { return c.bus.subscribe() }

func (c *cloudflareRemote) publish(st Status) {
	st.Transport = TransportCloudflare
	st.UpdatedAt = time.Now()
	c.bus.publish(st)
}

// Logout has nothing to sign out of: the tunnel belongs to agentflow.
func (c *cloudflareRemote) Logout(context.Context) error {
	return errors.New("the Cloudflare tunnel has no account to log out of; turn remote access off with `agentflow remote off`")
}

func (c *cloudflareRemote) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
	c.bus.close()
	return nil
}

func (c *cloudflareRemote) listenAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addr
}

// Run serves h on the tunnel listener, gets the tunnel from the account
// service (or the cache), keeps cloudflared running with its token and
// publishes the public URL while the tunnel is connected. It returns when ctx
// is done or Close is called.
func (c *cloudflareRemote) Run(ctx context.Context, h http.Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return nil
	}
	c.cancel = cancel
	c.mu.Unlock()
	defer cancel()

	ln, err := c.listen(ctx)
	if err != nil {
		return nil // ctx ended while retrying
	}
	srv := &http.Server{
		Handler:           CloudflareHandler(h),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	var wg sync.WaitGroup
	wg.Go(func() { _ = srv.Serve(ln) })
	defer func() {
		cancel()
		shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(shutdownCtx)
		wg.Wait()
	}()

	grants := make(chan TunnelGrant, 1)
	wg.Go(func() { c.follow(ctx, grants) })

	var grant TunnelGrant
	select {
	case <-ctx.Done():
		return nil
	case grant = <-grants:
	}
	bin, err := c.cloudflared(ctx)
	if err != nil {
		return nil
	}
	for {
		next, ok := c.runTunnel(ctx, bin, grant, grants)
		if !ok {
			return nil
		}
		c.log.Info("remote: the account service changed this machine's tunnel; restarting cloudflared", "hostname", next.Hostname)
		grant = next
	}
}

// listen opens the tunnel listener, retrying while the port is taken.
func (c *cloudflareRemote) listen(ctx context.Context) (net.Listener, error) {
	backoff := c.minBackoff
	for {
		ln, err := ListenTunnel(c.cfg.TunnelAddr, c.cfg.LocalAddr)
		if err == nil {
			c.mu.Lock()
			c.addr = ln.Addr()
			c.mu.Unlock()
			return ln, nil
		}
		c.log.Warn("remote: cloudflare tunnel listener", "err", err, "retryIn", backoff)
		c.publish(Status{State: StateError, Error: err.Error()})
		if err := sleepCtx(ctx, backoff); err != nil {
			return nil, err
		}
		backoff = nextBackoff(backoff, c.maxBackoff)
	}
}

// follow sends the machine's tunnel on grants: the cached one right away if
// there is one, then whatever the account service answers when it differs.
// Until the service has answered once it retries with backoff; after that it
// asks again every refresh interval.
func (c *cloudflareRemote) follow(ctx context.Context, grants chan TunnelGrant) {
	var current TunnelGrant
	send := func(g TunnelGrant) {
		if g == current {
			return
		}
		current = g
		select {
		case <-grants: // replace an unread one
		default:
		}
		grants <- g
	}
	if c.cfg.Cached != nil {
		if g, ok := c.cfg.Cached(); ok {
			send(g)
		}
	}
	backoff := c.minBackoff
	for {
		g, err := c.cfg.Fetch(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := c.refresh
		if err != nil {
			wait = backoff
			backoff = nextBackoff(backoff, c.maxBackoff)
			if current.Hostname == "" {
				c.publish(Status{State: StateError, Error: "can't get this machine's Cloudflare address from the account service: " + err.Error()})
			}
			c.log.Warn("remote: get the tunnel from the account service", "err", err, "cached", current.Hostname != "", "retryIn", wait)
		} else {
			backoff = c.minBackoff
			send(g)
		}
		if sleepCtx(ctx, wait) != nil {
			return
		}
	}
}

// cloudflared returns a verified binary, retrying failures.
func (c *cloudflareRemote) cloudflared(ctx context.Context) (string, error) {
	backoff := c.minBackoff
	for {
		bin, err := c.install(ctx)
		if err == nil {
			return bin, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		c.log.Warn("remote: cloudflared unavailable", "err", err, "retryIn", backoff)
		c.publish(Status{State: StateError, Error: "cloudflared: " + err.Error()})
		if err := sleepCtx(ctx, backoff); err != nil {
			return "", err
		}
		backoff = nextBackoff(backoff, c.maxBackoff)
	}
}

// runTunnel supervises cloudflared for grant and publishes its state until
// ctx ends (ok false) or a different grant arrives (returned, ok true).
func (c *cloudflareRemote) runTunnel(ctx context.Context, bin string, grant TunnelGrant, grants <-chan TunnelGrant) (TunnelGrant, bool) {
	childCtx, stopChild := context.WithCancel(ctx)
	defer stopChild()
	ready := &cloudflared.Readiness{}
	var exitErr errHolder
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&cloudflared.Supervisor{
			Bin:  bin,
			Args: c.args,
			Env:  []string{cloudflared.TokenEnv(grant.Token)},
			OnLine: func(line string) {
				ready.Line(line)
				c.log.Info("cloudflared: " + line)
			},
			OnExit: func(err error) {
				ready.Reset()
				if err == nil {
					err = errors.New("exited")
				}
				exitErr.set(err)
			},
			Log:        c.log,
			MinBackoff: c.minBackoff,
			MaxBackoff: c.maxBackoff,
		}).Run(childCtx)
	}()
	defer func() {
		stopChild()
		<-done
	}()

	publicURL := "https://" + grant.Hostname
	client := &http.Client{Timeout: 3 * time.Second}
	t := time.NewTicker(c.poll)
	defer t.Stop()
	for {
		switch err := exitErr.get(); {
		case ready.Ready(childCtx, client):
			exitErr.set(nil)
			c.publish(Status{State: StateRunning, PublicURL: publicURL})
		case err != nil:
			c.publish(Status{State: StateError, Error: "cloudflared " + err.Error() + "; restarting it"})
		default:
			c.publish(Status{State: StateStarting})
		}
		select {
		case <-ctx.Done():
			return TunnelGrant{}, false
		case g := <-grants:
			if g != grant {
				return g, true
			}
		case <-t.C:
		}
	}
}

// errHolder is an error shared between the child supervisor and the watcher.
type errHolder struct {
	mu  sync.Mutex
	err error
}

func (e *errHolder) set(err error) {
	e.mu.Lock()
	e.err = err
	e.mu.Unlock()
}

func (e *errHolder) get() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}
