package remote

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSelectPicksTheTransport(t *testing.T) {
	off, err := Select(SelectConfig{})
	if err != nil || off.Status().State != StateOff || off.Status().Transport != "" {
		t.Fatalf("empty transport: %+v %v", off.Status(), err)
	}
	ts, err := Select(SelectConfig{Transport: TransportTailscale, Tailscale: Config{DataDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ts.Close() })
	if st := ts.Status(); st.Transport != TransportTailscale || st.State != StateStarting {
		t.Fatalf("tailscale before Run: %+v", st)
	}
	if _, err := Select(SelectConfig{Transport: "carrier-pigeon"}); !errors.Is(err, ErrUnknownTransport) {
		t.Fatalf("unknown transport: %v", err)
	}
}

func TestTransportLabel(t *testing.T) {
	if TransportLabel(TransportTailscale) != "Tailscale" || TransportLabel("") != "remote access" {
		t.Fatal("labels")
	}
}

func TestListenTunnelRefusesTheLocalListenerAndNonLoopback(t *testing.T) {
	for _, c := range []struct{ addr, local string }{
		{"127.0.0.1:4344", "127.0.0.1:4344"},
		{"127.0.0.1:4344", "localhost:4344"},
		{"127.0.0.1:4344", ":4344"},
		{"127.0.0.1:4344", "0.0.0.0:4344"},
		{"0.0.0.0:4345", "127.0.0.1:4344"},
		{"192.168.1.20:4345", "127.0.0.1:4344"},
		{"localhost:4345", "127.0.0.1:4344"},
		{"garbage", "127.0.0.1:4344"},
	} {
		if ln, err := ListenTunnel(c.addr, c.local); err == nil {
			_ = ln.Close()
			t.Errorf("ListenTunnel(%q, local %q) was allowed", c.addr, c.local)
		}
	}
	ln, err := ListenTunnel("127.0.0.1:0", "127.0.0.1:4344")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if ip := ln.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Fatalf("listening on %v", ln.Addr())
	}
}

func TestSameListenPort(t *testing.T) {
	cases := map[[2]string]bool{
		{"127.0.0.1:4344", "127.0.0.1:4344"}: true,
		{"127.0.0.1:4344", "[::]:4344"}:      true,
		{"[::1]:4344", "[::1]:4344"}:         true,
		{"127.0.0.1:4344", "127.0.0.1:4345"}: false,
		{"127.0.0.1:0", "127.0.0.1:0"}:       false,
		{"127.0.0.2:4344", "127.0.0.1:4344"}: false,
		{"bad", "127.0.0.1:4344"}:            false,
	}
	for c, want := range cases {
		if got := SameListenPort(c[0], c[1]); got != want {
			t.Errorf("SameListenPort(%q, %q) = %v, want %v", c[0], c[1], got, want)
		}
	}
}

func TestTrustForwardedClientOnlyTrustsTheTunnel(t *testing.T) {
	var seen string
	h := TrustForwardedClient(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = ClientIP(r)
	}), LastForwardedFor)

	serve := func(remoteAddr, header string) string {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remoteAddr
		if header != "" {
			req.Header.Set("X-Forwarded-For", header)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return seen
	}
	if got := serve("127.0.0.1:50000", "203.0.113.9"); got != "203.0.113.9" {
		t.Errorf("from the tunnel: %q", got)
	}
	if got := serve("[::1]:50000", "2001:db8::1"); got != "2001:db8::1" {
		t.Errorf("from the tunnel over IPv6: %q", got)
	}
	if got := serve("198.51.100.4:50000", "203.0.113.9"); got != "198.51.100.4" {
		t.Errorf("from elsewhere the header must be ignored: %q", got)
	}
	if got := serve("127.0.0.1:50000", "not-an-ip"); got != "127.0.0.1" {
		t.Errorf("a garbled header must be ignored: %q", got)
	}
	if got := serve("127.0.0.1:50000", ""); got != "127.0.0.1" {
		t.Errorf("no header: %q", got)
	}
}

func TestLastForwardedFor(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	if LastForwardedFor(req) != "" {
		t.Fatal("no header")
	}
	req.Header.Add("X-Forwarded-For", "6.6.6.6, 7.7.7.7")
	req.Header.Add("X-Forwarded-For", "203.0.113.9")
	if got := LastForwardedFor(req); got != "203.0.113.9" {
		t.Fatalf("got %q: the entry the local proxy appended is the last one", got)
	}
}

func TestRequireForwardedClientRefusesUnforwardedRequests(t *testing.T) {
	var seen []string
	h := RequireForwardedClient(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = append(seen, ClientIP(r))
	}), LastForwardedFor)
	serve := func(remoteAddr, xff string) int {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := serve("127.0.0.1:1", "203.0.113.9"); code != http.StatusOK {
		t.Fatalf("forwarded: %d", code)
	}
	for _, c := range []struct{ remote, xff string }{
		{"127.0.0.1:1", ""},
		{"127.0.0.1:1", "garbage"},
		{"198.51.100.4:1", "203.0.113.9"}, // not from this computer
	} {
		if code := serve(c.remote, c.xff); code != http.StatusMisdirectedRequest {
			t.Errorf("%+v: %d, want 421", c, code)
		}
	}
	if len(seen) != 1 || seen[0] != "203.0.113.9" {
		t.Fatalf("handler saw %v", seen)
	}
}
