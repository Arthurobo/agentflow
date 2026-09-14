package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"
)

// HashToken digests a device/pairing/agentd token for storage. Devices tables
// NEVER store plaintext long-lived secrets: we keep only the
// SHA-256 hex digest, and verify by re-hashing the presented token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Device is one trusted client in agentd's device registry (migration 0004).
// kind: device (API token), pairing (single-use expiring QR token), agentd
// (machine credential for the live-server relay).
type Device struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	MachineID string `json:"machineId,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Status    string `json:"status,omitempty"` // pending | active | revoked
	CreatedAt int64  `json:"createdAt,omitempty"`
	LastSeen  int64  `json:"lastSeen,omitempty"`
	// TokenHash is populated on create/verify (never serialized).
	TokenHash string `json:"-"`
}

// UpsertDevice writes a device row (token_hash always stored hashed).
func (s *Store) UpsertDevice(ctx context.Context, d *Device, expiresAt int64) error {
	if d.Status == "" {
		d.Status = "active"
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `INSERT INTO devices (
		id, name, machine_id, kind, token_hash, expires_at, status, created_at, last_seen, revoked_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
	ON CONFLICT(id) DO UPDATE SET
		name = excluded.name,
		machine_id = excluded.machine_id,
		kind = excluded.kind,
		token_hash = excluded.token_hash,
		expires_at = excluded.expires_at,
		status = excluded.status,
		last_seen = excluded.last_seen`,
		d.ID, d.Name, d.MachineID, d.Kind, d.TokenHash, expiresAt, d.Status, now, now)
	return err
}

// TouchDevice updates last_seen for an active device.
func (s *Store) TouchDevice(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE devices SET last_seen = ? WHERE id = ? AND status = 'active'`,
		time.Now().UnixMilli(), id)
	return err
}

// VerifyDevice checks a presented token against the registry: active, not
// expired, hash matches. Returns the device or nil.
func (s *Store) VerifyDevice(ctx context.Context, token string) (*Device, error) {
	hash := HashToken(token)
	row := s.db.QueryRowContext(ctx, `SELECT id, name, machine_id, kind, token_hash,
		expires_at, status, created_at, last_seen FROM devices
		WHERE token_hash = ? AND status != 'revoked'`, hash)
	var d Device
	var expiresAt int64
	if err := row.Scan(&d.ID, &d.Name, &d.MachineID, &d.Kind, &d.TokenHash,
		&expiresAt, &d.Status, &d.CreatedAt, &d.LastSeen); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if expiresAt > 0 && time.Now().UnixMilli() > expiresAt {
		return nil, nil
	}
	// Device tokens live on a 30-day SLIDING window: every authenticated call
	// slides last_seen (TouchDevice), so a phone in regular use never re-pairs.
	// An idle token dies after 30 days and the device re-pairs with a fresh
	// `agentd pair` token. Pairing/machine credentials keep their own
	// expires_at semantics.
	if d.Kind == "device" {
		last := d.LastSeen
		if last == 0 {
			last = d.CreatedAt
		}
		if time.Now().UnixMilli()-last > DeviceTokenTTL.Milliseconds() {
			return nil, nil
		}
	}
	return &d, nil
}

// DeviceTokenTTL is how long a device token stays valid without use.
const DeviceTokenTTL = 30 * 24 * time.Hour

// VerifyClientDevice is the auth seam used by the agentd HTTP API. It is
// strictly tighter than VerifyDevice: it ONLY accepts rows of
// `kind="device"` with `status="active"`. Pairing tokens, the legacy
// `kind="agentd"` slot, and any pending/revoked row are rejected.
//
// `handlePairComplete` accepts ONLY kind=pairing tokens, so the
// pairing and device-auth paths never share a verifier.
func (s *Store) VerifyClientDevice(ctx context.Context, token string) (*Device, error) {
	d, err := s.VerifyDevice(ctx, token)
	if err != nil || d == nil {
		return nil, err
	}
	if d.Kind != "device" || d.Status != "active" {
		return nil, nil
	}
	return d, nil
}

// ListDevices returns devices filtered by kind ("" = all) with their hashed
// token marker (the hash is not secret enough alone to be useful, but callers
// must never expose it).
func (s *Store) ListDevices(ctx context.Context, kind string, limit int) ([]*Device, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, name, machine_id, kind, token_hash, expires_at, status, created_at, last_seen
		FROM devices`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Device{}
	for rows.Next() {
		var d Device
		var expiresAt int64
		if err := rows.Scan(&d.ID, &d.Name, &d.MachineID, &d.Kind, &d.TokenHash,
			&expiresAt, &d.Status, &d.CreatedAt, &d.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// RevokeDevice marks a device revoked (forgets its token by clearing the
// hash). E2EE device_keys rows are removed by migration 0028 along with
// the relay tables.
func (s *Store) RevokeDevice(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE devices SET status = 'revoked', revoked_at = ?, token_hash = '' WHERE id = ?`,
		time.Now().UnixMilli(), id)
	return err
}

// ConsumePairingToken atomically marks a one-shot pairing row consumed.
// Returns true if THIS call was the one that consumed it; false if it was
// already revoked (concurrent pair, expired, etc).
//
// Used to close the race between two /pair/complete calls racing the same
// 15-minute QR: without this CAS, both pairs succeed and the operator
// unwittingly enrolls two devices under one shoulder-surfable token.
func (s *Store) ConsumePairingToken(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET status = 'revoked', revoked_at = ?, token_hash = ''
		 WHERE id = ? AND status = 'pending'`,
		time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetDeviceByID returns one device by id regardless of status.
func (s *Store) GetDeviceByID(ctx context.Context, id string) (*Device, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, machine_id, kind, token_hash,
		expires_at, status, created_at, last_seen FROM devices WHERE id = ?`, id)
	var d Device
	var expiresAt int64
	if err := row.Scan(&d.ID, &d.Name, &d.MachineID, &d.Kind, &d.TokenHash,
		&expiresAt, &d.Status, &d.CreatedAt, &d.LastSeen); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}
