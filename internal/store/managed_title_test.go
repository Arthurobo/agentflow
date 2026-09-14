package store

import (
	"context"
	"testing"
)

// An OpenCode run named "Namely" showed as "Untitled". The name lived only
// in memory on the spawner's Session; the only thing that ever put it
// somewhere a list could read was appendCustomTitle writing a CLAUDE
// custom-title transcript record, which never runs for another engine. The
// row carries it now, for every engine.
func TestManagedTitleSurvivesStatusPersists(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-1", SessionID: "ses-1", Kind: "tty", State: "running",
		Engine: "opencode", Title: "Namely",
	}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	got, err := s.GetManagedSession(ctx, "run-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Title != "Namely" {
		t.Fatalf("spawn title not stored: %+v", got)
	}

	// Every status persist rebuilds the row from a struct that may not know
	// the title. It is not theirs to erase — the same preserve rule the kill
	// identity has.
	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-1", SessionID: "ses-1", Kind: "tty", State: "running", EventCount: 9,
	}); err != nil {
		t.Fatalf("status persist: %v", err)
	}
	got, _ = s.GetManagedSession(ctx, "run-1")
	if got.Title != "Namely" {
		t.Fatalf("a status persist blanked the title: %+v", got)
	}
	if got.EventCount != 9 {
		t.Fatalf("the fields the writer DOES carry must still apply: %+v", got)
	}

	// An explicit rename still wins.
	if err := s.UpsertManagedSession(ctx, &ManagedSession{
		ID: "run-1", SessionID: "ses-1", Kind: "tty", State: "running", Title: "Renamed",
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, _ = s.GetManagedSession(ctx, "run-1")
	if got.Title != "Renamed" {
		t.Fatalf("an explicit title must win: %+v", got)
	}
}

func TestManagedOrDerivedTitlePrecedence(t *testing.T) {
	if got := ManagedOrDerivedTitle("Namely", "first prompt", "AI Title", ""); got != "Namely" {
		t.Fatalf("managed must win, got %q", got)
	}
	if got := ManagedOrDerivedTitle("   ", "first prompt", "AI Title", ""); got != "AI Title" {
		t.Fatalf("a blank managed title is not a name, got %q", got)
	}
	if got := ManagedOrDerivedTitle("", "first prompt", "", ""); got != "first prompt" {
		t.Fatalf("fallback chain broken, got %q", got)
	}
	if got := ManagedOrDerivedTitle("", "", "", ""); got != "" {
		t.Fatalf("nothing in means nothing out, got %q", got)
	}
}

// The column has to exist on a database that predates it.
func TestManagedSessionTitleMigrationApplies(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()

	rows, err := s.db.QueryContext(context.Background(), `PRAGMA table_info(managed_sessions)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "title" {
			found = true
			if notNull != 1 {
				t.Fatalf("title should be NOT NULL so reads never see a nil")
			}
		}
	}
	if !found {
		t.Fatal("managed_sessions has no title column")
	}
}
