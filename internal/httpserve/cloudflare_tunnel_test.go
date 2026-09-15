package httpserve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/remote"
)

const tunnelHost = "brave-otter-0042.useagentflow.xyz"

// newTunnelServer serves the real public root the way the Cloudflare
// transport does: behind remote.CloudflareHandler on a loopback listener,
// with the named tunnel's https origin as the public origin.
func newTunnelServer(t *testing.T) (*gateFixture, *httptest.Server) {
	t.Helper()
	f := newGateFixture(t)
	f.api.SetPublicOrigin(func() string { return "https://" + tunnelHost })
	srv := httptest.NewServer(remote.CloudflareHandler(f.public))
	t.Cleanup(srv.Close)
	return f, srv
}

type tunnelReq struct {
	host, path, cfIP, cookie, auth string
}

func tunnelDo(t *testing.T, srv *httptest.Server, r tunnelReq) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+r.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = r.host
	if r.cfIP != "" {
		req.Header.Set("CF-Connecting-IP", r.cfIP)
	}
	if r.cookie != "" {
		req.AddCookie(&http.Cookie{Name: agentapi.DeviceCookie, Value: r.cookie})
	}
	if r.auth != "" {
		req.Header.Set("Authorization", "Bearer "+r.auth)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestCloudflareTunnelHostAllowList(t *testing.T) {
	_, srv := newTunnelServer(t)
	if code, body := tunnelDo(t, srv, tunnelReq{host: tunnelHost, path: "/api/v1/agentd/health", cfIP: "203.0.113.1"}); code != http.StatusOK {
		t.Fatalf("the tunnel's own host: %d %s", code, body)
	}
	if code, _ := tunnelDo(t, srv, tunnelReq{host: tunnelHost + ":443", path: "/api/v1/agentd/health", cfIP: "203.0.113.1"}); code != http.StatusOK {
		t.Fatalf("the tunnel's host with :443: %d", code)
	}
	for _, host := range []string{"calm-heron-0001.useagentflow.xyz", "useagentflow.xyz", "127.0.0.1:4345", "agentflow-3fa9c1.tail1234.ts.net"} {
		code, body := tunnelDo(t, srv, tunnelReq{host: host, path: "/api/v1/agentd/health", cfIP: "203.0.113.1"})
		if code != http.StatusMisdirectedRequest || !strings.Contains(body, "bad_host") {
			t.Errorf("host %s: %d %s, want 421", host, code, body)
		}
	}
}

// Unpaired visitors of the tunnel see nothing but pairing.
func TestCloudflareTunnelHidesTheAppFromUnpairedVisitors(t *testing.T) {
	_, srv := newTunnelServer(t)
	if code, body := tunnelDo(t, srv, tunnelReq{host: tunnelHost, path: "/", cfIP: "203.0.113.1"}); code != http.StatusNotFound || body != "" {
		t.Fatalf("unpaired visitor at /: %d %q", code, body)
	}
	if code, body := tunnelDo(t, srv, tunnelReq{host: tunnelHost, path: "/", cfIP: "203.0.113.1", cookie: gateToken}); code != http.StatusOK || body != "agentflow app" {
		t.Fatalf("paired visitor at /: %d %q", code, body)
	}
}

func TestCloudflareTunnelLimitsEachVisitorSeparately(t *testing.T) {
	_, srv := newTunnelServer(t)
	attacker, bystander := "198.51.100.1", "198.51.100.2"

	// Every request reaches the daemon from cloudflared on 127.0.0.1; the
	// lockout must still key on the visitor.
	for i := range Unauthorized401Limit {
		code, body := tunnelDo(t, srv, tunnelReq{host: tunnelHost, path: "/api/v1/agentd/devices", cfIP: attacker, cookie: gateToken, auth: "wrong-token"})
		if code != http.StatusUnauthorized {
			t.Fatalf("bad token %d: %d %s", i+1, code, body)
		}
	}
	if code, body := tunnelDo(t, srv, tunnelReq{host: tunnelHost, path: "/api/v1/agentd/devices", cfIP: attacker, cookie: gateToken, auth: gateToken}); code != http.StatusTooManyRequests || !strings.Contains(body, "locked_out") {
		t.Fatalf("attacker after %d 401s: %d %s, want locked out", Unauthorized401Limit, code, body)
	}

	// Another visitor through the same tunnel is unaffected.
	if code, body := tunnelDo(t, srv, tunnelReq{host: tunnelHost, path: "/api/v1/agentd/devices", cfIP: bystander, cookie: gateToken, auth: "wrong-token"}); code != http.StatusUnauthorized {
		t.Fatalf("bystander with a bad token: %d %s, want 401 (not locked out)", code, body)
	}
	if code, body := tunnelDo(t, srv, tunnelReq{host: tunnelHost, path: "/api/v1/agentd/devices", cfIP: bystander, cookie: gateToken, auth: gateToken}); code != http.StatusOK {
		t.Fatalf("bystander with the right token: %d %s", code, body)
	}

	// A request cloudflared didn't forward can't borrow or exhaust anyone's
	// limits: it is refused before the root sees it.
	if code, body := tunnelDo(t, srv, tunnelReq{host: tunnelHost, path: "/api/v1/agentd/devices", cookie: gateToken, auth: gateToken}); code != http.StatusMisdirectedRequest || !strings.Contains(body, "not_forwarded") {
		t.Fatalf("unforwarded request: %d %s", code, body)
	}
}

// The local root never answers the tunnel's hostname, so a tunnel pointed at
// the local listener by mistake gets nothing from its machine-trusted routes.
func TestLocalRootRefusesTheTunnelHost(t *testing.T) {
	f := newGateFixture(t)
	srv := httptest.NewServer(f.local)
	defer srv.Close()
	for _, path := range []string{"/", "/api/v1/agentd/health", "/api/v1/agentd/approvals/request"} {
		code, body := tunnelDo(t, srv, tunnelReq{host: tunnelHost, path: path, cfIP: "203.0.113.9"})
		if code != http.StatusMisdirectedRequest || !strings.Contains(body, "bad_host") {
			t.Fatalf("local root, tunnel host, %s: %d %s", path, code, body)
		}
	}
	// And a forged CF-Connecting-IP buys no identity there.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	if got := remote.ClientIP(req); got != "127.0.0.1" {
		t.Fatalf("client address without the tunnel wrapper = %q", got)
	}
}
