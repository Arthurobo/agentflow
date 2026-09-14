package remote

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"

	"tailscale.com/ipn"
)

// clientIPKey carries the real client address of a remote connection. The
// key lives here, in a package with no internal imports, because the HTTP
// layers that read it (agentapi's rate limiter, httpserve) import each other
// and this package; internal/httpserve re-exports the helpers.
type clientIPKey struct{}

// WithClientIP returns a context that records ip as the connection's client.
func WithClientIP(ctx context.Context, ip string) context.Context {
	if ip == "" {
		return ctx
	}
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIP returns the client IP recorded for r's connection, falling back to
// the host part of r.RemoteAddr (the local listener never records one).
func ClientIP(r *http.Request) string {
	if v, ok := r.Context().Value(clientIPKey{}).(string); ok && v != "" {
		return v
	}
	return hostOnly(r.RemoteAddr)
}

// ClientIPFromConn returns the address of the party that opened c.
//
// On a Funnel connection RemoteAddr is the Tailscale relay node, the same for
// every phone on the internet; the real client is the Src of the
// ipn.FunnelConn underneath the TLS layer. Tailscale does not add
// Tailscale-Funnel-Request or X-Forwarded-For headers on this path, so the
// connection is the only trustworthy source. Everything else (tailnet peers,
// plain TCP) uses RemoteAddr.
func ClientIPFromConn(c net.Conn) string {
	inner := c
	for {
		switch v := inner.(type) {
		case *ipn.FunnelConn:
			if v.Src.IsValid() {
				return v.Src.Addr().String()
			}
			return hostOnly(addrString(c.RemoteAddr()))
		case *tls.Conn:
			inner = v.NetConn()
			continue
		}
		return hostOnly(addrString(c.RemoteAddr()))
	}
}

// connContext is the http.Server ConnContext for the remote listener.
func connContext(ctx context.Context, c net.Conn) context.Context {
	return WithClientIP(ctx, ClientIPFromConn(c))
}

func addrString(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}

func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
