package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

type pairReqFixture struct {
	srv *Server
	st  *store.Store
	mu  sync.Mutex
	now time.Time
}

func newPairReqFixture(t *testing.T) *pairReqFixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	st, err := store.Open(filepath.Join(dir, "pairreq.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &pairReqFixture{st: st, now: time.Now()}
	f.srv = New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "m-test")
	f.srv.pairReqs.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "dev-desk", Name: "desktop", MachineID: "m-test", Kind: "device",
		Status: "active", TokenHash: store.HashToken("desk-token"),
	}, 0); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	return f
}

func (f *pairReqFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

type createdPairRequest struct {
	RequestID  string `json:"requestId"`
	PollSecret string `json:"pollSecret"`
	MatchCode  string `json:"matchCode"`
	ExpiresAt  int64  `json:"expiresAt"`
}

func (f *pairReqFixture) request(t *testing.T, h http.Handler, ip, name string) (int, createdPairRequest) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	req := httptest.NewRequest("POST", "/api/v1/agentd/pair/request", strings.NewReader(string(body)))
	req.RemoteAddr = ip + ":5555"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out createdPairRequest
	if rr.Code == http.StatusCreated {
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rr.Code, out
}

func poll(t *testing.T, h http.Handler, id, secret string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/agentd/pair/request/"+id, nil)
	if secret != "" {
		req.Header.Set(PairRequestPollHeader, secret)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	out := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func TestPairRequestIsCreatedWithSecretsAndACode(t *testing.T) {
	f := newPairReqFixture(t)
	code, req := f.request(t, f.srv.PublicHandler(), "203.0.113.7", "iPhone · Safari")
	if code != http.StatusCreated {
		t.Fatalf("status %d", code)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(req.RequestID) {
		t.Errorf("requestId %q is not 128 random bits", req.RequestID)
	}
	if !regexp.MustCompile(`^[0-9a-f]{48}$`).MatchString(req.PollSecret) {
		t.Errorf("pollSecret %q is not 192 random bits", req.PollSecret)
	}
	if !regexp.MustCompile(`^[0-9]{4}$`).MatchString(req.MatchCode) {
		t.Errorf("matchCode %q is not four digits", req.MatchCode)
	}
	if want := f.now.Add(PairRequestTTL).UnixMilli(); req.ExpiresAt != want {
		t.Errorf("expiresAt %d, want %d", req.ExpiresAt, want)
	}
	// Only the secret's hash is kept.
	for _, r := range f.srv.pairReqs.byID {
		if strings.Contains(fmt.Sprintf("%+v", r), req.PollSecret) {
			t.Fatal("the poll secret is stored in the clear")
		}
	}
	list := f.srv.ListPairRequests()
	if len(list) != 1 || list[0].Name != "iPhone · Safari" || list[0].ClientIP != "203.0.113.7" || list[0].MatchCode != req.MatchCode {
		t.Fatalf("pending list %+v", list)
	}
}

func TestPairRequestPollHidesRequestsFromWrongSecrets(t *testing.T) {
	f := newPairReqFixture(t)
	h := f.srv.PublicHandler()
	_, req := f.request(t, h, "203.0.113.7", "phone")
	for name, c := range map[string]struct{ id, secret string }{
		"wrong secret":   {req.RequestID, strings.Repeat("0", 48)},
		"missing secret": {req.RequestID, ""},
		"unknown id":     {strings.Repeat("a", 32), req.PollSecret},
	} {
		if code, _ := poll(t, h, c.id, c.secret); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, code)
		}
	}
	if code, body := poll(t, h, req.RequestID, req.PollSecret); code != http.StatusOK || body["status"] != "pending" {
		t.Fatalf("right secret: %d %v", code, body)
	}
}

func TestApprovedPairRequestHandsOutItsTokenOnce(t *testing.T) {
	f := newPairReqFixture(t)
	h := f.srv.PublicHandler()
	_, req := f.request(t, h, "203.0.113.7", "iPhone · Safari")

	info, err := f.srv.DecidePairRequest(req.MatchCode, true, "admin socket")
	if err != nil || info.ID != req.RequestID {
		t.Fatalf("approve by match code: %+v %v", info, err)
	}
	if len(f.srv.ListPairRequests()) != 0 {
		t.Fatal("an approved request is still listed as pending")
	}

	code, body := poll(t, h, req.RequestID, req.PollSecret)
	token, _ := body["deviceToken"].(string)
	if code != http.StatusOK || body["status"] != "approved" || token == "" || body["deviceId"] == "" {
		t.Fatalf("approved poll: %d %v", code, body)
	}
	dev, err := f.st.VerifyClientDevice(context.Background(), token)
	if err != nil || dev == nil || dev.Name != "iPhone · Safari" || dev.ID != body["deviceId"] {
		t.Fatalf("the delivered token is not a working device token: %+v %v", dev, err)
	}
	if code, body := poll(t, h, req.RequestID, req.PollSecret); code != http.StatusNotFound {
		t.Fatalf("second poll after delivery: %d %v", code, body)
	}
}

func TestDeniedPairRequestSaysSo(t *testing.T) {
	f := newPairReqFixture(t)
	h := f.srv.PublicHandler()
	_, req := f.request(t, h, "203.0.113.7", "phone")
	if _, err := f.srv.DecidePairRequest(req.RequestID, false, "cli"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code, body := poll(t, h, req.RequestID, req.PollSecret); code != http.StatusOK || body["status"] != "denied" || body["deviceToken"] != nil {
			t.Fatalf("denied poll: %d %v", code, body)
		}
	}
	if _, err := f.srv.DecidePairRequest(req.RequestID, true, "cli"); !errors.Is(err, ErrPairRequestNotFound) {
		t.Fatalf("a denied request was approved afterwards: %v", err)
	}
}

func TestPairRequestExpiresAfterFiveMinutes(t *testing.T) {
	f := newPairReqFixture(t)
	h := f.srv.PublicHandler()
	_, req := f.request(t, h, "203.0.113.7", "phone")
	f.advance(PairRequestTTL - time.Second)
	if _, body := poll(t, h, req.RequestID, req.PollSecret); body["status"] != "pending" {
		t.Fatalf("before expiry: %v", body)
	}
	f.advance(time.Second)
	if code, body := poll(t, h, req.RequestID, req.PollSecret); code != http.StatusOK || body["status"] != "expired" {
		t.Fatalf("at expiry: %d %v", code, body)
	}
	if len(f.srv.ListPairRequests()) != 0 {
		t.Fatal("an expired request is still listed")
	}
	if _, err := f.srv.DecidePairRequest(req.MatchCode, true, "cli"); !errors.Is(err, ErrPairRequestNotFound) {
		t.Fatalf("an expired request was approved: %v", err)
	}
	// Long after, it's forgotten entirely.
	f.advance(2 * PairRequestTTL)
	if code, _ := poll(t, h, req.RequestID, req.PollSecret); code != http.StatusNotFound {
		t.Fatalf("long-expired poll: %d", code)
	}
}

func TestPairRequestsCapPendingAtFive(t *testing.T) {
	f := newPairReqFixture(t)
	h := f.srv.PublicHandler()
	var first createdPairRequest
	codes := map[string]bool{}
	for i := range 5 {
		code, req := f.request(t, h, fmt.Sprintf("198.51.100.%d", i+1), "phone")
		if code != http.StatusCreated {
			t.Fatalf("request %d: status %d", i, code)
		}
		if codes[req.MatchCode] {
			t.Fatalf("match code %s given to two pending requests", req.MatchCode)
		}
		codes[req.MatchCode] = true
		if i == 0 {
			first = req
		}
	}
	if code, _ := f.request(t, h, "198.51.100.99", "phone"); code != http.StatusTooManyRequests {
		t.Fatalf("sixth pending request: status %d, want 429", code)
	}
	if _, err := f.srv.DecidePairRequest(first.RequestID, false, "cli"); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.request(t, h, "198.51.100.99", "phone"); code != http.StatusCreated {
		t.Fatalf("after a decision frees a slot: status %d", code)
	}
}

func TestPairRequestsShareThePairingRateLimit(t *testing.T) {
	f := newPairReqFixture(t)
	h := f.srv.PublicHandler()
	got := []int{}
	for range 6 {
		code, req := f.request(t, h, "192.0.2.50", "phone")
		got = append(got, code)
		if code == http.StatusCreated {
			// Keep the pending cap out of the way.
			_, _ = f.srv.DecidePairRequest(req.RequestID, false, "test")
		}
	}
	if got[4] != http.StatusCreated || got[5] != http.StatusTooManyRequests {
		t.Fatalf("statuses %v, want five 201s then 429", got)
	}
}

func TestPairRequestNameIsCleaned(t *testing.T) {
	f := newPairReqFixture(t)
	f.request(t, f.srv.PublicHandler(), "203.0.113.7", "evil\x1b[31m\u202e name "+strings.Repeat("x", 100))
	list := f.srv.ListPairRequests()
	if len(list) != 1 || strings.ContainsAny(list[0].Name, "\x1b\u202e") || len([]rune(list[0].Name)) > maxPairRequestName {
		t.Fatalf("name %q", list[0].Name)
	}
	_, _ = f.srv.DecidePairRequest(list[0].ID, false, "test")
	f.request(t, f.srv.PublicHandler(), "203.0.113.8", "  ")
	if got := f.srv.ListPairRequests(); len(got) != 1 || got[0].Name != "Unnamed device" {
		t.Fatalf("blank name: %+v", got)
	}
}

func TestPairRequestApprovalRoutesAreLocalOnly(t *testing.T) {
	f := newPairReqFixture(t)
	_, req := f.request(t, f.srv.Handler(), "127.0.0.1", "phone")

	do := func(h http.Handler, method, path string) int {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer desk-token")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr.Code
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/agentd/pair/requests"},
		{"POST", "/api/v1/agentd/pair/requests/" + req.RequestID + "/approve"},
		{"POST", "/api/v1/agentd/pair/requests/" + req.RequestID + "/deny"},
	} {
		if code := do(f.srv.PublicHandler(), c.method, c.path); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("public %s %s: status %d, want the route to be absent", c.method, c.path, code)
		}
	}
	if len(f.srv.ListPairRequests()) != 1 {
		t.Fatal("the public listener decided a request")
	}

	// Locally, a paired browser can see and decide.
	r := httptest.NewRequest("GET", "/api/v1/agentd/pair/requests", nil)
	rr := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated local list: %d", rr.Code)
	}
	if code := do(f.srv.Handler(), "GET", "/api/v1/agentd/pair/requests"); code != http.StatusOK {
		t.Fatalf("local list: %d", code)
	}
	if code := do(f.srv.Handler(), "POST", "/api/v1/agentd/pair/requests/"+req.MatchCode+"/approve"); code != http.StatusOK {
		t.Fatalf("local approve: %d", code)
	}
	if code := do(f.srv.Handler(), "POST", "/api/v1/agentd/pair/requests/"+req.MatchCode+"/approve"); code != http.StatusNotFound {
		t.Fatalf("approving twice: %d", code)
	}
	if _, body := poll(t, f.srv.PublicHandler(), req.RequestID, req.PollSecret); body["status"] != "approved" {
		t.Fatalf("poll after local approval: %v", body)
	}
}

func TestOneClientCannotHoldEveryPendingSlot(t *testing.T) {
	f := newPairReqFixture(t)
	h := f.srv.PublicHandler()
	var first createdPairRequest
	for i := range maxPendingPerClient {
		code, req := f.request(t, h, "192.0.2.77", "phone")
		if code != http.StatusCreated {
			t.Fatalf("request %d from one client: %d", i, code)
		}
		if i == 0 {
			first = req
		}
	}
	if code, _ := f.request(t, h, "192.0.2.77", "phone"); code != http.StatusTooManyRequests {
		t.Fatalf("a third pending request from the same client: %d, want 429", code)
	}
	if code, _ := f.request(t, h, "192.0.2.78", "the real phone"); code != http.StatusCreated {
		t.Fatalf("another client is shut out: %d", code)
	}
	if _, err := f.srv.DecidePairRequest(first.RequestID, false, "test"); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.request(t, h, "192.0.2.77", "phone"); code != http.StatusCreated {
		t.Fatalf("after a decision the client may ask again: %d", code)
	}
}

func TestPairCompleteCleansTheDeviceName(t *testing.T) {
	f := newPairReqFixture(t)
	ctx := context.Background()
	for _, tc := range []struct{ token, name, want string }{
		{"pair-tok-1", "evil\x1b[2J\u202e phone " + strings.Repeat("y", 100), ""},
		{"pair-tok-2", "", "device-"},
	} {
		if err := f.st.UpsertDevice(ctx, &store.Device{ID: "pair-" + tc.token, Kind: "pairing", Status: "pending",
			TokenHash: store.HashToken(tc.token)}, time.Now().Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]string{"token": tc.token, "name": tc.name})
		rr := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/agentd/pair/complete", strings.NewReader(string(body))))
		var out struct {
			DeviceID string `json:"deviceId"`
		}
		if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &out) != nil {
			t.Fatalf("pair: %d %s", rr.Code, rr.Body.String())
		}
		dev, err := f.st.GetDeviceByID(ctx, out.DeviceID)
		if err != nil || dev == nil {
			t.Fatalf("device: %v", err)
		}
		if strings.ContainsAny(dev.Name, "\x1b\u202e") || len([]rune(dev.Name)) > maxPairRequestName || !strings.HasPrefix(dev.Name, tc.want) {
			t.Errorf("name %q for %q", dev.Name, tc.name)
		}
	}
}
