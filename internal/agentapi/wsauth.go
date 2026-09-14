package agentapi

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/arthurobo/agentflow/internal/store"
)

// wsDevice authenticates a WebSocket upgrade request. Browsers cannot set
// headers on an upgrade, so the device token rides ?token=; the query string
// is never logged. It writes the error response itself and returns nil when
// the request must not proceed.
//
// The order matters and is the same for every WebSocket: token first, then
// Origin, and only then may the handler look up or start anything.
func (s *Server) wsDevice(w http.ResponseWriter, r *http.Request) (*store.Device, string) {
	tok := r.URL.Query().Get("token")
	if tok == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing token")
		return nil, ""
	}
	d, err := s.st.VerifyClientDevice(r.Context(), tok)
	if err != nil || d == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid device token")
		return nil, ""
	}
	if !s.originAllowed(r.Header.Get("Origin")) {
		writeError(w, http.StatusForbidden, "bad_origin", "websocket origin not allowed")
		return nil, ""
	}
	return d, tok
}

// originAllowed is the WebSocket Origin allow-list, compared by exact scheme
// and host:
//   - no Origin at all: a non-browser client (the token is still required);
//   - the local listener's own configured address over http, and the
//     loopback names (127.0.0.1, localhost, [::1]) on its port;
//   - the public origin of the remote listener;
//   - the opt-in development origin (AF_DEV_CORS_ORIGIN).
//
// The request's Host header is deliberately not trusted as "same origin": a
// DNS-rebinding page controls the name it reaches us by.
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	o := normalizeOrigin(origin)
	if o == "" {
		return false // includes the opaque "null" origin
	}
	if pub := normalizeOrigin(s.PublicOrigin()); pub != "" && o == pub {
		return true
	}
	if dev := normalizeOrigin(devCORSOrigin()); dev != "" && o == dev {
		return true
	}
	host, port, err := net.SplitHostPort(s.listenAddress())
	if err != nil || port == "" {
		return false
	}
	allowed := []string{
		"http://127.0.0.1:" + port,
		"http://localhost:" + port,
		"http://[::1]:" + port,
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		allowed = append(allowed, "http://"+net.JoinHostPort(strings.ToLower(host), port))
	}
	for _, a := range allowed {
		if o == a {
			return true
		}
	}
	return false
}

// normalizeOrigin returns scheme://host[:port] in lower case, or "" when the
// value is not an http(s) origin.
func normalizeOrigin(v string) string {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return scheme + "://" + strings.ToLower(u.Host)
}
