package httpserve

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arthurobo/agentflow/internal/remote"
)

// The system Tailscale reaches the public root through a loopback listener,
// so every request's TCP peer is 127.0.0.1. The rate limits and the 401
// lockout must still tell visitors apart by the client tailscaled forwards.
func TestForwardedClientsGetTheirOwnLimits(t *testing.T) {
	f := newGateFixture(t)
	const host = "desk.tail1234.ts.net:8443"
	f.api.SetPublicOrigin(func() string { return "https://" + host })
	tunnel := remote.RequireForwardedClient(f.public, remote.LastForwardedFor)

	do := func(xff, hostHeader string) int {
		req := httptest.NewRequest("GET", "/api/v1/agentd/devices", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		req.Host = hostHeader
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		req.AddCookie(&http.Cookie{Name: "af_device", Value: gateToken})
		req.Header.Set("Authorization", "Bearer wrong-token")
		w := httptest.NewRecorder()
		tunnel.ServeHTTP(w, req)
		return w.Code
	}

	for i := range Unauthorized401Limit {
		if code := do("203.0.113.1", host); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d from the first client: %d", i+1, code)
		}
	}
	if code := do("203.0.113.1", host); code != http.StatusTooManyRequests {
		t.Fatalf("the first client after %d failures: %d, want 429", Unauthorized401Limit, code)
	}
	if code := do("203.0.113.2", host); code != http.StatusUnauthorized {
		t.Fatalf("a second client inherited the first one's lockout: %d", code)
	}
	if code := do("", host); code != http.StatusMisdirectedRequest {
		t.Fatalf("a request tailscaled didn't forward: %d, want 421", code)
	}
}

func TestPublicOriginWithAPortAcceptsOnlyThatHostAndPort(t *testing.T) {
	allowed := publicHostAllowed(func() string { return "https://desk.tail1234.ts.net:8443" })
	if !allowed("desk.tail1234.ts.net:8443") {
		t.Fatal("the public host and port must be accepted")
	}
	for _, h := range []string{"desk.tail1234.ts.net", "desk.tail1234.ts.net:443", "desk.tail1234.ts.net:8444", "127.0.0.1:8443"} {
		if allowed(h) {
			t.Errorf("%s must be refused", h)
		}
	}
}
