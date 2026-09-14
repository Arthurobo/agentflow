package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func nowMS() int64 { return time.Now().UnixMilli() }

// TestManagedSessionsCRUD covers the agentd runtime state table (migration
// 0004): the spawner writes/re-reads state without touching /3 rows.
func TestManagedSessionsCRUD(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	m := &ManagedSession{
		ID: "run-a", SessionID: "sess-a", Kind: "chat", CWD: "/tmp/ws", Project: "ws",
		Model: "claude-fable-5", Prompt: "do a thing", State: "running", PID: 4242,
		EventCount: 12,
	}
	if err := s.UpsertManagedSession(ctx, m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetManagedSession(ctx, "run-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.State != "running" || got.EventCount != 12 || got.SessionID != "sess-a" {
		t.Fatalf("unexpected session: %+v", got)
	}

	bySID, err := s.GetManagedSessionBySessionID(ctx, "sess-a")
	if err != nil {
		t.Fatalf("get by sid: %v", err)
	}
	if bySID == nil || bySID.ID != "run-a" {
		t.Fatalf("expected run-a by session id, got %+v", bySID)
	}

	m.State = "crashed"
	m.ExitCode = 2
	m.PermissionDenial = 1
	if err := s.UpsertManagedSession(ctx, m); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, _ = s.GetManagedSession(ctx, "run-a")
	if got.State != "crashed" || got.ExitCode != 2 || got.PermissionDenial != 1 {
		t.Fatalf("update not applied: %+v", got)
	}

	list, err := s.ListManagedSessions(ctx, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 managed session, got %d", len(list))
	}
	only, err := s.ListManagedSessions(ctx, "crashed", 0)
	if err != nil || len(only) != 1 {
		t.Fatalf("expected 1 crashed session, got %d (%v)", len(only), err)
	}

	if err := s.DeleteManagedSession(ctx, "sess-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ = s.GetManagedSession(ctx, "sess-a")
	if got != nil {
		t.Fatalf("expected deleted session to be nil, got %+v", got)
	}
}

// TestDeviceRegistryCRUD covers pairing tokens + device tokens: storage is
// hash-only, verification is by re-hash, revocation forbids further use.
func TestDeviceRegistryCRUD(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	// pairing (single-use, expiring) then activation as a device
	pairing := &Device{ID: "pair-1", Name: "scan", Kind: "pairing", Status: "pending", TokenHash: HashToken("pair-secret")}
	if err := s.UpsertDevice(ctx, pairing, nowMS()+int64(60_000)); err != nil {
		t.Fatalf("upsert pairing: %v", err)
	}
	if d, _ := s.VerifyDevice(ctx, "pair-secret"); d == nil || d.ID != "pair-1" {
		t.Fatalf("pairing token should verify while pending: %+v", d)
	}

	dev := &Device{ID: "dev-1", Name: "test-phone", MachineID: "ws-01", Kind: "device", Status: "active", TokenHash: HashToken("dev-secret")}
	if err := s.UpsertDevice(ctx, dev, 0); err != nil {
		t.Fatalf("upsert device: %v", err)
	}
	if d, _ := s.VerifyDevice(ctx, "dev-secret"); d == nil || d.Kind != "device" {
		t.Fatalf("device token should verify: %+v", d)
	}
	if d, _ := s.VerifyDevice(ctx, "wrong"); d != nil {
		t.Fatalf("wrong token must not verify")
	}

	if err := s.RevokeDevice(ctx, "dev-1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if d, _ := s.VerifyDevice(ctx, "dev-secret"); d != nil {
		t.Fatalf("revoked device token must not verify")
	}
}
