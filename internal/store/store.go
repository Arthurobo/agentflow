// Package store is the SQLite-backed local state for agentflow: managed runs,
// loops and their mail, devices and approvals, and the session index
// (sessions, events, projects, event_blobs and an FTS5 search index). It is
// the only package that owns SQL.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DefaultDataPath returns the default SQLite database path.
func DefaultDataPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "agentflow", "agentflow.db"), nil
}

// Store wraps the SQLite connection. A single writer connection is used by
// ingest (serialized batch commits); readers use the pooled connections.
type Store struct {
	db *sql.DB
	// notify wakes long polls without a database connection in hand; see
	// notify.go. Lazily built so Open needs no change.
	notifyOnce sync.Once

	presence     *presence
	presenceOnce sync.Once
	notify       *mailNotifier
}

// Open opens (creating if needed) the database at path, applies migrations
// and sets WAL/busy-timeout pragmas. dir is created if missing.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	// Create the file with 0600 BEFORE SQLite opens it, so the umask never
	// gets a chance to leave it world-readable. A ? or # in the path (rare
	// but legal on Linux) would otherwise break SQLite's file: DSN;
	// url.PathEscape handles both.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create db: %w", err)
	}
	_ = f.Close()
	// _txlock=immediate takes the write lock at BEGIN, so a read-then-write
	// transaction cannot fail with SQLITE_BUSY_SNAPSHOT when another
	// connection commits between its read and its write.
	//
	// auto_vacuum=INCREMENTAL only takes effect on a database that has no
	// tables yet, which is exactly a new install; retention then hands the
	// pages it frees back to the filesystem. On an existing database it is
	// a no-op.
	dsn := "file:" + url.PathEscape(path) + "?_pragma=auto_vacuum(INCREMENTAL)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite WAL mode supports concurrent readers alongside a single writer.
	// Setting MaxOpenConns(1) serializes readers behind the writer, which
	// makes a health check (a few SELECT COUNT(*) queries) hang whenever
	// the index writer is mid-INSERT. Use a small pool so reads can proceed in parallel
	// without giving up the single-writer discipline at the application
	// level (the Go side still funnels writes through a single goroutine).
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	s := &Store{db: db}
	// Lock down file permissions on the database AND its WAL sidecars. The
	// SQLite driver creates files at the process umask (typically 0644 on
	// Linux), which leaves session history, device token hashes and pending
	// approvals world-readable on a multi-user box. The DB and its sidecars
	// contain the same data; they must all be owner-only.
	if err := lockDownDB(path); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: lock down db: %w", err)
	}
	if err := enableWAL(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable WAL: %w", err)
	}
	if err := s.refuseNewerSchema(context.Background(), path); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

// lockDownDB chmods the database file and its WAL/SHM sidecars to 0600.
// Best-effort: a chmod that fails is logged by the caller as a fatal error
// (the daemon refuses to run with a world-readable DB) but per-sidecar
// failures are tolerated — the main file is the load-bearing one and the
// sidecars get re-created by SQLite on the next write.
func lockDownDB(path string) error {
	targets := []string{path, path + "-wal", path + "-shm"}
	var firstErr error
	for _, p := range targets {
		if _, err := os.Stat(p); err != nil {
			continue // sidecars may not exist yet
		}
		if err := os.Chmod(p, 0o600); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// enableWAL puts the database in WAL mode, tolerating the race between
// processes opening the same file at once. Switching journal mode takes an
// EXCLUSIVE lock and, unlike an ordinary write, does not go through the busy
// handler, so a plain `PRAGMA journal_mode=WAL` returns SQLITE_BUSY outright
// when another opener is mid-migration. The DSN already asks for WAL per
// connection, so this is the verification rather than the switch: a file some
// other opener has already put in WAL needs no switch at all, and the rest is
// a bounded retry.
func enableWAL(db *sql.DB) error {
	var lastErr error
	for i := 0; i < 100; i++ {
		var mode string
		if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err == nil {
			if strings.EqualFold(mode, "wal") {
				return nil
			}
			lastErr = fmt.Errorf("journal_mode is %q, not wal", mode)
		} else {
			lastErr = err
		}
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err == nil && strings.EqualFold(mode, "wal") {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return lastErr
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for cmd-level tooling (migrate/doctor); the store
// package remains the only source of SQL.
func (s *Store) DB() *sql.DB { return s.db }

// ErrSchemaTooNew is returned by Open for a database a newer agentflow has
// already migrated.
var ErrSchemaTooNew = errors.New("store: database schema is newer than this agentflow")

// refuseNewerSchema stops an older binary from running against a database a
// newer one has migrated: after a downgrade, or with a backup restored under
// an old install. Reading columns it does not know is harmless; writing rows
// that break a newer migration's invariants is not, and the newer binary
// would then trust a database it can no longer reason about. It runs before
// migrate, so nothing is written first.
func (s *Store) refuseNewerSchema(ctx context.Context, path string) error {
	var tables int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&tables); err != nil {
		return fmt.Errorf("store: read schema: %w", err)
	}
	if tables == 0 {
		return nil // a new database
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: read schema: %w", err)
	}
	onDisk, onDiskName := 0, ""
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: read schema: %w", err)
		}
		if n := migrationNumber(v); n > onDisk {
			onDisk, onDiskName = n, v
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("store: read schema: %w", err)
	}
	_ = rows.Close()
	known, knownName, err := latestEmbeddedMigration()
	if err != nil {
		return err
	}
	if onDisk > known {
		return fmt.Errorf("%w: %s was migrated to %s, and this binary only knows up to %s. "+
			"Upgrade agentflow, or point AF_DB at a different database", ErrSchemaTooNew, path, onDiskName, knownName)
	}
	return nil
}

// migrationNumber is the leading number of a migration file name, 0 when
// there is none.
func migrationNumber(name string) int {
	end := 0
	for end < len(name) && name[end] >= '0' && name[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(name[:end])
	if err != nil {
		return 0
	}
	return n
}

// latestEmbeddedMigration is the highest-numbered migration in the binary.
func latestEmbeddedMigration() (int, string, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return 0, "", err
	}
	best, name := 0, ""
	for _, e := range entries {
		if n := migrationNumber(e.Name()); n > best {
			best, name = n, e.Name()
		}
	}
	return best, name, nil
}

// tx runs fn inside a transaction, committing on success.
func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// migrate applies embedded migrations in order, tracking applied versions.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	applied := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return err
		}
		applied[v] = true
	}
	_ = rows.Close()

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		version := filepath.Base(e.Name())
		if applied[version] {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		// CLAIM FIRST, then apply. schema_migrations.version is the primary
		// key, so this INSERT is the mutual-exclusion primitive between
		// concurrent appliers sharing one database file — which is the
		// normal case here: two processes opening the same agentflow.db
		// within milliseconds (a CLI command next to the daemon, or a
		// restart that overlaps the old process) both call migrate(). With the DDL running first, the loser of that
		// race re-applied a migration the winner had just committed and
		// died with "duplicate column name: pgid", taking agentd down on
		// the first restart after every new migration.
		if err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
				version, time.Now().UnixMilli()); err != nil {
				return fmt.Errorf("claim migration %s: %w", version, err)
			}
			if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
				return fmt.Errorf("migration %s: %w", version, err)
			}
			return nil
		}); err != nil {
			// Either another process claimed it first (PK conflict) or it
			// committed while we were mid-flight. Re-read the authoritative
			// state: if the version is recorded now, the schema is already
			// where we need it and skipping is correct. Only a genuinely
			// unapplied migration is a startup error.
			var n int
			if qerr := s.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); qerr == nil && n > 0 {
				continue
			}
			return err
		}
	}
	return nil
}

// Counts returns aggregate row counts (used by tests, backfill reports and
// the doctor command).
type Counts struct {
	Sessions   int64 `json:"sessions"`
	Events     int64 `json:"events"`
	Blobs      int64 `json:"blobs"`
	SearchDocs int64 `json:"searchDocs"`
	Projects   int64 `json:"projects"`
	TailerRows int64 `json:"tailerStateRows"`
}

// Counts returns current row counts.
func (s *Store) Counts(ctx context.Context) (*Counts, error) {
	c := &Counts{}
	var err error
	if c.Sessions, err = s.scalarInt(ctx, `SELECT COUNT(*) FROM sessions`); err != nil {
		return nil, err
	}
	if c.Events, err = s.scalarInt(ctx, `SELECT COUNT(*) FROM events`); err != nil {
		return nil, err
	}
	if c.Blobs, err = s.scalarInt(ctx, `SELECT COUNT(*) FROM event_blobs`); err != nil {
		return nil, err
	}
	if c.SearchDocs, err = s.scalarInt(ctx, `SELECT COUNT(*) FROM search_docs`); err != nil {
		return nil, err
	}
	if c.Projects, err = s.scalarInt(ctx, `SELECT COUNT(*) FROM projects`); err != nil {
		return nil, err
	}
	if c.TailerRows, err = s.scalarInt(ctx, `SELECT COUNT(*) FROM tailer_state`); err != nil {
		return nil, err
	}
	return c, nil
}

// FTS5Enabled reports whether the FTS5 extension is compiled in (capability
// probe; modernc.org/sqlite ships it).
func (s *Store) FTS5Enabled(ctx context.Context) (bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT sqlite_compileoption_used('ENABLE_FTS5')`).Scan(&v)
	return err == nil && v == "1", err
}

// SchemaVersion returns the highest applied migration filename.
func (s *Store) SchemaVersion(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) scalarInt(ctx context.Context, q string) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
