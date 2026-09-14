package cloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBaseURLFromEnv(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "AF_CLOUD_URL" {
				return v
			}
			return ""
		}
	}
	if got := BaseURLFromEnv(env("")); got != DefaultBaseURL {
		t.Errorf("unset: %q", got)
	}
	if got := BaseURLFromEnv(env(" http://127.0.0.1:8080/ ")); got != "http://127.0.0.1:8080" {
		t.Errorf("set: %q", got)
	}
}

func TestClientSendsTheContract(t *testing.T) {
	f := newFakeService(t)
	old := Version
	Version = "1.2.3"
	defer func() { Version = old }()

	c := NewClient(f.srv.URL + "/")
	ctx := context.Background()
	if err := c.RequestCode(ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	token, err := c.Verify(ctx, "a@example.com", "111111")
	if err != nil || token != strings.Repeat("t", 43) {
		t.Fatalf("verify: %q, %v", token, err)
	}
	m := Machine{ID: "0123abcd", Name: "workstation", Transport: "tailscale", URL: "https://laptop.tail1234.ts.net:8443", Version: "1.2.3"}
	if err := c.PutMachine(ctx, token, m); err != nil {
		t.Fatal(err)
	}
	if err := c.Heartbeat(ctx, token, "0123abcd"); err != nil {
		t.Fatal(err)
	}
	if err := c.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	want := []string{"POST /v1/auth/code", "POST /v1/auth/verify", "PUT /v1/machines/0123abcd", "POST /v1/machines/0123abcd/heartbeat", "POST /v1/auth/logout"}
	if len(f.requests) != len(want) {
		t.Fatalf("%d requests, want %d", len(f.requests), len(want))
	}
	for i, r := range f.requests {
		if got := r.Method + " " + r.URL.Path; got != want[i] {
			t.Errorf("request %d = %s, want %s", i, got, want[i])
		}
		if ua := r.Header.Get("User-Agent"); ua != "agentflow/1.2.3" {
			t.Errorf("request %d User-Agent = %q", i, ua)
		}
		authed := i >= 2
		if got := r.Header.Get("Authorization"); authed != (got == "Bearer "+token) {
			t.Errorf("request %d Authorization = %q", i, got)
		}
	}
	put := f.puts[0]
	if put["name"] != "workstation" || put["transport"] != "tailscale" || put["url"] != "https://laptop.tail1234.ts.net:8443" || put["agentflowVersion"] != "1.2.3" {
		t.Errorf("machine body = %v", put)
	}
	if f.logouts != 1 {
		t.Errorf("logouts = %d", f.logouts)
	}
}

func TestClientMapsErrors(t *testing.T) {
	f := newFakeService(t)
	c := NewClient(f.srv.URL)
	ctx := context.Background()

	err := c.RequestCode(ctx, "x@invalid.example")
	var apiErr *APIError
	if !errors.Is(err, ErrInvalidEmail) || !errors.As(err, &apiErr) || apiErr.Status != 400 || apiErr.Code != "invalid_email" {
		t.Fatalf("invalid email: %v", err)
	}
	if _, err := c.Verify(ctx, "a@example.com", "000000"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("invalid code: %v", err)
	}
	if err := c.Heartbeat(ctx, "bogus", "m"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized: %v", err)
	}
	f.setRateLimit(true)
	if err := c.RequestCode(ctx, "a@example.com"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate limited: %v", err)
	}

	f.mu.Lock()
	f.tokens["tok"] = "a@example.com"
	f.mu.Unlock()
	if err := c.PutMachine(ctx, "tok", Machine{ID: "taken"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not found: %v", err)
	}

	// A non-JSON error still becomes an APIError with the status.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer plain.Close()
	err = NewClient(plain.URL).RequestCode(ctx, "a@example.com")
	if !errors.As(err, &apiErr) || apiErr.Status != 502 {
		t.Fatalf("plain error: %v", err)
	}
}

func TestLogoutOfUnknownTokenSucceeds(t *testing.T) {
	f := newFakeService(t)
	if err := NewClient(f.srv.URL).Logout(context.Background(), "already-revoked"); err != nil {
		t.Fatalf("logout with a revoked token: %v", err)
	}
}

func TestNewClientRefusesPlainHTTPToRemoteHosts(t *testing.T) {
	ctx := context.Background()
	for _, base := range []string{"http://cloud.example.com", "ftp://cloud.example.com", "cloud.example.com"} {
		err := NewClient(base).RequestCode(ctx, "a@example.com")
		if err == nil || !strings.Contains(err.Error(), "https") && !strings.Contains(err.Error(), "absolute") {
			t.Errorf("%s: err = %v, want refusal before any request", base, err)
		}
	}
	for _, base := range []string{"", "https://cloud.example.com", "http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		if err := NewClient(base).(*httpClient).baseErr; err != nil {
			t.Errorf("%q refused: %v", base, err)
		}
	}
	if c := NewClient("").(*httpClient); c.base != DefaultBaseURL || c.http.Timeout != requestTimeout {
		t.Errorf("default client base %q timeout %s", c.base, c.http.Timeout)
	}
}

func TestClientBoundsResponse(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"token":"`+strings.Repeat("x", 2*maxResponse)+`"}`)
	}))
	defer big.Close()
	if _, err := NewClient(big.URL).Verify(context.Background(), "a@example.com", "123456"); err == nil {
		t.Fatal("an oversized response was accepted")
	}
}
