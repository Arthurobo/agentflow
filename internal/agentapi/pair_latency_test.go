package agentapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// Pairing a phone took 1231ms inside agentd. The handler's only jobs are a
// SELECT, one row write, and a notification to the relay — so a second of it
// was something waiting, and these tests bound each thing it can wait on.
func pairFixture(t *testing.T) (*agentapi.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	st, err := store.Open(filepath.Join(dir, "pair.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ing := ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"})
	sp := spawner.New(st, ing, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp.SetCorpusRoot(dir)
	srv := agentapi.New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "test-machine")

	// A one-shot pairing token, exactly as `agentd pair` mints one.
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "pairing-slot", Name: "pair", MachineID: "test-machine",
		Kind: "pairing", Status: "pending", TokenHash: store.HashToken("pair-token"),
	}, time.Now().Add(15*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("seed pairing token: %v", err)
	}
	return srv, st
}

func doPair(t *testing.T, srv *agentapi.Server) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"token": "pair-token", "name": "phone"})
	req := httptest.NewRequest("POST", "/api/v1/agentd/pair/complete", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	start := time.Now()
	srv.Handler().ServeHTTP(rr, req)
	return rr, time.Since(start)
}

// The acceptance bound, with nothing else going on.
func TestPairCompleteIsFast(t *testing.T) {
	srv, _ := pairFixture(t)

	rr, elapsed := doPair(t, srv)
	if rr.Code != http.StatusCreated {
		t.Fatalf("pair returned %d %s", rr.Code, rr.Body.String())
	}
	t.Logf("pair/complete took %v", elapsed)
	if elapsed > 50*time.Millisecond {
		t.Fatalf("pair took %v, want under 50ms", elapsed)
	}

	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["deviceToken"] == "" || out["deviceToken"] == nil {
		t.Fatalf("pair must return a device token: %v", out)
	}
}

// The same bound while another goroutine holds a write transaction — the
// case the retry ladder existed for, and the one that was blamed for the
// 1231ms.
func TestPairCompleteIsFastWhileAWriteTransactionIsHeld(t *testing.T) {
	srv, st := pairFixture(t)

	release := make(chan struct{})
	holding := make(chan struct{})
	go func() {
		ctx := context.Background()
		tx, err := st.DB().BeginTx(ctx, nil)
		if err != nil {
			close(holding)
			return
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO devices (id, name, machine_id, kind, token_hash, expires_at, status, created_at, last_seen, revoked_at)
			 VALUES ('hog', 'hog', 'm', 'agentd', 'hoghash', 0, 'active', 1, 1, 0)`); err != nil {
			close(holding)
			_ = tx.Rollback()
			return
		}
		close(holding)
		<-release
		_ = tx.Commit()
	}()
	<-holding
	// Hold it for a beat, then let go — SQLite's busy handler should absorb
	// this without the handler's own backoff ladder ever running.
	go func() {
		time.Sleep(150 * time.Millisecond)
		close(release)
	}()

	rr, elapsed := doPair(t, srv)
	if rr.Code != http.StatusCreated {
		t.Fatalf("pair returned %d %s under contention", rr.Code, rr.Body.String())
	}
	t.Logf("pair/complete under a held write transaction took %v", elapsed)
	// It may legitimately wait out the 150ms lock; what it must not do is
	// spend most of a second on a backoff ladder afterwards.
	if elapsed > 400*time.Millisecond {
		t.Fatalf("pair took %v under a 150ms lock; the backoff is dominating", elapsed)
	}
}
