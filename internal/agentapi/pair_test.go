package agentapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/store"
)

// TestPairComplete_RejectsBootstrapToken pins the invariant that the
// legacy `kind=agentd` row (a relay-bootstrap slot) must NOT pair phones
// anymore. The relay is gone, so the only legitimate pairing flow is a
// `kind=pairing` token minted by `agentflow pair`.
func TestPairComplete_RejectsBootstrapToken(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "pair-bootstrap.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const bootstrap = "the-bootstrap-token"
	_ = st.UpsertDevice(context.Background(), &store.Device{
		ID:        "agentd-bootstrap-m-test",
		Name:      "relay-bootstrap",
		MachineID: "m-test",
		Kind:      "agentd",
		Status:    "active",
		TokenHash: store.HashToken(bootstrap),
	}, 0)

	const pairTok = "the-pair-token"
	_ = st.UpsertDevice(context.Background(), &store.Device{
		ID:        "pair-abc",
		Name:      "qr-pair",
		MachineID: "m-test",
		Kind:      "pairing",
		Status:    "pending",
		TokenHash: store.HashToken(pairTok),
	}, time.Now().Add(time.Hour).UnixMilli())

	srvAPI := agentapi.New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "m-test")

	// 1. bootstrap token → 401 (was 201 under the relay)
	if code := doPairRequestRaw(t, srvAPI, bootstrap, "phone-1"); code != 401 {
		t.Fatalf("bootstrap pair: want 401, got %d", code)
	}

	// 2. pairing-kind token → still works
	body := doPairRequest(t, srvAPI, pairTok, "phone-2")
	if _, ok := body["deviceId"].(string); !ok {
		t.Fatalf("pairing-kind pair: %+v", body)
	}

	// 3. unknown token → 401
	if code := doPairRequestRaw(t, srvAPI, "never-issued", "phone-3"); code != 401 {
		t.Fatalf("unknown-token pair: want 401, got %d", code)
	}
}

func doPairRequest(t *testing.T, s *agentapi.Server, token, name string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"token": token, "name": name})
	req := httptest.NewRequest("POST", "/api/v1/agentd/pair/complete", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("pair %s: want 201, got %d %s", name, rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func doPairRequestRaw(t *testing.T, s *agentapi.Server, token, name string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"token": token, "name": name})
	req := httptest.NewRequest("POST", "/api/v1/agentd/pair/complete", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr.Code
}

// A pairing token minted by the running daemon (the path `agentflow pair` and
// `agentflow start` take) used to be stored with no expiry, so a QR code that
// was photographed stayed redeemable forever.
func TestMintedPairingTokenExpiresOnTheServer(t *testing.T) {
	srv, st := pairFixture(t)
	ctx := context.Background()

	tok, expiresAt, err := srv.MintPairingToken(ctx)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	want := time.Now().Add(agentapi.PairingTTL).UnixMilli()
	if expiresAt < want-5000 || expiresAt > want+5000 {
		t.Fatalf("returned expiry %d is not about %s from now", expiresAt, agentapi.PairingTTL)
	}
	var stored int64
	if err := st.DB().QueryRowContext(ctx, `SELECT expires_at FROM devices WHERE token_hash = ?`,
		store.HashToken(tok)).Scan(&stored); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if stored != expiresAt {
		t.Fatalf("stored expires_at = %d, want %d", stored, expiresAt)
	}

	// Once the expiry has passed the token no longer pairs.
	if _, err := st.DB().ExecContext(ctx, `UPDATE devices SET expires_at = ? WHERE token_hash = ?`,
		time.Now().Add(-time.Minute).UnixMilli(), store.HashToken(tok)); err != nil {
		t.Fatal(err)
	}
	if code := doPairRequestRaw(t, srv, tok, "late-phone"); code != 401 {
		t.Fatalf("expired pairing token paired with status %d", code)
	}
}
