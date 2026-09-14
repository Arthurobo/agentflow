package store

import (
	"context"
	"testing"
)

// One session id can have several runs, one per resume. The live one must
// win, then the most recently updated, whatever order the rows were written.
func TestGetManagedSessionBySessionIDPrefersLiveThenRecent(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	rows := []*ManagedSession{
		{ID: "run-live-old", SessionID: "sess-1", Kind: "tty", State: "running", UpdatedAt: 100},
		{ID: "run-dead-new", SessionID: "sess-1", Kind: "tty", State: "stopped", UpdatedAt: 900},
		{ID: "run-live-new", SessionID: "sess-1", Kind: "tty", State: "starting", UpdatedAt: 500},
	}
	for _, r := range rows {
		if err := s.UpsertManagedSession(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetManagedSessionBySessionID(ctx, "sess-1")
	if err != nil || got == nil || got.ID != "run-live-new" {
		t.Fatalf("want the most recent live run, got %+v %v", got, err)
	}
	for _, id := range []string{"run-live-old", "run-live-new"} {
		if err := s.UpsertManagedSession(ctx, &ManagedSession{ID: id, SessionID: "sess-1", Kind: "tty", State: "finished", UpdatedAt: 50}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.GetManagedSessionBySessionID(ctx, "sess-1"); got == nil || got.ID != "run-dead-new" {
		t.Fatalf("with nothing live, the most recently updated wins, got %+v", got)
	}
}

// A writer that does not know the session id must not erase the one a run
// already has.
func TestUpsertManagedSessionKeepsAKnownSessionID(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if err := s.UpsertManagedSession(ctx, &ManagedSession{ID: "run-oc", SessionID: "ses_bound", Kind: "tty", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertManagedSession(ctx, &ManagedSession{ID: "run-oc", Kind: "tty", State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetManagedSession(ctx, "run-oc")
	if got.SessionID != "ses_bound" || got.State != "stopped" {
		t.Fatalf("the exit persist must keep the session id: %+v", got)
	}
	if err := s.UpsertManagedSession(ctx, &ManagedSession{ID: "run-oc", SessionID: "ses_other", Kind: "tty", State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetManagedSession(ctx, "run-oc"); got.SessionID != "ses_other" {
		t.Fatalf("an explicit id still wins: %+v", got)
	}
}
