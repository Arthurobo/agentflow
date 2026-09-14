package httpserve

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/loopapi"
	"github.com/arthurobo/agentflow/internal/mailapi"
	"github.com/arthurobo/agentflow/internal/store"
)

const (
	gatePublicHost = "agentflow-3fa9c1.tail1234.ts.net"
	gateToken      = "paired-device-token"
)

type gateFixture struct {
	st            *store.Store
	api           *agentapi.Server
	public, local http.Handler
}

// newGateFixture builds both roots over a real store, with a UI that answers
// every path with a marker so a test can tell "served" from "hidden".
func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	st, err := store.Open(filepath.Join(dir, "gate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.UpsertDevice(ctx, &store.Device{ID: "dev-1", Kind: "device", Status: "active", TokenHash: store.HashToken(gateToken)}, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDevice(ctx, &store.Device{ID: "pair-1", Kind: "pairing", Status: "pending", TokenHash: store.HashToken("pair-token")},
		time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := agentapi.New(st, nil, log, "gate-test")
	api.SetPublicOrigin(func() string { return "https://" + gatePublicHost })
	agents := mailapi.New(st, log, mailapi.Config{})
	loops := loopapi.New(st, agents, log, loopapi.Config{BaseURL: "http://127.0.0.1:4344"})
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "agentflow app") })
	return &gateFixture{
		st:     st,
		api:    api,
		public: Root(Options{Public: true, Control: api, Agents: agents, Engineer: loops, UI: ui}),
		local:  Root(Options{Control: api, Agents: agents, Engineer: loops, UI: ui, ListenAddr: "127.0.0.1:4344"}),
	}
}

type gateReq struct {
	method, path, cookie, auth, body string
}

func (f *gateFixture) do(public bool, r gateReq) *httptest.ResponseRecorder {
	method := r.method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader = http.NoBody
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	req := httptest.NewRequest(method, r.path, body)
	req.Host, req.RemoteAddr = "127.0.0.1:4344", "127.0.0.1:50000"
	if public {
		req.Host, req.RemoteAddr = gatePublicHost, "203.0.113.7:50000"
	}
	if r.cookie != "" {
		req.AddCookie(&http.Cookie{Name: agentapi.DeviceCookie, Value: r.cookie})
	}
	if r.auth != "" {
		req.Header.Set("Authorization", "Bearer "+r.auth)
	}
	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	if public {
		f.public.ServeHTTP(w, req)
	} else {
		f.local.ServeHTTP(w, req)
	}
	return w
}

func assertHidden(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != http.StatusNotFound {
		t.Fatalf("%s: status %d, want 404 (%s)", what, w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("%s: a hidden 404 must be empty, got %q", what, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "" {
		t.Fatalf("%s: a hidden 404 must not say what it is, got Content-Type %q", what, ct)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Fatalf("%s: a hidden 404 must not redirect, got %q", what, loc)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s: a hidden 404 must not be cached (it serves the app once paired)", what)
	}
}

func TestPublicListenerHidesTheAppFromUnpairedVisitors(t *testing.T) {
	f := newGateFixture(t)
	for _, path := range []string{"/", "/session/", "/session/?id=run-1", "/loop/", "/loop/board/", "/index.txt",
		"/api/v1/agentd/sessions", "/api/v1/loops", "/api/v1/nope", "/api"} {
		assertHidden(t, f.do(true, gateReq{path: path}), "unpaired "+path)
	}
	// A device token in the header isn't a paired browser either: the
	// page it would have come from could not have loaded.
	assertHidden(t, f.do(true, gateReq{path: "/api/v1/agentd/sessions", auth: gateToken}), "header without a cookie")
	// Pairing tokens and made-up values don't count as paired.
	assertHidden(t, f.do(true, gateReq{path: "/", cookie: "pair-token"}), "a pairing token as the cookie")
	assertHidden(t, f.do(true, gateReq{path: "/", cookie: "guess"}), "an unknown cookie")
}

func TestUnpairedVisitorsCanStillPair(t *testing.T) {
	f := newGateFixture(t)
	for _, path := range []string{"/pair/", "/pair", "/pair/index.txt", "/_next/static/chunks/app.js", "/icons/icon-192.png", "/manifest.json", "/sw.js"} {
		if w := f.do(true, gateReq{path: path}); w.Code != http.StatusOK || w.Body.String() != "agentflow app" {
			t.Errorf("unpaired %s: %d %q", path, w.Code, w.Body.String())
		}
	}
	if w := f.do(true, gateReq{path: "/api/v1/agentd/health"}); w.Code != http.StatusOK {
		t.Errorf("unpaired health: %d", w.Code)
	}
	if w := f.do(true, gateReq{method: "POST", path: "/api/v1/agentd/pair/request", body: `{"name":"phone"}`}); w.Code != http.StatusCreated {
		t.Errorf("unpaired access request: %d %s", w.Code, w.Body.String())
	}
	if w := f.do(true, gateReq{method: "POST", path: "/api/v1/agentd/pair/complete", body: `{"token":"wrong"}`}); w.Code != http.StatusUnauthorized {
		t.Errorf("unpaired pair/complete: %d %s", w.Code, w.Body.String())
	}
}

func TestPairedCookieServesTheAppUntilTheDeviceIsRevoked(t *testing.T) {
	f := newGateFixture(t)
	for _, path := range []string{"/", "/session/", "/loop/"} {
		if w := f.do(true, gateReq{path: path, cookie: gateToken}); w.Code != http.StatusOK || w.Body.String() != "agentflow app" {
			t.Fatalf("paired %s: %d %q", path, w.Code, w.Body.String())
		}
	}
	if err := f.st.RevokeDevice(context.Background(), "dev-1"); err != nil {
		t.Fatal(err)
	}
	assertHidden(t, f.do(true, gateReq{path: "/", cookie: gateToken}), "revoked cookie")
}

func TestTheAPIStillRequiresTheHeaderWithAValidCookie(t *testing.T) {
	f := newGateFixture(t)
	w := f.do(true, gateReq{path: "/api/v1/agentd/devices", cookie: gateToken})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("cookie without a header: %d %s", w.Code, w.Body.String())
	}
	if w := f.do(true, gateReq{path: "/api/v1/agentd/devices", cookie: gateToken, auth: gateToken}); w.Code != http.StatusOK {
		t.Fatalf("cookie and header: %d %s", w.Code, w.Body.String())
	}
}

func TestLocalListenerIsNotGated(t *testing.T) {
	f := newGateFixture(t)
	if w := f.do(false, gateReq{path: "/"}); w.Code != http.StatusOK {
		t.Fatalf("local / without a cookie: %d", w.Code)
	}
	if w := f.do(false, gateReq{path: "/api/v1/agentd/sessions"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("local API without auth: %d", w.Code)
	}
}

func deviceCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: w.Header()}).Cookies() {
		if c.Name == agentapi.DeviceCookie {
			return c
		}
	}
	return nil
}

func TestPairingOnThePublicListenerSetsTheCookie(t *testing.T) {
	f := newGateFixture(t)
	w := f.do(true, gateReq{method: "POST", path: "/api/v1/agentd/pair/complete", body: `{"token":"pair-token","name":"phone"}`})
	if w.Code != http.StatusCreated {
		t.Fatalf("pair: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		DeviceToken string `json:"deviceToken"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	c := deviceCookie(t, w)
	if c == nil || c.Value != body.DeviceToken || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("cookie = %+v", c)
	}
	// The cookie it set opens the app.
	if w := f.do(true, gateReq{path: "/", cookie: c.Value}); w.Code != http.StatusOK {
		t.Fatalf("app with the new cookie: %d", w.Code)
	}
}

func TestApprovedAccessRequestSetsTheCookieOnThePublicListener(t *testing.T) {
	f := newGateFixture(t)
	w := f.do(true, gateReq{method: "POST", path: "/api/v1/agentd/pair/request", body: `{"name":"phone"}`})
	var created struct{ RequestID, PollSecret, MatchCode string }
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("request: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.api.DecidePairRequest(created.MatchCode, true, "test"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v1/agentd/pair/request/"+created.RequestID, nil)
	req.Host, req.RemoteAddr = gatePublicHost, "203.0.113.7:50000"
	req.Header.Set(agentapi.PairRequestPollHeader, created.PollSecret)
	w = httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	var polled struct{ Status, DeviceToken string }
	_ = json.Unmarshal(w.Body.Bytes(), &polled)
	if c := deviceCookie(t, w); polled.Status != "approved" || c == nil || c.Value != polled.DeviceToken || !c.HttpOnly || !c.Secure {
		t.Fatalf("poll %d %s, cookie %+v", w.Code, w.Body.String(), c)
	}
}

func TestLocalPairingSetsNoCookie(t *testing.T) {
	f := newGateFixture(t)
	w := f.do(false, gateReq{method: "POST", path: "/api/v1/agentd/pair/complete", body: `{"token":"pair-token","name":"desk"}`})
	if w.Code != http.StatusCreated || deviceCookie(t, w) != nil {
		t.Fatalf("local pair: %d, cookie %+v", w.Code, deviceCookie(t, w))
	}
}

func TestAPairedBrowserCanFetchItsCookie(t *testing.T) {
	f := newGateFixture(t)
	w := f.do(true, gateReq{method: "POST", path: "/api/v1/agentd/pair/cookie", auth: gateToken})
	if c := deviceCookie(t, w); w.Code != http.StatusNoContent || c == nil || c.Value != gateToken {
		t.Fatalf("pair/cookie: %d, cookie %+v", w.Code, c)
	}
	if w := f.do(true, gateReq{method: "POST", path: "/api/v1/agentd/pair/cookie", auth: "not-a-device"}); w.Code != http.StatusUnauthorized || deviceCookie(t, w) != nil {
		t.Fatalf("pair/cookie with a bad token: %d", w.Code)
	}
}
