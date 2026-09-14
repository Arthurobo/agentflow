package loopapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

// Only an active paired device reaches the engineer surface. A pairing token
// is a one-shot credential for /pair/complete, and anything else that
// happens to live in the devices table is not an engineer either.
func TestEngineerSurfaceRefusesEverythingButAnActiveDevice(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seed := func(id, kind, status, token string, expires int64) {
		t.Helper()
		if err := h.st.UpsertDevice(ctx, &store.Device{
			ID: id, Kind: kind, Status: status, TokenHash: store.HashToken(token),
		}, expires); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("pair-1", "pairing", "pending", "pending-pairing-token", time.Now().Add(15*time.Minute).UnixMilli())
	seed("agentd-1", "agentd", "active", "machine-token", 0)
	seed("dev-revoked", "device", "active", "revoked-device-token", 0)
	if err := h.st.RevokeDevice(ctx, "dev-revoked"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	for name, tok := range map[string]string{
		"pending pairing token": "pending-pairing-token",
		"agentd kind token":     "machine-token",
		"revoked device":        "revoked-device-token",
		"no token":              "",
	} {
		resp, _ := h.do(tok, "GET", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: GET %s = %d, want 401", name, Prefix, resp.StatusCode)
		}
	}
	if resp, _ := h.do(h.device, "GET", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("active device: GET %s = %d, want 200", Prefix, resp.StatusCode)
	}
}
