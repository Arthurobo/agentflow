// stop_test.go — every loop action that retires members stops their
// processes. Ending a loop used to update rows and nothing else, so the board
// read "ended" while every member kept running with its terminal open.
package loopapi

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

func (f *fakeLauncher) stoppedRuns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.stopped...)
	sort.Strings(out)
	return out
}

func (f *fakeLauncher) launched() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// createSpawned creates an audit loop through the API with a launcher that
// identifies every member, and waits for the whole crew to have launched.
func createSpawned(t *testing.T, h *harness) (string, *fakeLauncher, map[string]string) {
	t.Helper()
	f := &fakeLauncher{sessionFor: map[string]string{
		"ORCHESTRATOR": "sess-orc", "INVESTIGATION": "sess-inv", "REVIEW": "sess-rev",
	}}
	h.srv.SetLauncher(f)
	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "stop test", "task": "audit the export path end to end", "cwd": "/tmp/work", "play": "audit",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	loopID := body["loop"].(map[string]any)["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for f.launched() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the crew never finished launching: %d", f.launched())
		}
		time.Sleep(10 * time.Millisecond)
	}
	runs := map[string]string{}
	roster, _ := h.st.ListLoopMembers(context.Background(), loopID)
	for _, m := range roster {
		if m.RunID != "" {
			runs[m.Role] = m.RunID
		}
	}
	if len(runs) != 3 {
		t.Fatalf("every worker must have a run: %v", runs)
	}
	return loopID, f, runs
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func TestEndStopsEveryLiveMember(t *testing.T) {
	h := newHarness(t)
	loopID, f, runs := createSpawned(t, h)

	resp, body := h.do(h.device, "POST", "/"+loopID+"/end", map[string]any{"reason": "done"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("end: %d %v", resp.StatusCode, body)
	}
	if got, want := f.stoppedRuns(), sortedValues(runs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ending must stop every member's run: stopped %v, want %v", got, want)
	}
	if n, _ := body["stopped"].(float64); int(n) != 3 {
		t.Fatalf("the response must say how many were stopped: %v", body)
	}
	if !strings.Contains(body["agents"].(string), "stopped") {
		t.Fatalf("the response must say the processes were stopped, not that they exit: %v", body["agents"])
	}
}

func TestDismissStopsMembers(t *testing.T) {
	h := newHarness(t)
	loopID, f, runs := createSpawned(t, h)

	resp, body := h.do(h.device, "POST", "/"+loopID+"/dismiss", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dismiss: %d %v", resp.StatusCode, body)
	}
	if got, want := f.stoppedRuns(), sortedValues(runs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dismissing must stop every member's run: stopped %v, want %v", got, want)
	}
}

func TestRetireStopsOnlyThatRole(t *testing.T) {
	h := newHarness(t)
	loopID, f, runs := createSpawned(t, h)

	resp, body := h.do(h.device, "POST", "/"+loopID+"/members/REVIEW/retire", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retire: %d %v", resp.StatusCode, body)
	}
	if got := f.stoppedRuns(); len(got) != 1 || got[0] != runs["REVIEW"] {
		t.Fatalf("retiring REVIEW must stop exactly its run %s, stopped %v", runs["REVIEW"], got)
	}
}

// A launcher-less deployment has no processes; ending, dismissing and
// retiring must still work rather than dereference a nil launcher.
func TestEndWithoutLauncherDoesNotPanic(t *testing.T) {
	h := newHarness(t)
	loopID, _ := h.create("end a loop nobody spawned")
	for _, path := range []string{"/members/REVIEW/retire", "/end", "/dismiss"} {
		if resp, body := h.do(h.device, "POST", "/"+loopID+path, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, body)
		}
	}
}

// recordingBlockLauncher blocks each launch until released and records stops.
type recordingBlockLauncher struct {
	started chan string
	release chan struct{}

	mu       sync.Mutex
	launches []string
	stopped  []string
}

func (b *recordingBlockLauncher) Launch(_ context.Context, runID string, spec LaunchSpec) (LaunchResult, error) {
	b.mu.Lock()
	b.launches = append(b.launches, runID)
	b.mu.Unlock()
	b.started <- spec.Role
	<-b.release
	return LaunchResult{SessionID: "sess-" + runID, Identified: true}, nil
}

func (b *recordingBlockLauncher) Stop(_ context.Context, runID string) error {
	b.mu.Lock()
	b.stopped = append(b.stopped, runID)
	b.mu.Unlock()
	return nil
}

// The crew comes up for minutes. A loop ended while that is happening must
// launch nobody else, and the member that was mid-launch when the end landed
// must be stopped once it exists.
func TestEndDuringSpawnLaunchesNothingMore(t *testing.T) {
	h := newHarness(t)
	b := &recordingBlockLauncher{started: make(chan string, 8), release: make(chan struct{})}
	h.srv.SetLauncher(b)
	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "ended mid spawn", "task": "audit the export path end to end", "cwd": "/tmp/work", "play": "audit",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	loopID := body["loop"].(map[string]any)["id"].(string)
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing started launching")
	}
	b.mu.Lock()
	inFlight := b.launches[0]
	b.mu.Unlock()

	if resp, body := h.do(h.device, "POST", "/"+loopID+"/end", map[string]any{"status": "killed"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("end: %d %v", resp.StatusCode, body)
	}
	close(b.release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		stoppedInFlight := false
		for _, r := range b.stopped {
			if r == inFlight {
				stoppedInFlight = true
			}
		}
		launches := len(b.launches)
		b.mu.Unlock()
		if stoppedInFlight {
			// Give a buggy spawn loop the chance to launch the next member.
			time.Sleep(200 * time.Millisecond)
			b.mu.Lock()
			launches = len(b.launches)
			b.mu.Unlock()
			if launches != 1 {
				t.Fatalf("an ended loop must launch nobody else, launched %d", launches)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the member that was mid-launch was never stopped (launches=%d)", launches)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCreateOverMaxMembersIs400(t *testing.T) {
	h := newHarness(t)
	crew := []map[string]any{{"role": "ORCHESTRATOR", "tool": "claude"}, {"role": "INVESTIGATION", "tool": "claude"}}
	for i := 2; i <= 9; i++ {
		crew = append(crew, map[string]any{"role": "INVESTIGATION#" + string(rune('0'+i)), "tool": "claude"})
	}
	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "too big", "task": "investigate with far too many agents at once", "play": "recon", "crew": crew,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a crew over the member limit is the caller's mistake, want 400, got %d %v", resp.StatusCode, body)
	}
	if loops, _ := h.st.ListLoops(context.Background(), "", 0); len(loops) != 0 {
		t.Fatalf("a refused create must leave nothing behind, found %d loops", len(loops))
	}
}

func TestAddMemberOverCapIs400(t *testing.T) {
	h := newHarness(t)
	loopID, _ := h.create("audit the export path with a full crew")
	roster, _ := h.st.ListLoopMembers(context.Background(), loopID)
	for i := len(roster); i < store.DefaultMaxMembers; i++ {
		resp, body := h.do(h.device, "POST", "/"+loopID+"/members", map[string]any{
			"role": "INVESTIGATION#" + string(rune('2'+i-len(roster))), "tool": "claude",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("add under the limit: %d %v", resp.StatusCode, body)
		}
	}
	resp, body := h.do(h.device, "POST", "/"+loopID+"/members", map[string]any{"role": "EXTRA", "tool": "claude"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an add over the member limit must be a 400, got %d %v", resp.StatusCode, body)
	}
}

// A play that cannot be staffed by the crew is refused before anything is
// written. It used to be discovered after the loop, the crew and the task had
// all been committed, which left an active loop behind the 400.
func TestCreateRefusedByThePlayLeavesNoLoop(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "understaffed", "task": "make the change and review it properly",
		"play": "deep", "crew": []map[string]any{{"role": "ORCHESTRATOR", "tool": "claude"}},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %v", resp.StatusCode, body)
	}
	ctx := context.Background()
	if loops, _ := h.st.ListLoops(ctx, "", 0); len(loops) != 0 {
		t.Fatalf("no loop may be left behind, found %d", len(loops))
	}
}

// Continuing a loop delivers the new task to the successor's orchestrator and
// starts every member that has no process: new roles, and adopted members
// whose processes stopped when the parent ended.
func TestContinueDeliversTheTaskAndSpawnsMembersWithoutAProcess(t *testing.T) {
	h := newHarness(t)
	loopID, f, _ := createSpawned(t, h)
	if resp, body := h.do(h.device, "POST", "/"+loopID+"/end", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("end: %d %v", resp.StatusCode, body)
	}
	before := f.launched()

	resp, next := h.do(h.device, "POST", "/"+loopID+"/continue", map[string]any{
		"task": "round two: now fix what the audit found",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("continue: %d %v", resp.StatusCode, next)
	}
	successorID := next["loop"].(map[string]any)["id"].(string)

	msgs, _ := h.st.ListLoopMessages(context.Background(), successorID, 50)
	delivered := false
	for _, m := range msgs {
		if m.SenderRole == store.RoleEngineer && m.RecipientRole == store.RoleOrchestrator &&
			m.Body == "round two: now fix what the audit found" {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("the successor's orchestrator must be sent the new task: %+v", msgs)
	}

	deadline := time.Now().Add(5 * time.Second)
	for f.launched() < before+3 {
		if time.Now().After(deadline) {
			t.Fatalf("members whose processes were stopped must be started again: launched %d, want %d",
				f.launched(), before+3)
		}
		time.Sleep(10 * time.Millisecond)
	}
	roster, _ := h.st.ListLoopMembers(context.Background(), successorID)
	for _, m := range roster {
		if m.Role == store.RoleEngineer {
			continue
		}
		for _, old := range f.stoppedRuns() {
			if m.RunID == old {
				t.Fatalf("%s still points at its stopped run %s", m.Role, old)
			}
		}
	}
}
