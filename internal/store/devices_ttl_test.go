package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestDeviceTokenSlidingTTL pins the 30-day sliding device-token contract: a
// recently used token verifies forever (the window slides on every auth), an
// idle one beyond 30 days is dead until re-paired with a fresh `agentd pair`
// token, and non-device kinds keep their expires_at semantics.
func TestDeviceTokenSlidingTTL(t *testing.T) {
	st, err := storeOpenT(t)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	const tok = "ttl-token"
	dev := &Device{ID: "d-ttl", Name: "phone", Kind: "device", Status: "active",
		TokenHash: HashToken(tok)}
	if err := st.UpsertDevice(ctx, dev, 0); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// fresh device: verifies (created/last_seen = now)
	if d, err := st.VerifyDevice(ctx, tok); err != nil || d == nil {
		t.Fatalf("fresh token must verify: %v %v", d, err)
	}

	// used 29 days ago: still inside the sliding window
	backdate(ctx, t, st, "d-ttl", 29*24*time.Hour)
	if d, _ := st.VerifyDevice(ctx, tok); d == nil {
		t.Fatal("token used 29d ago must still verify")
	}

	// idle 31 days: dead
	backdate(ctx, t, st, "d-ttl", 31*24*time.Hour)
	if d, _ := st.VerifyDevice(ctx, tok); d != nil {
		t.Fatal("token idle 31d must be rejected")
	}

	// pairing tokens are NOT subject to the inactivity window (their own
	// expires_at governs them)
	const ptok = "pair-token"
	pdev := &Device{ID: "d-pair", Kind: "pairing", Status: "pending",
		TokenHash: HashToken(ptok)}
	if err := st.UpsertDevice(ctx, pdev, time.Now().Add(5*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("upsert pairing: %v", err)
	}
	backdate(ctx, t, st, "d-pair", 40*24*time.Hour)
	if d, _ := st.VerifyDevice(ctx, ptok); d == nil {
		t.Fatal("pairing token inside its own expires_at must verify")
	}
}

// backdate rewinds a device row's created_at/last_seen for TTL tests.
func backdate(ctx context.Context, t *testing.T, st *Store, id string, d time.Duration) {
	t.Helper()
	ts := time.Now().Add(-d).UnixMilli()
	if _, err := st.db.ExecContext(ctx,
		`UPDATE devices SET created_at = ?, last_seen = ? WHERE id = ?`, ts, ts, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func storeOpenT(t *testing.T) (*Store, error) {
	t.Helper()
	return Open(filepath.Join(t.TempDir(), "test.db"))
}
