package httpserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/httpserve/ratelimit"
)

func TestLocalHostAllowList(t *testing.T) {
	allowed := localHostAllowed("127.0.0.1:4344")
	for _, h := range []string{"127.0.0.1:4344", "localhost:4344", "[::1]:4344"} {
		if !allowed(h) {
			t.Fatalf("%s must be accepted", h)
		}
	}
	for _, h := range []string{
		"evil.example:4344", // DNS rebinding
		"evil.example",
		"127.0.0.1:9999",
		"localhost",
		"192.168.1.10:4344", // not the configured address
		"",
	} {
		if allowed(h) {
			t.Fatalf("%s must be refused", h)
		}
	}

	named := localHostAllowed("devbox.lan:4344")
	if !named("devbox.lan:4344") || !named("localhost:4344") || named("other.lan:4344") {
		t.Fatal("a named listen address accepts exactly itself plus loopback")
	}
	wild := localHostAllowed("0.0.0.0:4344")
	if !wild("192.168.1.10:4344") || wild("evil.example:4344") {
		t.Fatal("a wildcard listener accepts IP literals on its port but never names")
	}
}

func TestPublicHostAllowList(t *testing.T) {
	origin := "https://agentflow-3fa9c1.tail1234.ts.net"
	allowed := publicHostAllowed(func() string { return origin })
	if !allowed("agentflow-3fa9c1.tail1234.ts.net") || !allowed("agentflow-3fa9c1.tail1234.ts.net:443") {
		t.Fatal("the public host must be accepted")
	}
	for _, h := range []string{"evil.example", "127.0.0.1:4344", "agentflow-3fa9c1.tail1234.ts.net:8443", "x.agentflow-3fa9c1.tail1234.ts.net"} {
		if allowed(h) {
			t.Fatalf("%s must be refused", h)
		}
	}
	origin = ""
	if allowed("agentflow-3fa9c1.tail1234.ts.net") {
		t.Fatal("with no public origin every host is refused")
	}
}

func TestHostCheckAnswersJSON421(t *testing.T) {
	h := hostCheck(localHostAllowed("127.0.0.1:4344"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("a refused host must not reach the handler")
	}))
	req := httptest.NewRequest("GET", "/api/v1/agentd/health", nil)
	req.Host = "evil.example:4344"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMisdirectedRequest || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("got %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func publicReq(ip string) *http.Request {
	req := httptest.NewRequest("GET", "/api/v1/agentd/sessions", nil)
	req.RemoteAddr = ip + ":5555"
	return req
}

func TestPublicRateLimitTrips(t *testing.T) {
	l := newPublicLimiter()
	h := l.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	limited := 0
	for i := 0; i < PublicBurst+10; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, publicReq("203.0.113.1"))
		if w.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatalf("%d requests in a burst never tripped the limit", PublicBurst+10)
	}
	// Another client is unaffected.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, publicReq("203.0.113.2"))
	if w.Code != http.StatusOK {
		t.Fatalf("a different client was limited: %d", w.Code)
	}
}

func TestRepeatedUnauthorizedResponsesLockTheClientOut(t *testing.T) {
	l := newPublicLimiter()
	h := l.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	for i := 0; i < Unauthorized401Limit; i++ {
		// Stay under the rate limit: the lockout, not the bucket, must trip.
		w := httptest.NewRecorder()
		h.ServeHTTP(w, publicReq("198.51.100.9"))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: %d, want 401", i+1, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, publicReq("198.51.100.9"))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("after %d 401s: %d, want 429 with Retry-After", Unauthorized401Limit, w.Code)
	}
}

func TestLimiterKeysOnTheFunnelClientAddress(t *testing.T) {
	// The remote listener stamps the real client into the connection
	// context; every Funnel request shares the relay's RemoteAddr.
	l := newPublicLimiter()
	h := l.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	for i := 0; i < Unauthorized401Limit; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, publicReq("100.100.100.100"))
	}
	req := publicReq("100.100.100.100")
	req = req.WithContext(ratelimit.WithClientIP(context.Background(), "203.0.113.50:1234"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a distinct Funnel client inherited the relay's lockout: %d", w.Code)
	}
}

func TestUnknownAPIPathsAnswerJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/thing", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := jsonErrors(mux)
	for path, code := range map[string]int{"/api/v1/nope": 404} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != code || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") ||
			!strings.Contains(w.Body.String(), `"code":"not_found"`) || strings.Contains(w.Body.String(), "404 page not found") {
			t.Fatalf("%s: %d %q %q", path, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/thing", nil))
	if w.Code != http.StatusMethodNotAllowed || !strings.Contains(w.Body.String(), `"method_not_allowed"`) {
		t.Fatalf("wrong method: %d %q", w.Code, w.Body.String())
	}
}

func TestBodyCap(t *testing.T) {
	var read int
	h := bodyCap(true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64<<10)
		for {
			n, err := r.Body.Read(buf)
			read += n
			if err != nil {
				if strings.Contains(err.Error(), "too large") {
					w.WriteHeader(http.StatusRequestEntityTooLarge)
				}
				return
			}
		}
	}))
	big := strings.Repeat("a", MaxBodyBytes+1)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/loops", strings.NewReader(big)))
	if w.Code != http.StatusRequestEntityTooLarge || read != 0 {
		t.Fatalf("declared oversized body: %d (handler read %d bytes)", w.Code, read)
	}
	// Undeclared length (chunked): cut off at the cap.
	req := httptest.NewRequest("POST", "/api/v1/loops", strings.NewReader(big))
	req.ContentLength = -1
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge || read > MaxBodyBytes {
		t.Fatalf("chunked oversized body: %d, read %d", w.Code, read)
	}
}
