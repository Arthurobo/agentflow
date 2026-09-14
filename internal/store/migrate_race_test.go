// migrate_race_test.go — concurrent migrate() on one database file.
//
// Two processes can open the same agentflow.db within milliseconds (a CLI
// command next to the daemon, or a restart overlapping the old process), and
// Store.Open runs migrate() in both. Before the claim-first fix, the loser of
// that race re-ran DDL the winner had just committed and Open failed with
// "duplicate column name: pgid", taking the daemon down on the first restart
// after a new migration shipped.
package store

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentMigrateIsSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.db")

	const openers = 6
	var wg sync.WaitGroup
	errs := make([]error, openers)
	stores := make([]*Store, openers)

	// Open the same fresh database from several goroutines at once, so they
	// all read an empty schema_migrations and all try to apply every
	// migration.
	start := make(chan struct{})
	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			st, err := Open(path)
			errs[i], stores[i] = err, st
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("opener %d failed: %v (concurrent migrate must be safe — one process applies, the others skip)", i, err)
		}
	}
	t.Cleanup(func() {
		for _, st := range stores {
			if st != nil {
				_ = st.Close()
			}
		}
	})
	if t.Failed() {
		return
	}

	// Exactly one row per migration: no duplicates, nothing skipped.
	st := stores[0]
	var total, distinct int
	if err := st.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version) FROM schema_migrations`).Scan(&total, &distinct); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if total != distinct {
		t.Fatalf("schema_migrations has %d rows but only %d distinct versions", total, distinct)
	}
	if total == 0 {
		t.Fatal("no migrations recorded")
	}

	// And the schema really is migrated — spot-check a column from the
	// migration that exposed the race.
	var hasPgid int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('managed_sessions') WHERE name='pgid'`).Scan(&hasPgid); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if hasPgid != 1 {
		t.Fatal("managed_sessions.pgid missing — migrations were skipped, not applied")
	}
}

// TestMigrateIsIdempotent: re-opening an already-migrated database applies
// nothing and adds no rows.
func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	st1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var before int
	if err := st1.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&before); err != nil {
		t.Fatalf("count: %v", err)
	}
	_ = st1.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer func() { _ = st2.Close() }()
	var after int
	if err := st2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&after); err != nil {
		t.Fatalf("count: %v", err)
	}
	if before != after {
		t.Fatalf("migration count changed on reopen: %d -> %d", before, after)
	}
}
