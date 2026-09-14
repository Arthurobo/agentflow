// migrate_rename_test.go — the schema 0020 leaves behind.
//
// The rest of the suite proves the NEW names exist, because every query in
// mail.go and mailbox.go would fail on a missing table. Nothing proved the
// other half: that the old engine's four tables are gone. Dropping
// loop_sessions and loop_events is invisible to every other test, so without
// this the drops could be deleted and the suite would stay green while a
// deleted feature kept its tables forever.
package store

import (
	"path/filepath"
	"testing"
)

func TestMigrationLeavesOnlyTheRenamedLoopTables(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "rename.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	names := map[string]bool{}
	rows, err := s.DB().Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for _, want := range []string{"loops", "loop_members", "loop_messages", "loop_notes", "loop_refusals", "loop_events"} {
		if !names[want] {
			t.Errorf("table %q is missing after migration", want)
		}
	}
	// The old TTY-supervision engine's session table, deleted with its
	// package, and the outbox, whose only reader was the hosted relay.
	// (loop_events is a new table with an old name; see 0032.)
	for _, gone := range []string{"loop_sessions", "mail_messages", "mail_loops", "mail_members", "mail_refusals", "mail_notes", "outbox"} {
		if names[gone] {
			t.Errorf("table %q belongs to the deleted engine and must be dropped", gone)
		}
	}
	// The cutover prefix must survive nowhere.
	for n := range names {
		if len(n) >= 5 && n[:5] == "mail_" {
			t.Errorf("table %q still carries the cutover prefix", n)
		}
	}
}
