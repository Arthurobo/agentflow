package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMintPairingTokenStoresPendingOneShotRow(t *testing.T) {
	st, err := storeOpenT(t)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	before := time.Now()
	token, expiresAt, err := st.MintPairingToken(ctx, "laptop", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 48 {
		t.Fatalf("token length %d", len(token))
	}
	if exp := time.UnixMilli(expiresAt); exp.Before(before.Add(14*time.Minute)) || exp.After(time.Now().Add(16*time.Minute)) {
		t.Fatalf("expiresAt %v not ~15 min out", exp)
	}
	d, err := st.VerifyDevice(ctx, token)
	if err != nil || d == nil {
		t.Fatalf("minted token does not verify: %v %v", d, err)
	}
	if d.Kind != "pairing" || d.Status != "pending" || d.MachineID != "laptop" || !strings.HasPrefix(d.ID, "pair-") {
		t.Fatalf("row = %+v", d)
	}
	// A pairing token is never a client credential.
	if c, _ := st.VerifyClientDevice(ctx, token); c != nil {
		t.Fatal("pairing token accepted as a device token")
	}
	if ok, err := st.ConsumePairingToken(ctx, d.ID); err != nil || !ok {
		t.Fatalf("consume: %v %v", ok, err)
	}
	if ok, _ := st.ConsumePairingToken(ctx, d.ID); ok {
		t.Fatal("token consumed twice")
	}
}
