// Hostile-network suite. The daemon answers on a public Funnel URL and on
// 127.0.0.1, where any web page can reach it, so every request here is sent
// through the same composed root handlers the daemon serves (httpserve.Root),
// never through an inner mux that skips the protections around it. A failure
// here means a deployment invariant has been broken.
package agentapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/approvals"
	"github.com/arthurobo/agentflow/internal/httpserve"
	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/loopapi"
	"github.com/arthurobo/agentflow/internal/mailapi"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
	"github.com/arthurobo/agentflow/internal/webui"
)

const (
	hostileLocalAddr    = "127.0.0.1:4344"
	hostilePublicHost   = "agentflow-3fa9c1.tail1234.ts.net"
	hostilePublicOrigin = "https://" + hostilePublicHost
	hostileDeviceToken  = "device-token-1"
)

type hostile struct {
	t      *testing.T
	dir    string
	marker string
	st     *store.Store
	sp     *spawner.Spawner
	srv    *agentapi.Server
	agents *mailapi.Server
	loops  *loopapi.Server
	local  http.Handler
	public http.Handler
}

// newHostile builds the daemon's two roots over a real store and spawner. The
// engine is a fake that appends a line to h.marker every time it is started
// as a terminal, which is how tests prove nothing was spawned.
func newHostile(t *testing.T) *hostile {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("home: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("AF_REMOTE", "off")
	t.Setenv("AF_DEV_CORS_ORIGIN", "")

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	marker := filepath.Join(dir, "spawned")
	fake := filepath.Join(dir, "fake-claude")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo '2.1.270 (Claude Code)'; exit 0; fi\n" +
		"echo started >> '" + marker + "'\n" +
		"printf 'tui up\\r\\n'\n" +
		"while IFS= read -r line; do printf 'got: %s\\r\\n' \"$line\"; done\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { //nolint:gosec // the fixture must be executable
		t.Fatalf("fake claude: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sp := spawner.New(st, ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"}), logger)
	sp.SetClaudePath(fake)
	sp.SetCorpusRoot(filepath.Join(dir, "corpus"))
	t.Cleanup(func() { _ = sp.StopAll(context.Background()) })

	ctx := context.Background()
	seed := func(id, kind, status, token string, expires int64) {
		t.Helper()
		if err := st.UpsertDevice(ctx, &store.Device{ID: id, Kind: kind, Status: status, TokenHash: store.HashToken(token)}, expires); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("dev-1", "device", "active", hostileDeviceToken, 0)
	seed("pair-1", "pairing", "pending", "pair-token-1", time.Now().Add(15*time.Minute).UnixMilli())
	seed("agentd-1", "agentd", "active", "agentd-token-1", 0)
	seed("dev-revoked", "device", "active", "revoked-token-1", 0)
	if err := st.RevokeDevice(ctx, "dev-revoked"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	srv := agentapi.New(st, sp, logger, "hostile-test")
	srv.SetApprovals(approvals.New(st, logger))
	srv.SetHookSecret([]byte("hook-secret-for-tests-1234567890"))
	srv.SetPublicOrigin(func() string { return hostilePublicOrigin })
	agents := mailapi.New(st, logger, mailapi.Config{})
	loops := loopapi.New(st, agents, logger, loopapi.Config{BaseURL: "http://" + hostileLocalAddr})

	h := &hostile{t: t, dir: dir, marker: marker, st: st, sp: sp, srv: srv, agents: agents, loops: loops}
	h.local = h.root(false, hostileLocalAddr)
	h.public = h.root(true, "")
	return h
}

func (h *hostile) root(public bool, listen string) http.Handler {
	return httpserve.Root(httpserve.Options{
		Public: public, Control: h.srv, Agents: h.agents, Engineer: h.loops,
		UI: webui.Handler(), ListenAddr: listen,
	})
}

type hostileReq struct {
	method, path, auth, body, host, origin string
	// unpaired leaves off the paired-browser cookie a public request
	// otherwise carries. Without the cookie the public listener hides
	// everything but pairing, which this suite covers in the gate's own
	// tests; here the cookie is present so the API's own checks are what
	// answer.
	unpaired bool
}

// do sends r through the local or public root with the Host a browser would
// send to that listener unless r names another.
func (h *hostile) do(public bool, r hostileReq) *httptest.ResponseRecorder {
	h.t.Helper()
	var body io.Reader = http.NoBody
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	method := r.method
	if method == "" {
		method = http.MethodGet
	}
	req := httptest.NewRequest(method, r.path, body)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Host = hostileLocalAddr
	if public {
		req.Host = hostilePublicHost
		req.RemoteAddr = "203.0.113.7:50000"
	}
	if r.host != "" {
		req.Host = r.host
	}
	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.auth != "" {
		req.Header.Set("Authorization", "Bearer "+r.auth)
	}
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	if public && !r.unpaired {
		req.AddCookie(&http.Cookie{Name: agentapi.DeviceCookie, Value: hostileDeviceToken})
	}
	w := httptest.NewRecorder()
	if public {
		h.public.ServeHTTP(w, req)
	} else {
		h.local.ServeHTTP(w, req)
	}
	return w
}

// spawns reports how many terminals the fake engine has started.
func (h *hostile) spawns() int {
	raw, err := os.ReadFile(h.marker)
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "started")
}

func (h *hostile) seedRun(id, state, stopReason, sessionID string) {
	h.t.Helper()
	now := time.Now().UnixMilli()
	if err := h.st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: id, SessionID: sessionID, Kind: "tty", Engine: "claude", State: state,
		StopReason: stopReason, StartedAt: now, EndedAt: now, UpdatedAt: now, CWD: h.dir,
	}); err != nil {
		h.t.Fatalf("seed run %s: %v", id, err)
	}
}

func assertJSONError(t *testing.T, w *httptest.ResponseRecorder, code string) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q, want JSON; body %s", ct, w.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, w.Body.String())
	}
	if code != "" && body.Error.Code != code {
		t.Fatalf("error code %q, want %q (%s)", body.Error.Code, code, w.Body.String())
	}
}

func TestRouteExposureMatrix(t *testing.T) {
	h := newHostile(t)
	cases := []struct {
		name          string
		req           hostileReq
		local, public int
	}{
		{"static UI", hostileReq{path: "/"}, 200, 200},
		{"health", hostileReq{path: "/api/v1/agentd/health"}, 200, 200},
		{"pair complete without a token", hostileReq{method: "POST", path: "/api/v1/agentd/pair/complete", body: `{}`}, 400, 400},
		{"pair complete with a wrong token", hostileReq{method: "POST", path: "/api/v1/agentd/pair/complete", body: `{"token":"wrong"}`}, 401, 401},
		{"sessions without auth", hostileReq{path: "/api/v1/agentd/sessions"}, 401, 401},
		{"sessions with a device", hostileReq{path: "/api/v1/agentd/sessions", auth: hostileDeviceToken}, 200, 200},
		{"cwds without auth", hostileReq{path: "/api/v1/agentd/cwds"}, 401, 401},
		{"member mail API", hostileReq{path: mailapi.Prefix + "/whoami"}, 401, 404},
		{"approvals hook without the secret", hostileReq{method: "POST", path: "/api/v1/agentd/approvals/request", body: `{}`}, 401, 404},
		{"loops without auth", hostileReq{path: loopapi.Prefix}, 401, 401},
		{"loops with a device", hostileReq{path: loopapi.Prefix, auth: hostileDeviceToken}, 200, 200},
		{"tools without auth", hostileReq{path: loopapi.ToolsPrefix}, 401, 401},
		{"tools with a device", hostileReq{path: loopapi.ToolsPrefix, auth: hostileDeviceToken}, 200, 200},
		{"prompts without auth", hostileReq{path: loopapi.PromptsPrefix}, 401, 401},
		{"prompts with a device", hostileReq{path: loopapi.PromptsPrefix, auth: hostileDeviceToken}, 200, 200},
		{"unknown API path", hostileReq{path: "/api/v1/nope", auth: hostileDeviceToken}, 404, 404},
		{"bare /api", hostileReq{path: "/api"}, 404, 404},
		{"wrong method on an API route", hostileReq{method: "PUT", path: "/api/v1/agentd/health"}, 405, 405},
		// A visitor who hasn't paired finds nothing on the public URL but
		// the way to pair; locally nothing changes.
		{"static UI, unpaired", hostileReq{path: "/", unpaired: true}, 200, 404},
		{"sessions with a device header but no cookie", hostileReq{path: "/api/v1/agentd/sessions", auth: hostileDeviceToken, unpaired: true}, 200, 404},
		{"health, unpaired", hostileReq{path: "/api/v1/agentd/health", unpaired: true}, 200, 200},
		{"pair complete with a wrong token, unpaired", hostileReq{method: "POST", path: "/api/v1/agentd/pair/complete", body: `{"token":"wrong"}`, unpaired: true}, 401, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, public := range []bool{false, true} {
				want := tc.local
				if public {
					want = tc.public
				}
				w := h.do(public, tc.req)
				if w.Code != want {
					t.Fatalf("public=%v %s %s: status %d, want %d (%s)", public, tc.req.method, tc.req.path, w.Code, want, w.Body.String())
				}
				hidden := public && tc.req.unpaired && w.Code == http.StatusNotFound
				if strings.HasPrefix(tc.req.path, "/api") && w.Code >= 400 && !hidden {
					assertJSONError(t, w, "")
				}
			}
		})
	}
}

func TestHealthIsMinimalOnPublicAndFullLocally(t *testing.T) {
	h := newHostile(t)
	var pub map[string]any
	if err := json.Unmarshal(h.do(true, hostileReq{path: "/api/v1/agentd/health"}).Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pub) != 1 || pub["status"] != "ok" {
		t.Fatalf("public health must be exactly the status, got %v", pub)
	}
	var loc map[string]any
	if err := json.Unmarshal(h.do(false, hostileReq{path: "/api/v1/agentd/health"}).Body.Bytes(), &loc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if loc["machineId"] != "hostile-test" || loc["sessions"] == nil {
		t.Fatalf("local health lost its detail: %v", loc)
	}
}

func TestOnlyActiveDeviceTokensAuthenticate(t *testing.T) {
	h := newHostile(t)
	for name, tok := range map[string]string{
		"pending pairing token": "pair-token-1",
		"agentd kind token":     "agentd-token-1",
		"revoked device":        "revoked-token-1",
	} {
		for _, path := range []string{"/api/v1/agentd/sessions", loopapi.Prefix} {
			for _, public := range []bool{false, true} {
				if w := h.do(public, hostileReq{path: path, auth: tok}); w.Code != http.StatusUnauthorized {
					t.Fatalf("%s on %s (public=%v): status %d, want 401", name, path, public, w.Code)
				}
			}
		}
	}
	// And a pairing token still pairs, exactly once.
	if w := h.do(false, hostileReq{method: "POST", path: "/api/v1/agentd/pair/complete", body: `{"token":"pair-token-1","name":"phone"}`}); w.Code != http.StatusCreated {
		t.Fatalf("pairing: %d %s", w.Code, w.Body.String())
	}
	if w := h.do(false, hostileReq{method: "POST", path: "/api/v1/agentd/pair/complete", body: `{"token":"pair-token-1","name":"again"}`}); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed pairing token: %d", w.Code)
	}
}

func TestPairingIsRateLimitedPerClient(t *testing.T) {
	h := newHostile(t)
	for i := 0; i < 5; i++ {
		if w := h.do(true, hostileReq{method: "POST", path: "/api/v1/agentd/pair/complete", body: `{"token":"guess"}`}); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i+1, w.Code)
		}
	}
	w := h.do(true, hostileReq{method: "POST", path: "/api/v1/agentd/pair/complete", body: `{"token":"pair-token-1"}`})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt within a minute: %d, want 429", w.Code)
	}
	// Even the right token waits: a brute force that finally guesses it
	// must not get through the limit.
	if d, _ := h.st.GetDeviceByID(context.Background(), "pair-1"); d == nil || d.Status != "pending" {
		t.Fatalf("pairing token consumed through the limit: %+v", d)
	}
}

func TestWebSocketsRequireATokenEvenFromLoopback(t *testing.T) {
	h := newHostile(t)
	h.seedRun("run-done", "finished", "", "")
	for _, path := range []string{"/api/v1/agentd/sessions/run-done/tty/ws"} {
		w := h.do(false, hostileReq{path: path})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token: %d, want 401", path, w.Code)
		}
	}
	if n := h.spawns(); n != 0 {
		t.Fatalf("an unauthenticated attach started %d terminals", n)
	}
}

// A cross-site page must not be able to start anything, even with a stolen
// token, and the Host header it controls must not make it "same origin".
func TestWebSocketsRejectForeignOriginsWithoutStartingAnything(t *testing.T) {
	h := newHostile(t)
	h.seedRun("run-done", "finished", "", "") // would resume if attached
	path := "/api/v1/agentd/sessions/run-done/tty/ws?token=" + hostileDeviceToken
	for _, origin := range []string{
		"https://evil.example",
		"null",
		"http://127.0.0.1:9999",       // another local port
		"https://" + hostileLocalAddr, // wrong scheme
		"http://" + hostilePublicHost, // public host over http
	} {
		if w := h.do(false, hostileReq{path: path, origin: origin}); w.Code != http.StatusForbidden {
			t.Fatalf("origin %q: %d, want 403 (%s)", origin, w.Code, w.Body.String())
		}
	}
	// DNS rebinding: the page's own name as both Host and Origin. The root
	// refuses the Host outright; the control handler alone must still refuse
	// the Origin rather than calling it same-origin.
	if w := h.do(false, hostileReq{path: path, host: "evil.example:4344", origin: "http://evil.example:4344"}); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding host through the root: %d, want 421", w.Code)
	}
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "evil.example:4344"
	req.Header.Set("Origin", "http://evil.example:4344")
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("rebinding origin on the control handler: %d, want 403", w.Code)
	}
	time.Sleep(300 * time.Millisecond)
	if n := h.spawns(); n != 0 {
		t.Fatalf("a foreign origin started %d terminals", n)
	}
}

func TestWebSocketOriginAllowList(t *testing.T) {
	h := newHostile(t)
	hs := httptest.NewUnstartedServer(nil)
	hs.Config.Handler = h.root(false, hs.Listener.Addr().String())
	hs.Start()
	t.Cleanup(hs.Close)
	_, port, _ := net.SplitHostPort(hs.Listener.Addr().String())

	h.seedRun("run-live", "running", "", "")
	master := newBlockingMaster()
	h.srv.RegisterTTY("run-live", master, nil, 0)
	t.Cleanup(func() { _ = master.Close() })

	dial := func(origin string) (int, error) {
		u := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/v1/agentd/sessions/run-live/tty/ws?token=" + hostileDeviceToken
		hdr := http.Header{}
		if origin != "" {
			hdr.Set("Origin", origin)
		}
		c, resp, err := websocket.DefaultDialer.Dial(u, hdr)
		if c != nil {
			_ = c.Close()
		}
		if resp != nil {
			return resp.StatusCode, err
		}
		return 0, err
	}
	for _, origin := range []string{"", "http://127.0.0.1:" + port, "http://localhost:" + port, "http://[::1]:" + port, hostilePublicOrigin} {
		if code, err := dial(origin); err != nil || code != http.StatusSwitchingProtocols {
			t.Fatalf("origin %q must be allowed: %d %v", origin, code, err)
		}
	}
	for _, origin := range []string{"https://evil.example", "null", "http://localhost:1"} {
		if code, _ := dial(origin); code != http.StatusForbidden {
			t.Fatalf("origin %q must be refused: %d", origin, code)
		}
	}
}

func TestUnknownRunIDsNeverSpawn(t *testing.T) {
	h := newHostile(t)
	ws := h.do(false, hostileReq{path: "/api/v1/agentd/sessions/no-such-run/tty/ws?token=" + hostileDeviceToken})
	if ws.Code != http.StatusNotFound {
		t.Fatalf("tty ws for an unknown run: %d, want 404 (%s)", ws.Code, ws.Body.String())
	}
	post := h.do(false, hostileReq{method: "POST", path: "/api/v1/agentd/sessions/no-such-run/tty", auth: hostileDeviceToken})
	if post.Code != http.StatusNotFound {
		t.Fatalf("POST tty for an unknown run: %d, want 404 (%s)", post.Code, post.Body.String())
	}
	time.Sleep(300 * time.Millisecond)
	if n := h.spawns(); n != 0 {
		t.Fatalf("unknown run ids started %d terminals", n)
	}
	if _, err := h.sp.Status(context.Background(), "no-such-run"); err == nil {
		t.Fatal("an unknown run id must not gain a row")
	}
}

func TestOversizedBodiesAreRefused(t *testing.T) {
	h := newHostile(t)
	// Valid JSON, just too big: only the body cap can refuse it.
	big := `{"kind":"tty","prompt":"` + strings.Repeat("a", httpserve.MaxBodyBytes+1) + `"}`
	for _, path := range []string{"/api/v1/agentd/sessions", loopapi.Prefix} {
		for _, public := range []bool{false, true} {
			w := h.do(public, hostileReq{method: "POST", path: path, auth: hostileDeviceToken, body: big})
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s (public=%v): %d, want 413", path, public, w.Code)
			}
			assertJSONError(t, w, "body_too_large")
		}
	}
	if n := h.spawns(); n != 0 {
		t.Fatalf("an oversized spawn request started %d terminals", n)
	}
}

func TestPathTraversalThroughTheRoot(t *testing.T) {
	h := newHostile(t)
	for _, p := range []string{
		"/..%2f..%2fetc%2fpasswd",
		"/_next/../../etc/passwd",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/api/v1/agentd/..%2f..%2fetc/passwd",
	} {
		for _, public := range []bool{false, true} {
			req := httptest.NewRequest("GET", "/", nil)
			req.URL = &url.URL{Path: mustUnescape(t, p), RawPath: p}
			req.Host = hostileLocalAddr
			if public {
				req.Host = hostilePublicHost
			}
			w := httptest.NewRecorder()
			if public {
				h.public.ServeHTTP(w, req)
			} else {
				h.local.ServeHTTP(w, req)
			}
			if w.Code != http.StatusNotFound || bytes.Contains(w.Body.Bytes(), []byte("root:")) {
				t.Fatalf("%s (public=%v): %d %q", p, public, w.Code, w.Body.String())
			}
		}
	}
}

func mustUnescape(t *testing.T, p string) string {
	t.Helper()
	u, err := url.PathUnescape(p)
	if err != nil {
		t.Fatalf("unescape %s: %v", p, err)
	}
	return u
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newHostile(t)
	for _, public := range []bool{false, true} {
		for _, path := range []string{"/", "/api/v1/agentd/health", "/api/v1/nope", loopapi.Prefix} {
			w := h.do(public, hostileReq{path: path})
			for k, v := range map[string]string{
				"X-Content-Type-Options": "nosniff",
				"Referrer-Policy":        "no-referrer",
				"X-Frame-Options":        "DENY",
				"Permissions-Policy":     "camera=(self), microphone=()",
			} {
				if got := w.Header().Get(k); got != v {
					t.Fatalf("public=%v %s: %s = %q, want %q", public, path, k, got, v)
				}
			}
			if got := w.Header().Get("Strict-Transport-Security") != ""; got != public {
				t.Fatalf("public=%v %s: HSTS present = %v", public, path, got)
			}
			csp := w.Header().Get("Content-Security-Policy")
			if path == "/" && !strings.Contains(csp, "frame-ancestors 'none'") {
				t.Fatalf("public=%v: the UI must carry the CSP, got %q", public, csp)
			}
		}
	}
}

// blockingMaster is a PTY stand-in whose Read blocks until Close.
type blockingMaster struct {
	closed chan struct{}
}

func newBlockingMaster() *blockingMaster { return &blockingMaster{closed: make(chan struct{})} }

func (m *blockingMaster) Read([]byte) (int, error) {
	<-m.closed
	return 0, io.EOF
}

func (m *blockingMaster) Write(b []byte) (int, error) { return len(b), nil }

func (m *blockingMaster) Close() error {
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return nil
}
