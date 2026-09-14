// Package httpserve composes the daemon's HTTP root handlers. The daemon
// serves two: the local handler on the loopback listener and the public
// handler on the remote listener. Both carry the web UI; only the local one
// mounts the agent-member mail API and the approvals hook.
//
// Every cross-cutting protection lives here, in one place, so no route can be
// added that skips it: security headers, the Host allow-list, rate limits and
// the paired-browser gate on the public listener, the request body cap and
// JSON errors for unknown API paths.
package httpserve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/httpserve/ratelimit"
	"github.com/arthurobo/agentflow/internal/loopapi"
	"github.com/arthurobo/agentflow/internal/mailapi"
)

// Options selects what a root handler serves.
type Options struct {
	// Public is true for the handler served on the remote listener.
	Public   bool
	Control  *agentapi.Server
	Agents   *mailapi.Server
	Engineer *loopapi.Server
	// UI serves everything outside /api/. Nil serves nothing there.
	UI http.Handler
	// ListenAddr is the local listener's configured address (host:port). The
	// local root accepts only Host headers naming the loopback interface on
	// that port, or this exact address. Ignored when Public is set.
	ListenAddr string
}

// Limits applied by Root. Exported so tests and docs can name them.
const (
	// MaxBodyBytes caps a request body on every /api/ route.
	MaxBodyBytes = 1 << 20
	// MaxLocalAgentBodyBytes is the cap for the two machine-internal routes
	// on the local listener: agent mail (bodies up to half a million
	// characters) and the approvals hook (a tool input can be a whole file).
	MaxLocalAgentBodyBytes = 4 << 20

	// PublicRate and PublicBurst bound requests per client on the public
	// listener.
	PublicRate  = 20
	PublicBurst = 40
	// Unauthorized401Limit 401 responses within Unauthorized401Window block
	// the client for Unauthorized401Block.
	Unauthorized401Limit  = 20
	Unauthorized401Window = 10 * time.Minute
	Unauthorized401Block  = 15 * time.Minute
)

// contentSecurityPolicy is sent with the web UI. The Next.js static export
// boots each page with inline <script> tags whose contents change with every
// build, so script-src needs 'unsafe-inline'; everything is still restricted
// to the daemon's own origin, framing is refused and <base> cannot be
// injected. Components also use inline style attributes, hence style-src.
const contentSecurityPolicy = "default-src 'self'; connect-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'"

// Root returns the root handler for one listener.
func Root(o Options) http.Handler {
	control := o.Control.Handler()
	if o.Public {
		control = o.Control.PublicHandler()
	} else if o.ListenAddr != "" {
		// WebSocket Origin checks accept pages served from this listener.
		o.Control.SetListenAddr(o.ListenAddr)
	}
	api := jsonErrors(loopapi.Mount(control, o.Agents, o.Engineer, loopapi.MountOptions{Public: o.Public}))
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No legitimate request climbs out of a directory. Refusing these
		// here keeps every handler below from having to get it right.
		if hasDotDotSegment(r.URL.Path) {
			if isAPI(r.URL.Path) {
				writeError(w, http.StatusNotFound, "not_found", "no such API route")
			} else {
				http.NotFound(w, r)
			}
			return
		}
		if isAPI(r.URL.Path) {
			api.ServeHTTP(w, r)
			return
		}
		if o.UI == nil {
			http.NotFound(w, r)
			return
		}
		o.UI.ServeHTTP(w, r)
	})
	h = bodyCap(o.Public, h)
	if o.Public {
		h = hidePublicApp(o.Control.DeviceTokenValid, h)
		h = hostCheck(publicHostAllowed(o.Control.PublicOrigin), h)
		h = newPublicLimiter().wrap(h)
	} else {
		h = hostCheck(localHostAllowed(o.ListenAddr), h)
	}
	return securityHeaders(o.Public, h)
}

func isAPI(p string) bool { return p == "/api" || strings.HasPrefix(p, "/api/") }

// hidePublicApp answers a plain 404 to a visitor of a public listener who
// hasn't paired, for everything but what pairing needs: the pair page and
// its assets, the health check and the pairing API. A public URL can be
// found (certificate transparency logs list every Funnel host), and to
// whoever finds it the address should say nothing about what runs behind it.
//
// Paired is decided by the agentapi.DeviceCookie cookie alone. It says
// nothing about what the API allows: every API route still requires the
// device token in the Authorization header.
func hidePublicApp(valid func(ctx context.Context, token string) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if openToUnpaired(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(agentapi.DeviceCookie); err == nil && valid(r.Context(), c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusNotFound)
	})
}

// openToUnpaired lists what a browser needs to load the pair page and pair.
func openToUnpaired(p string) bool {
	switch p {
	case "/pair", "/manifest.json", "/sw.js", "/api/v1/agentd/health":
		return true
	}
	for _, prefix := range []string{"/pair/", "/_next/static/", "/icons/", "/api/v1/agentd/pair/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// hasDotDotSegment reports whether the decoded path has a ".." segment, with
// either slash as separator ("%2e%2e" and "..%2f" arrive here decoded).
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(c rune) bool { return c == '/' || c == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

// securityHeaders sets the headers every response carries. HSTS only makes
// sense over HTTPS, so only the public listener sends it.
func securityHeaders(public bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// The pair page scans a QR code with the camera.
		h.Set("Permissions-Policy", "camera=(self), microphone=()")
		if public {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !isAPI(r.URL.Path) {
			h.Set("Content-Security-Policy", contentSecurityPolicy)
		}
		next.ServeHTTP(w, r)
	})
}

// hostCheck refuses requests whose Host header is not one this listener
// answers to. On the loopback listener this is the defense against DNS
// rebinding: a web page on evil.example that rebinds its name to 127.0.0.1
// still sends Host: evil.example.
func hostCheck(allowed func(host string) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed(strings.ToLower(r.Host)) {
			writeError(w, http.StatusMisdirectedRequest, "bad_host", "this server does not answer to that host name")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func localHostAllowed(listenAddr string) func(string) bool {
	listenHost, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		listenHost, port = "", ""
	}
	listenHost = strings.ToLower(listenHost)
	wildcard := listenHost == "" || listenHost == "0.0.0.0" || listenHost == "::"
	return func(host string) bool {
		h, p, err := net.SplitHostPort(host)
		if err != nil {
			// No port in the header: only valid when the listener is on 80.
			h, p = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), "80"
		}
		if port != "" && p != port {
			return false
		}
		switch h {
		case "127.0.0.1", "localhost", "::1":
			return true
		}
		if listenHost != "" && !wildcard && h == strings.Trim(listenHost, "[]") {
			return true
		}
		// A listener bound to every interface is reached by IP address from
		// the LAN. An IP literal cannot be rebound, so it is safe to accept.
		return wildcard && port != "" && net.ParseIP(h) != nil
	}
}

func publicHostAllowed(origin func() string) func(string) bool {
	return func(host string) bool {
		o := origin()
		if o == "" {
			return false
		}
		want := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://"))
		want = strings.TrimSuffix(want, "/")
		if host == want {
			return true
		}
		if h, p, err := net.SplitHostPort(host); err == nil && p == "443" && h == want {
			return true
		}
		return false
	}
}

// bodyCap bounds request bodies on /api/. A declared oversized body is refused
// up front with 413; a chunked one is cut off by MaxBytesReader and the
// handler's decode fails. WebSocket upgrades have no body and are left alone.
func bodyCap(public bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAPI(r.URL.Path) || r.Body == nil || r.Body == http.NoBody || isUpgrade(r) {
			next.ServeHTTP(w, r)
			return
		}
		limit := int64(MaxBodyBytes)
		if !public && (strings.HasPrefix(r.URL.Path, mailapi.Prefix+"/") || r.URL.Path == "/api/v1/agentd/approvals/request") {
			limit = MaxLocalAgentBodyBytes
		}
		if r.ContentLength > limit {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				"request body exceeds "+strconv.FormatInt(limit, 10)+" bytes")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// publicLimiter applies the per-client limits on the public listener.
type publicLimiter struct {
	rate    *ratelimit.Bucket
	lockout *ratelimit.Lockout
}

func newPublicLimiter() *publicLimiter {
	return &publicLimiter{
		rate:    ratelimit.NewBucket(PublicRate, PublicBurst),
		lockout: ratelimit.NewLockout(Unauthorized401Limit, Unauthorized401Window, Unauthorized401Block),
	}
}

func (l *publicLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ratelimit.ClientIP(r)
		if blocked, left := l.lockout.Blocked(ip); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(int(left.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "locked_out", "too many unauthorized requests; try again later")
			return
		}
		if !l.rate.Allow(ip) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == http.StatusUnauthorized {
			l.lockout.Strike(ip)
		}
	})
}

// statusWriter records the response status. Hijack and Unwrap keep WebSocket
// upgrades working through it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return hijack(w.ResponseWriter)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func hijack(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hijack not supported by %T", w)
	}
	return h.Hijack()
}

// jsonErrors turns the router's own plain-text 404 and 405 answers into the
// API's JSON error shape, so a client never has to parse HTML or text from an
// /api/ path. Handlers that already answer JSON are left untouched.
func jsonErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&jsonErrorWriter{ResponseWriter: w}, r)
	})
}

type jsonErrorWriter struct {
	http.ResponseWriter
	wroteHeader bool
	swallow     bool
}

func (w *jsonErrorWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) &&
		!strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		errCode, msg := "not_found", "no such API route"
		if code == http.StatusMethodNotAllowed {
			errCode, msg = "method_not_allowed", "method not allowed on this route"
		}
		writeError(w.ResponseWriter, code, errCode, msg)
		w.swallow = true
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *jsonErrorWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.swallow {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func (w *jsonErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *jsonErrorWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return hijack(w.ResponseWriter)
}

func (w *jsonErrorWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
