package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/arthurobo/agentflow/internal/store"
)

type stopRecorder struct {
	mu   sync.Mutex
	runs []string
}

func (r *stopRecorder) stop(runIDs []string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, runIDs...)
	return len(runIDs)
}

func (r *stopRecorder) stopped() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.runs...)
	sort.Strings(out)
	return out
}

func sweeperLoop(t *testing.T, st *store.Store) (*store.Loop, map[string]*store.LoopMember) {
	t.Helper()
	ctx := context.Background()
	l := &store.Loop{Task: "a loop the sweeper watches over"}
	if _, err := st.StartLoop(ctx, l, []store.MemberSpec{
		{Role: store.RoleOrchestrator, RunID: "run_orch"},
		{Role: "INVESTIGATION", RunID: "run_inv"},
	}, "recon", "find out what is going on"); err != nil {
		t.Fatalf("loop: %v", err)
	}
	for _, run := range []string{"run_orch", "run_inv"} {
		seedRun(t, st, run, "running", "gen")
	}
	roster, err := st.ListLoopMembers(ctx, l.ID)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	byRole := map[string]*store.LoopMember{}
	for _, m := range roster {
		byRole[m.Role] = m
	}
	return l, byRole
}

// A loop over one of its limits is ended as capped, with a loop event, and
// every member's process is stopped through the same path an end takes.
func TestBudgetTripCapsLoopAndStopsMembers(t *testing.T) {
	st := reaperStore(t)
	ctx := context.Background()
	l, _ := sweeperLoop(t, st)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE loops SET max_messages_per_round = 1 WHERE id = ?`, l.ID); err != nil {
		t.Fatalf("limit: %v", err)
	}
	rec := &stopRecorder{}
	sweepOnce(ctx, st, nil, nil, rec.stop, quietLog())

	got, err := st.GetLoop(ctx, l.ID)
	if err != nil || got.Status != store.LoopCapped || !got.Ended() {
		t.Fatalf("the loop must be capped: %+v %v", got, err)
	}
	if !strings.Contains(got.EndReason, "messages") {
		t.Fatalf("the end reason must say which limit: %q", got.EndReason)
	}
	if s := rec.stopped(); strings.Join(s, ",") != "run_inv,run_orch" {
		t.Fatalf("every member must be stopped, got %v", s)
	}
	var events int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM loop_events WHERE loop_id = ? AND kind = ?`, l.ID, store.EvLoopCapped).Scan(&events); err != nil || events != 1 {
		t.Fatalf("capping must be one loop event, got %d %v", events, err)
	}
	// A second sweep finds nothing more to do.
	sweepOnce(ctx, st, nil, nil, rec.stop, quietLog())
	if s := rec.stopped(); len(s) != 2 {
		t.Fatalf("an ended loop must not be stopped again: %v", s)
	}
}

// A finished play ends its loop, once the orchestrator has reported.
func TestPlayDoneEndsLoop(t *testing.T) {
	st := reaperStore(t)
	ctx := context.Background()
	l, roles := sweeperLoop(t, st)
	orch, inv := roles[store.RoleOrchestrator], roles["INVESTIGATION"]
	engineer, _ := st.LoopMemberByRole(ctx, l.ID, store.RoleEngineer)
	for _, p := range []struct {
		from, to *store.LoopMember
		body     string
	}{{orch, inv, "investigate"}, {inv, orch, "conclude"}, {orch, engineer, "here is what we found"}} {
		if _, err := st.PostMail(ctx, p.from, p.to, store.MailPost{Body: p.body}); err != nil {
			t.Fatalf("%s: %v", p.body, err)
		}
	}
	if cur, _ := st.GetLoop(ctx, l.ID); cur.PlayStatus != store.PlayDone {
		t.Fatalf("the play must be done: %+v", cur)
	}
	rec := &stopRecorder{}
	sweepOnce(ctx, st, nil, nil, rec.stop, quietLog())
	got, _ := st.GetLoop(ctx, l.ID)
	if !got.Ended() || got.Status != store.LoopComplete {
		t.Fatalf("a finished play must end its loop as complete: %+v", got)
	}
	if len(rec.stopped()) != 2 {
		t.Fatalf("the members must be stopped with it: %v", rec.stopped())
	}
}

func TestRetentionDaysComesFromTheEnvironment(t *testing.T) {
	for env, want := range map[string]int{"": store.DefaultRetentionDays, "30": 30, "0": 0, "-4": store.DefaultRetentionDays, "soon": store.DefaultRetentionDays} {
		t.Setenv("AF_RETENTION_DAYS", env)
		if got := retentionDays(); got != want {
			t.Errorf("AF_RETENTION_DAYS=%q: got %d, want %d", env, got, want)
		}
	}
}
