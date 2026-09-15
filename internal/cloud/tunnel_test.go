package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
)

// fakeProvision mimics POST /v1/provision: one tunnel per machine id, bound to
// the first secret it saw.
type fakeProvision struct {
	mu      sync.Mutex
	secrets map[string]string // machine id -> secret
	bodies  []map[string]string
	status  int // when set, every call answers this status
	token   string
}

func newFakeProvision(t *testing.T) (*fakeProvision, *httptest.Server) {
	t.Helper()
	f := &fakeProvision{secrets: map[string]string{}, token: "tok-1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/provision" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.bodies = append(f.bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":{"code":"provisioning_failed","message":"try again"}}`))
			return
		}
		id, secret := body["machineId"], body["provisionSecret"]
		if have, ok := f.secrets[id]; ok && have != secret {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"machine_provisioned","message":"send the secret"}}`))
			return
		}
		f.secrets[id] = secret
		_ = json.NewEncoder(w).Encode(map[string]string{"hostname": "Brave-Otter-0042.useagentflow.xyz", "tunnelToken": f.token, "transport": "cloudflare"})
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestTunnelSourceFetchesAndCaches(t *testing.T) {
	f, srv := newFakeProvision(t)
	dir := t.TempDir()
	src := &TunnelSource{DataDir: dir, MachineName: "nucbox", Client: NewProvisionClient(srv.URL)}
	ctx := context.Background()

	if _, ok := src.Cached(); ok {
		t.Fatal("cached tunnel before any fetch")
	}
	g, err := src.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g.Hostname != "brave-otter-0042.useagentflow.xyz" || g.Token != "tok-1" {
		t.Fatalf("grant = %+v", g)
	}
	if c, ok := src.Cached(); !ok || c != g {
		t.Fatalf("cached = %+v, %v", c, ok)
	}

	id, err := MachineID(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := f.bodies[0]
	if first["machineId"] != id || first["hostname"] != "nucbox" || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(first["provisionSecret"]) {
		t.Fatalf("request body = %v", first)
	}

	// The token is rotated server-side: the next fetch sends the same secret
	// and stores the new token.
	f.token = "tok-2"
	g, err = src.Fetch(ctx)
	if err != nil || g.Token != "tok-2" {
		t.Fatalf("second fetch: %+v, %v", g, err)
	}
	if f.bodies[1]["provisionSecret"] != first["provisionSecret"] {
		t.Fatal("the secret changed between fetches")
	}
	if c, _ := src.Cached(); c.Token != "tok-2" {
		t.Fatalf("cache not updated: %+v", c)
	}

	info, err := os.Stat(filepath.Join(dir, TunnelFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("tunnel file: %v, %v", info, err)
	}
}

// The secret is saved before the request, so a failed call is retried with
// the same one and the cache is left alone.
func TestTunnelSourceFailureKeepsSecretAndCache(t *testing.T) {
	f, srv := newFakeProvision(t)
	dir := t.TempDir()
	src := &TunnelSource{DataDir: dir, MachineName: "box", Client: NewProvisionClient(srv.URL)}
	ctx := context.Background()

	f.status = http.StatusBadGateway
	if _, err := src.Fetch(ctx); err == nil {
		t.Fatal("fetch succeeded against a failing service")
	}
	if _, ok := src.Cached(); ok {
		t.Fatal("a failed fetch left a cached tunnel")
	}
	f.status = 0
	if _, err := src.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if f.bodies[0]["provisionSecret"] != f.bodies[1]["provisionSecret"] {
		t.Fatal("retry used a new secret")
	}

	f.status = http.StatusServiceUnavailable
	if _, err := src.Fetch(ctx); err == nil {
		t.Fatal("expected an error")
	}
	if c, ok := src.Cached(); !ok || c.Token != "tok-1" {
		t.Fatalf("cache after a failed refresh: %+v, %v", c, ok)
	}
}

func TestTunnelSourceSecretMismatch(t *testing.T) {
	_, srv := newFakeProvision(t)
	dir := t.TempDir()
	src := &TunnelSource{DataDir: dir, MachineName: "box", Client: NewProvisionClient(srv.URL)}
	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Losing the tunnel file but keeping the machine id.
	if err := os.Remove(filepath.Join(dir, TunnelFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Fetch(context.Background()); !errors.Is(err, ErrTunnelSecretMismatch) {
		t.Fatalf("err = %v, want ErrTunnelSecretMismatch", err)
	}
}

func TestProvisionTunnelRejectsBadAnswers(t *testing.T) {
	for name, body := range map[string]string{
		"no token":         `{"hostname":"a-b-0001.useagentflow.xyz","tunnelToken":""}`,
		"bad hostname":     `{"hostname":"https://x","tunnelToken":"t"}`,
		"token with space": `{"hostname":"a-b-0001.useagentflow.xyz","tunnelToken":"a b"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			if _, err := NewProvisionClient(srv.URL).ProvisionTunnel(context.Background(), "id", "h", "s"); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
