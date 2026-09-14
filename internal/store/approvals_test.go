package store

import (
	"context"
	"testing"
	"time"
)

// A row that has left pending is final: a late decision must not overwrite an
// expiry, and the caller learns that nothing changed.
func TestResolveApprovalOnlyMovesPendingRows(t *testing.T) {
	st, err := storeOpenT(t)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := st.UpsertApproval(ctx, &Approval{
		ID: "ap-1", RunID: "run-1", ToolName: "Bash", State: ApprovalPending,
		CreatedAt: now, ExpiresAt: now + 60_000,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ok, err := st.ResolvePendingApproval(ctx, "ap-1", ApprovalExpired, "timeout")
	if err != nil || !ok {
		t.Fatalf("first resolve: ok=%v err=%v", ok, err)
	}
	ok, err = st.ResolvePendingApproval(ctx, "ap-1", ApprovalApproved, "device:late")
	if err != nil || ok {
		t.Fatalf("late resolve: ok=%v err=%v, want not applied", ok, err)
	}
	if err := st.ResolveApproval(ctx, "ap-1", ApprovalApproved, "device:late"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	a, err := st.GetApproval(ctx, "ap-1")
	if err != nil || a == nil {
		t.Fatalf("get: %v", err)
	}
	if a.State != ApprovalExpired || a.DecisionBy != "timeout" {
		t.Fatalf("expired row overwritten: %+v", a)
	}
}

// An expired row — by state or by a passed deadline — is not an open approval.
func TestPendingApprovalForRunToolIgnoresExpiredRows(t *testing.T) {
	st, err := storeOpenT(t)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	for _, a := range []*Approval{
		{ID: "ap-state", RunID: "run-1", ToolName: "Bash", State: ApprovalExpired, CreatedAt: now, ExpiresAt: now + 60_000},
		{ID: "ap-deadline", RunID: "run-1", ToolName: "Bash", State: ApprovalPending, CreatedAt: now - 120_000, ExpiresAt: now - 60_000},
	} {
		if err := st.UpsertApproval(ctx, a); err != nil {
			t.Fatalf("insert %s: %v", a.ID, err)
		}
	}
	if a, err := st.PendingApprovalForRunTool(ctx, "run-1", "Bash"); err != nil || a != nil {
		t.Fatalf("got %+v (err %v), want no open approval", a, err)
	}
	if err := st.UpsertApproval(ctx, &Approval{
		ID: "ap-live", RunID: "run-1", ToolName: "Bash", State: ApprovalPending,
		CreatedAt: now - 180_000, ExpiresAt: now + 60_000,
	}); err != nil {
		t.Fatalf("insert live: %v", err)
	}
	if a, err := st.PendingApprovalForRunTool(ctx, "run-1", "Bash"); err != nil || a == nil || a.ID != "ap-live" {
		t.Fatalf("got %+v (err %v), want ap-live", a, err)
	}
}
