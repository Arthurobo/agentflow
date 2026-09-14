package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func seedIndexedSession(t *testing.T, s *Store, sid string, ts int64, blob string) {
	t.Helper()
	ev := Incoming{SessionID: sid, UUID: sid + "-u1", Event: "assistant_message", Content: "answer for " + sid, TS: ts, Seq: 1}
	b := &Batch{Events: []Incoming{ev}, Session: &SessionMeta{SessionID: sid, CWD: "/w", Project: "w", FilePath: "/w/" + sid}}
	if blob != "" {
		b.Events = append(b.Events, Incoming{SessionID: sid, UUID: sid + "-u2", Event: "tool_result",
			Content: "preview", ContentHash: blob, Oversized: true, TS: ts, Seq: 2})
		b.Blobs = []Blob{{Hash: blob, Content: "the whole oversized output of " + sid}}
	}
	if err := s.InsertBatch(context.Background(), b); err != nil {
		t.Fatalf("seed %s: %v", sid, err)
	}
}

func countWhere(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestRetentionPrunesOldSessions(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Now()
	old := now.AddDate(0, 0, -120).UnixMilli()
	seedIndexedSession(t, s, "old", old, "hash-old")
	seedIndexedSession(t, s, "old-but-live", old, "")
	seedIndexedSession(t, s, "recent", now.AddDate(0, 0, -1).UnixMilli(), "hash-recent")
	if err := s.UpsertManagedSession(ctx, &ManagedSession{ID: "run-live", SessionID: "old-but-live", Kind: "tty", State: "running"}); err != nil {
		t.Fatal(err)
	}

	rep, err := s.PruneSessionsNotUpdatedSince(ctx, now.AddDate(0, 0, -DefaultRetentionDays))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rep.Sessions != 1 || rep.Events != 2 || rep.Blobs != 1 {
		t.Fatalf("want one session, two events and one blob pruned: %+v", rep)
	}
	for table, q := range map[string]string{
		"events":      `SELECT COUNT(*) FROM events WHERE session_id = ?`,
		"sessions":    `SELECT COUNT(*) FROM sessions WHERE session_id = ?`,
		"search_docs": `SELECT COUNT(*) FROM search_docs WHERE session_id = ?`,
	} {
		if n := countWhere(t, s, q, "old"); n != 0 {
			t.Errorf("%s still holds %d rows of the old session", table, n)
		}
		if n := countWhere(t, s, q, "recent"); n == 0 {
			t.Errorf("%s lost the recent session", table)
		}
		if n := countWhere(t, s, q, "old-but-live"); n == 0 {
			t.Errorf("%s lost a session a live run holds", table)
		}
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM event_blobs WHERE content_hash = ?`, "hash-old"); n != 0 {
		t.Error("the old session's blob must go")
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM event_blobs WHERE content_hash = ?`, "hash-recent"); n != 1 {
		t.Error("a blob a remaining event references must stay")
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM search_idx WHERE search_idx MATCH ?`, "old"); n != 1 {
		t.Errorf("the full-text index must forget the pruned session's text, %d matches left", n)
	}
}

// An older binary must not run against a database a newer one migrated, and
// must refuse before it writes anything.
func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES ('9999_from_the_future.sql', 1)`); err != nil {
		t.Fatal(err)
	}
	before := countWhere(t, s, `SELECT COUNT(*) FROM schema_migrations`)
	_ = s.Close()

	if _, err := Open(path); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("a database from a newer binary must be refused, got %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var after int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("the refused open must not have migrated anything: %d -> %d", before, after)
	}
	if migrationNumber("0033_loop_budget_fixes.sql") != 33 || migrationNumber("notes.txt") != 0 {
		t.Fatal("migration numbers are the leading digits of the file name")
	}
}

// Read-then-write transactions from many goroutines must not fail with
// SQLITE_BUSY_SNAPSHOT: with a deferred BEGIN, a transaction that read before
// another connection committed cannot then upgrade to a write, and the busy
// timeout does not help.
func TestTxlockImmediateAvoidsBusySnapshot(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE counter (n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO counter (n) VALUES (0)`); err != nil {
		t.Fatal(err)
	}
	const workers, rounds = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				err := s.tx(ctx, func(tx *sql.Tx) error {
					var n int
					if err := tx.QueryRowContext(ctx, `SELECT n FROM counter`).Scan(&n); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, `UPDATE counter SET n = ?`, n+1)
					return err
				})
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a read-then-write transaction failed: %v", err)
	}
	if n := countWhere(t, s, `SELECT n FROM counter`); n != workers*rounds {
		t.Fatalf("every increment must land exactly once: got %d, want %d", n, workers*rounds)
	}
}

// NOTE: the historical data-migration that cleared legacy blank-id session
// rows (0034_drop_blank_sessions) was folded away when the migrations were
// consolidated into a single 0001_init at open-source inception. A fresh
// schema never contains those rows, so there is nothing left to test here.
