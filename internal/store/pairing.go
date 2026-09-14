package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// MintPairingToken stores a one-shot pairing token for machineID that expires
// after ttl, and returns the token and its expiry in unix milliseconds. Only
// the token's hash is stored. The row has the same shape the daemon's own
// minting writes, so the CLI can mint while the daemon is stopped.
func (s *Store) MintPairingToken(ctx context.Context, machineID string, ttl time.Duration) (token string, expiresAt int64, err error) {
	tok := make([]byte, 24)
	idb := make([]byte, 6)
	if _, err := rand.Read(tok); err != nil {
		return "", 0, fmt.Errorf("pairing token: %w", err)
	}
	if _, err := rand.Read(idb); err != nil {
		return "", 0, fmt.Errorf("pairing id: %w", err)
	}
	token = hex.EncodeToString(tok)
	id := "pair-" + hex.EncodeToString(idb)
	expiresAt = time.Now().Add(ttl).UnixMilli()
	dev := &Device{
		ID: id, Name: "pairing-" + id[:6], MachineID: machineID,
		Kind: "pairing", Status: "pending", TokenHash: HashToken(token),
	}
	if err := s.UpsertDevice(ctx, dev, expiresAt); err != nil {
		return "", 0, err
	}
	return token, expiresAt, nil
}
