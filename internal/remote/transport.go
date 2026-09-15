package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/client/local"
)

// A transport puts the daemon's public root on the internet. The Cloudflare
// named tunnel, the Tailscale implementations (system and embedded) and Off
// implement Transport, publish the same Status vocabulary (Status.Transport
// says which one it is), and serve the public root on a listener of their
// own: never on the local listener, which answers only this computer and
// trusts it accordingly.

// Transport names.
const (
	// TransportCloudflare is a Cloudflare named tunnel provisioned by the
	// agentflow account service: the default.
	TransportCloudflare = "cloudflare"
	// TransportTailscale is Tailscale, through the system installation or
	// agentflow's embedded node.
	TransportTailscale = "tailscale"
)

// TransportLabel is the name a person knows a transport by.
func TransportLabel(transport string) string {
	switch transport {
	case TransportCloudflare:
		return "Cloudflare"
	case TransportTailscale:
		return "Tailscale"
	default:
		return "remote access"
	}
}

// SelectConfig chooses and configures a transport.
type SelectConfig struct {
	// Transport is TransportCloudflare, TransportTailscale, or "" for off.
	Transport string
	// Tailscale configures the tailscale transport.
	Tailscale Config
	// Cloudflare configures the cloudflare transport.
	Cloudflare CloudflareConfig
}

// ErrUnknownTransport is returned by Select for a name it doesn't know.
var ErrUnknownTransport = errors.New("unknown remote transport")

// Select returns the Transport for cfg.Transport; "" is Off.
func Select(cfg SelectConfig) (Transport, error) {
	switch cfg.Transport {
	case "":
		return Off{}, nil
	case TransportCloudflare:
		if cfg.Cloudflare.Fetch == nil {
			return nil, errors.New("cloudflare transport: no account service to get the tunnel from")
		}
		return NewCloudflare(cfg.Cloudflare), nil
	case TransportTailscale:
		if cfg.Tailscale.Embedded || useEmbeddedFallback(cfg.Tailscale, exec.LookPath, systemTailscaleAnswers, fallbackProbeWait) {
			return New(cfg.Tailscale), nil
		}
		return NewSystem(cfg.Tailscale), nil
	default:
		return nil, fmt.Errorf("%w %q", ErrUnknownTransport, cfg.Transport)
	}
}

// ServeRecordName is the file in the data directory where the system
// Tailscale transport records the serve entry it added; uninstall deletes it.
const ServeRecordName = serveRecordName

// fallbackProbeWait is how long an installed Tailscale that isn't answering
// gets before agentflow falls back to its own node: tailscaled can still be
// starting when agentflow's service starts at login.
const fallbackProbeWait = 10 * time.Second

// useEmbeddedFallback keeps an existing embedded node in use when this
// computer has no working Tailscale of its own: a machine that was set up
// with agentflow's built-in node (its state is in DataDir/tailscale) and
// never installed Tailscale must keep its URL and paired phones after an
// upgrade. It never falls back while the system Tailscale answers.
func useEmbeddedFallback(cfg Config, lookPath func(string) (string, error), answers func(ctx context.Context) error, wait time.Duration) bool {
	if cfg.DataDir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "tailscale", "tailscaled.state")); err != nil {
		return false
	}
	reason := "Tailscale is not installed"
	if _, err := lookPath("tailscale"); err == nil {
		deadline := time.Now().Add(wait)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := answers(ctx)
			cancel()
			if err == nil {
				return false
			}
			if !time.Now().Before(deadline) {
				reason = "Tailscale is installed but not answering: " + err.Error()
				break
			}
			time.Sleep(min(time.Second, max(time.Until(deadline), time.Millisecond)))
		}
	}
	if cfg.Log != nil {
		cfg.Log.Info("remote: using agentflow's own Tailscale node set up earlier on this computer", "why", reason)
	}
	return true
}

func systemTailscaleAnswers(ctx context.Context) error {
	_, err := (&local.Client{}).StatusWithoutPeers(ctx)
	return err
}

// ListenTunnel opens the loopback listener a local tunnel process (tailscaled,
// cloudflared) forwards public requests to. It refuses anything that could
// be the local listener or reachable from another machine: the public root
// behind it trusts forwarding headers that only the tunnel may set.
func ListenTunnel(addr, localAddr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("tunnel address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("tunnel address %q must be a loopback IP address", addr)
	}
	if SameListenPort(addr, localAddr) {
		return nil, fmt.Errorf("tunnel address %q is the local listener's address; they must differ", addr)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("listen for the tunnel on %s: %w", addr, err)
	}
	return ln, nil
}

// SameListenPort reports whether two listen addresses would compete for the
// same port: equal non-zero ports on hosts that overlap (equal, or either
// one a wildcard).
func SameListenPort(a, b string) bool {
	ha, pa, errA := net.SplitHostPort(a)
	hb, pb, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil || pa == "0" || pa == "" || pa != pb {
		return false
	}
	wild := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
	norm := func(h string) string {
		if h == "localhost" {
			return "127.0.0.1"
		}
		return strings.Trim(h, "[]")
	}
	return wild(ha) || wild(hb) || norm(ha) == norm(hb)
}

// TrustForwardedClient wraps the public root for a tunnel listener: the
// client address rate limits and audit logs use comes from the header the
// tunnel sets, and only from a request that arrived from this computer
// (the tunnel process). clientIP extracts it from the request; an empty
// result leaves the TCP peer as the client.
//
// This wrapper belongs on the tunnel's own listener and nowhere else. On any
// listener reachable by someone other than the tunnel, the header would be
// whatever the caller wanted it to be.
func TrustForwardedClient(h http.Handler, clientIP func(r *http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := net.ParseIP(hostOnly(r.RemoteAddr)); ip != nil && ip.IsLoopback() {
			if fwd := clientIP(r); net.ParseIP(fwd) != nil {
				r = r.WithContext(WithClientIP(r.Context(), fwd))
			}
		}
		h.ServeHTTP(w, r)
	})
}

// RequireForwardedClient is TrustForwardedClient for a tunnel that always
// names the client: a request without a usable header didn't come through
// the tunnel (a local process probing the loopback port), and is refused
// with 421 rather than counted under the tunnel's own address, where it
// would share one rate-limit bucket with every probe.
func RequireForwardedClient(h http.Handler, clientIP func(r *http.Request) string) http.Handler {
	return TrustForwardedClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Value(clientIPKey{}).(string); !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMisdirectedRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"not_forwarded","message":"this port only answers requests forwarded by the tunnel"}}`+"\n")
			return
		}
		h.ServeHTTP(w, r)
	}), clientIP)
}

// LastForwardedFor is the address a reverse proxy on this computer appended
// to X-Forwarded-For: the last entry. Earlier entries came from the client.
func LastForwardedFor(r *http.Request) string {
	vals := r.Header.Values("X-Forwarded-For")
	if len(vals) == 0 {
		return ""
	}
	parts := strings.Split(vals[len(vals)-1], ",")
	return strings.TrimSpace(parts[len(parts)-1])
}
