package adminsock

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recorder struct {
	logouts   int
	logoutErr error
	revoked   []string
	mints     int
	decisions []string
	reloads   int
}

func (r *recorder) config(dir string) Config {
	return Config{
		Dir:    dir,
		Status: func() any { return map[string]any{"state": "running", "publicUrl": "https://x.ts.net"} },
		Logout: func(context.Context) error {
			if r.logoutErr != nil {
				return r.logoutErr
			}
			r.logouts++
			return nil
		},
		Revoke: func(_ context.Context, id string) (int, error) {
			if id == "missing" {
				return 0, ErrNotFound
			}
			r.revoked = append(r.revoked, id)
			return 2, nil
		},
		MintPair: func(context.Context) (string, int64, error) {
			r.mints++
			return "tok-123", 1757000000000, nil
		},
		Health: func() any { return map[string]any{"status": "ok"} },
		PairRequests: func() []PairRequest {
			return []PairRequest{{ID: "req-1", Name: "iPhone · Safari", MatchCode: "4821", ClientIP: "203.0.113.7"}}
		},
		DecidePairRequest: func(_ context.Context, idOrCode string, approve bool) (PairRequest, error) {
			if idOrCode != "req-1" && idOrCode != "4821" {
				return PairRequest{}, ErrNotFound
			}
			verb := "deny"
			if approve {
				verb = "approve"
			}
			r.decisions = append(r.decisions, verb+" "+idOrCode)
			return PairRequest{ID: "req-1", Name: "iPhone · Safari", MatchCode: "4821"}, nil
		},
		ReloadAccount: func() any {
			r.reloads++
			return map[string]any{"reporting": true, "email": "a@example.com"}
		},
	}
}

func startTest(t *testing.T) (*Server, *Client, *recorder, string) {
	t.Helper()
	dataDir := t.TempDir()
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := Start(ctx, rec.config(Dir(dataDir)))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, NewClient(SocketPath(dataDir)), rec, dataDir
}

func TestSocketAndDirectoryPermissions(t *testing.T) {
	srv, _, _, dataDir := startTest(t)
	if srv.Path() != filepath.Join(dataDir, "run", "agentflow.sock") {
		t.Fatalf("socket path = %s", srv.Path())
	}
	fi, err := os.Stat(srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v", fi.Mode())
	}
	di, err := os.Stat(filepath.Dir(srv.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode = %v", di.Mode().Perm())
	}
}

func TestExistingLooseDirectoryIsTightened(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(Dir(dataDir), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	srv, err := Start(context.Background(), rec.config(Dir(dataDir)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	di, _ := os.Stat(Dir(dataDir))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", di.Mode().Perm())
	}
}

func TestStaleSocketIsReplacedButRegularFileIsNot(t *testing.T) {
	dataDir := t.TempDir()
	dir := Dir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A socket left behind by a crashed daemon: bound, then abandoned.
	stale, err := net.Listen("unix", filepath.Join(dir, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()

	rec := &recorder{}
	srv, err := Start(context.Background(), rec.config(dir))
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	var out map[string]any
	if err := NewClient(srv.Path()).Do(context.Background(), "GET", "/health", &out); err != nil {
		t.Fatal(err)
	}
	_ = srv.Close()

	if err := os.WriteFile(filepath.Join(dir, SocketName), []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), rec.config(dir)); err == nil {
		t.Fatal("Start removed a regular file at the socket path")
	}
}

func TestStatusAndHealth(t *testing.T) {
	_, c, _, _ := startTest(t)
	var st struct {
		State     string `json:"state"`
		PublicURL string `json:"publicUrl"`
	}
	if err := c.Do(context.Background(), "GET", "/remote/status", &st); err != nil {
		t.Fatal(err)
	}
	if st.State != "running" || st.PublicURL != "https://x.ts.net" {
		t.Fatalf("status = %+v", st)
	}
	var h map[string]any
	if err := c.Do(context.Background(), "GET", "/health", &h); err != nil || h["status"] != "ok" {
		t.Fatalf("health = %v %v", h, err)
	}
}

func TestLogoutCallsLogout(t *testing.T) {
	_, c, rec, _ := startTest(t)
	if err := c.Do(context.Background(), "POST", "/remote/logout", nil); err != nil {
		t.Fatal(err)
	}
	if rec.logouts != 1 {
		t.Fatalf("logout calls = %d", rec.logouts)
	}
	rec.logoutErr = errors.New("remote access is off")
	err := c.Do(context.Background(), "POST", "/remote/logout", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 || apiErr.Message != "remote access is off" {
		t.Fatalf("failed logout err = %v", err)
	}
}

func TestMintReturnsTokenAndExpiry(t *testing.T) {
	_, c, rec, _ := startTest(t)
	var out MintResponse
	if err := c.Do(context.Background(), "POST", "/pair/mint", &out); err != nil {
		t.Fatal(err)
	}
	if out.Token != "tok-123" || out.ExpiresAt != 1757000000000 || rec.mints != 1 {
		t.Fatalf("mint = %+v (calls %d)", out, rec.mints)
	}
	if err := c.Do(context.Background(), "GET", "/pair/mint", nil); err == nil {
		t.Fatal("GET /pair/mint should not mint")
	}
	if rec.mints != 1 {
		t.Fatal("GET minted a token")
	}
}

func TestRevokeReportsClosedConnections(t *testing.T) {
	_, c, rec, _ := startTest(t)
	var out RevokeResponse
	if err := c.Do(context.Background(), "POST", "/devices/dev-1/revoke", &out); err != nil {
		t.Fatal(err)
	}
	if out.Revoked != "dev-1" || out.ConnectionsClosed != 2 || len(rec.revoked) != 1 {
		t.Fatalf("revoke = %+v %v", out, rec.revoked)
	}
	err := c.Do(context.Background(), "POST", "/devices/missing/revoke", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("unknown device err = %v", err)
	}
}

func TestClientReportsNotRunning(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "run", SocketName))
	if err := c.Do(context.Background(), "GET", "/health", nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}

func TestCloseRemovesSocket(t *testing.T) {
	srv, c, _, _ := startTest(t)
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(srv.Path()); !os.IsNotExist(err) {
		t.Fatalf("socket still present: %v", err)
	}
	if err := c.Do(context.Background(), "GET", "/health", nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err after close = %v", err)
	}
}

func TestStartRequiresEveryHandler(t *testing.T) {
	cfg := (&recorder{}).config(t.TempDir())
	cfg.Logout = nil
	if _, err := Start(context.Background(), cfg); err == nil {
		t.Fatal("Start accepted a config without Logout")
	}
}

// A socket path longer than the kernel's sun_path fails to bind with
// "invalid argument", which left a daemon under a deep home directory without
// its admin socket (and the CLI silently falling back to the database).
func TestDirStaysBindableUnderADeepDataDir(t *testing.T) {
	short := "/home/user/.local/share/agentflow"
	if got := Dir(short); got != filepath.Join(short, "run") {
		t.Fatalf("Dir(%q) = %q, want the run directory beside the data", short, got)
	}
	deep := "/" + strings.Repeat("very-long-directory-name/", 8) + "agentflow"
	d := Dir(deep)
	if p := filepath.Join(d, SocketName); len(p) > maxSocketPath {
		t.Fatalf("socket path %q is %d bytes, over %d", p, len(p), maxSocketPath)
	}
	if d != Dir(deep) {
		t.Fatal("Dir is not stable for the same data directory")
	}
	if Dir(deep+"-other") == d {
		t.Fatal("two data directories share a fallback socket directory")
	}
}

func TestStartBindsUnderADeepDataDir(t *testing.T) {
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 60), strings.Repeat("e", 60))
	dir := Dir(deep)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	srv, err := Start(context.Background(), (&recorder{}).config(dir))
	if err != nil {
		t.Fatalf("Start under a deep data dir: %v", err)
	}
	defer func() { _ = srv.Close() }()
	if _, err := os.Stat(srv.Path()); err != nil {
		t.Fatalf("socket not created: %v", err)
	}
}

func TestStartRefusesADirectoryOthersCanReach(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if srv, err := Start(context.Background(), (&recorder{}).config(link)); err == nil {
		_ = srv.Close()
		t.Fatal("Start accepted a symlinked socket directory")
	}
}

func TestPairRequestsListAndDecide(t *testing.T) {
	_, c, rec, _ := startTest(t)
	ctx := context.Background()
	var list PairRequestsResponse
	if err := c.Do(ctx, "GET", "/pair/requests", &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Requests) != 1 || list.Requests[0].MatchCode != "4821" || list.Requests[0].Name != "iPhone · Safari" {
		t.Fatalf("list = %+v", list)
	}
	var dec DecisionResponse
	if err := c.Do(ctx, "POST", "/pair/requests/4821/approve", &dec); err != nil {
		t.Fatal(err)
	}
	if dec.Decision != "approved" || dec.Request.ID != "req-1" {
		t.Fatalf("approve = %+v", dec)
	}
	if err := c.Do(ctx, "POST", "/pair/requests/req-1/deny", &dec); err != nil || dec.Decision != "denied" {
		t.Fatalf("deny = %+v %v", dec, err)
	}
	var apiErr *APIError
	if err := c.Do(ctx, "POST", "/pair/requests/9999/approve", nil); !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("unknown request: %v", err)
	}
	if strings.Join(rec.decisions, ",") != "approve 4821,deny req-1" {
		t.Fatalf("decisions = %v", rec.decisions)
	}
}

func TestReloadAccount(t *testing.T) {
	_, c, rec, _ := startTest(t)
	var out struct {
		Reporting bool   `json:"reporting"`
		Email     string `json:"email"`
	}
	if err := c.Do(context.Background(), "POST", "/account/reload", &out); err != nil {
		t.Fatal(err)
	}
	if rec.reloads != 1 || !out.Reporting || out.Email != "a@example.com" {
		t.Fatalf("reloads=%d out=%+v", rec.reloads, out)
	}
}
