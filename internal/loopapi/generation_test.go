package loopapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/arthurobo/agentflow/internal/mailapi"
	"github.com/arthurobo/agentflow/internal/store"
)

// A loop born in this agentd life must say so. Without the stamp the NEXT
// boot cannot tell a loop somebody is still running from one whose daemon
// died, so it either reaps the living or leaves the dead reading "active" —
// which is what it did.
func newStampedHarness(t *testing.T, gen string) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "loops.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	device := "device-token-for-the-engineer"
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "dev1", Name: "laptop", Kind: "device", TokenHash: store.HashToken(device),
	}, 0); err != nil {
		t.Fatalf("device: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	agents := mailapi.New(st, log, mailapi.Config{})
	srv := New(st, agents, log, Config{
		BaseURL: "http://127.0.0.1:4344", Generation: gen,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{t: t, st: st, srv: srv, agents: agents, http: ts, device: device}
}

func TestACreatedLoopCarriesThisBootsGeneration(t *testing.T) {
	h := newStampedHarness(t, "gen-this-boot")
	loopID, _ := h.create("stamp this loop with the running daemon")

	got, err := h.st.GetLoop(context.Background(), loopID)
	if err != nil || got == nil {
		t.Fatalf("get: %+v %v", got, err)
	}
	if got.Generation != "gen-this-boot" {
		t.Fatalf("generation = %q, want the running daemon's", got.Generation)
	}

	// And it is NOT an orphan to the daemon that made it, which is the
	// property the whole stamp exists for.
	orphans, err := h.st.ListActiveLoopsNotInGeneration(context.Background(), "gen-this-boot")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, l := range orphans {
		if l.ID == loopID {
			t.Fatal("a loop this boot just created must never look orphaned to it")
		}
	}
}

// A successor is started by whoever is running NOW. Inheriting the parent's
// generation would have it reaped on the next restart for no reason.
func TestASuccessorCarriesTheCurrentGenerationNotItsParents(t *testing.T) {
	h := newStampedHarness(t, "gen-this-boot")
	loopID, _ := h.create("round one against the export path")

	// Age the parent into a previous life, the way a restart would.
	if _, err := h.st.DB().ExecContext(context.Background(),
		`UPDATE loops SET generation = 'gen-previous' WHERE id = ?`, loopID); err != nil {
		t.Fatalf("age the parent: %v", err)
	}

	resp, next := h.do(h.device, "POST", "/"+loopID+"/continue", map[string]any{
		"task": "round two, with the reviewer kept on", "carryNotes": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("continue: %d %v", resp.StatusCode, next)
	}
	successorID := next["loop"].(map[string]any)["id"].(string)

	got, err := h.st.GetLoop(context.Background(), successorID)
	if err != nil || got == nil {
		t.Fatalf("get: %+v %v", got, err)
	}
	if got.Generation != "gen-this-boot" {
		t.Fatalf("successor generation = %q, want this boot's, not the parent's", got.Generation)
	}
}

// A deployment that does not stamp still works; its loops are simply reaped
// on the following restart, exactly like the ones that predate the column.
func TestAnUnstampedDeploymentStillCreatesLoops(t *testing.T) {
	h := newStampedHarness(t, "")
	loopID, _ := h.create("no stamp on this deployment at all")

	got, err := h.st.GetLoop(context.Background(), loopID)
	if err != nil || got == nil {
		t.Fatalf("get: %+v %v", got, err)
	}
	if got.Generation != "" {
		t.Fatalf("generation = %q, want empty", got.Generation)
	}
	orphans, err := h.st.ListActiveLoopsNotInGeneration(context.Background(), "gen-next-boot")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(orphans) != 1 || orphans[0].ID != loopID {
		t.Fatalf("an unstamped loop is an orphan to the next boot, got %+v", orphans)
	}
}
