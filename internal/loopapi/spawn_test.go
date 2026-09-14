// spawn_test.go — creating a loop starts its crew. The guarantees are about
// ORDER and about what happens when one member does not come up, because both
// were paid for by real incidents: rows after processes left an unreachable
// live agent, and parallel spawns into one cwd bound several members to one
// transcript.
package loopapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

type launchCall struct {
	RunID   string
	Spec    LaunchSpec
	Exclude []string
}

type fakeLauncher struct {
	mu sync.Mutex
	// sessionFor maps a role to the session id its spawn "discovers"; a role
	// absent from the map reports no identity at all.
	sessionFor map[string]string
	failFor    map[string]error
	calls      []launchCall
	stopped    []string
	// inFlight proves the calls do not overlap.
	inFlight int
	overlap  bool
}

func (f *fakeLauncher) Launch(_ context.Context, runID string, spec LaunchSpec) (LaunchResult, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > 1 {
		f.overlap = true
	}
	f.calls = append(f.calls, launchCall{RunID: runID, Spec: spec, Exclude: append([]string(nil), spec.ExcludeSessionIDs...)})
	err := f.failFor[spec.Role]
	sid, identified := f.sessionFor[spec.Role]
	f.inFlight--
	f.mu.Unlock()
	if err != nil {
		return LaunchResult{}, err
	}
	return LaunchResult{SessionID: sid, Identified: identified}, nil
}

func (f *fakeLauncher) Stop(_ context.Context, runID string) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, runID)
	f.mu.Unlock()
	return nil
}

// titles snapshots the session names under the lock, for the tests that read
// them while spawnCrew is still running detached.
func (f *fakeLauncher) titles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Spec.Title)
	}
	return out
}

func (f *fakeLauncher) roles() []string {
	out := []string{}
	for _, c := range f.calls {
		out = append(out, c.Spec.Role)
	}
	return out
}

func plansFor(roles ...string) []memberPlan {
	out := make([]memberPlan, 0, len(roles))
	for _, r := range roles {
		out = append(out, memberPlan{Role: r, Tool: "claude", RunID: "run_" + strings.ToLower(r), Prompt: "brief for " + r})
	}
	return out
}

// The orchestrator reads the roster and starts addressing people, so it must
// not be running before the members it will address exist.
func TestOrchestratorSpawnsLast(t *testing.T) {
	got := spawnOrder(plansFor("ORCHESTRATOR", "INVESTIGATION", "REVIEW"))
	if len(got) != 3 || got[2].Role != "ORCHESTRATOR" {
		t.Fatalf("orchestrator must be last: %v", []memberPlan(got))
	}
	// a fan-out instance of the orchestrator family goes last too
	got = spawnOrder(plansFor("ORCHESTRATOR#2", "REVIEW"))
	if got[len(got)-1].Role != "ORCHESTRATOR#2" {
		t.Fatalf("orchestrator family must be last: %v", []memberPlan(got))
	}
}

func TestSpawnIsSequentialAndAccumulatesExclusions(t *testing.T) {
	h := newHarness(t)
	f := &fakeLauncher{sessionFor: map[string]string{
		"INVESTIGATION": "sess-inv",
		"REVIEW":        "sess-rev",
		"ORCHESTRATOR":  "sess-orc",
	}}
	h.srv.SetLauncher(f)
	ctx := context.Background()

	// a session id already claimed by something else on this machine
	if err := h.st.UpsertManagedSession(ctx, &store.ManagedSession{
		ID: "other-run", SessionID: "sess-already-claimed", State: "running",
	}); err != nil {
		t.Fatalf("seed managed session: %v", err)
	}

	loop := &store.Loop{Task: "spawn me", CWD: "/tmp/work"}
	if _, err := h.st.CreateCrew(ctx, loop, []store.MemberSpec{
		{Role: "INVESTIGATION", RunID: "run_investigation"},
		{Role: "REVIEW", RunID: "run_review"},
		{Role: "ORCHESTRATOR", RunID: "run_orchestrator"},
	}); err != nil {
		t.Fatalf("crew: %v", err)
	}

	h.srv.spawnCrew(ctx, loop.ID, loop.Task, loop.CWD, plansFor("ORCHESTRATOR", "INVESTIGATION", "REVIEW"))

	if f.overlap {
		t.Error("spawns overlapped; discovery picks the newest transcript in the cwd and they would collide")
	}
	if got := f.roles(); len(got) != 3 || got[2] != "ORCHESTRATOR" {
		t.Fatalf("spawn order = %v", got)
	}
	// every spawn starts from what the machine already owns
	for i, c := range f.calls {
		if !contains(c.Exclude, "sess-already-claimed") {
			t.Errorf("call %d (%s) did not exclude the already-claimed session: %v", i, c.Spec.Role, c.Exclude)
		}
	}
	// and each one adds what the previous spawn found
	second := f.calls[1]
	if !contains(second.Exclude, "sess-inv") {
		t.Errorf("the second spawn must exclude the first's session: %v", second.Exclude)
	}
	third := f.calls[2]
	if !contains(third.Exclude, "sess-inv") || !contains(third.Exclude, "sess-rev") {
		t.Errorf("the third spawn must exclude both earlier sessions: %v", third.Exclude)
	}
	// the link is written back onto the row
	roster, err := h.st.ListLoopMembers(ctx, loop.ID)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	for _, m := range roster {
		if m.Role == store.RoleEngineer {
			// He is an address, not a process: no run, no session, nothing
			// spawned. A run id here would mean we started a process for the
			// human.
			if m.RunID != "" || m.SessionID != "" {
				t.Errorf("the engineer was spawned: run=%q session=%q", m.RunID, m.SessionID)
			}
			continue
		}
		if m.RunID == "" {
			t.Errorf("%s has no run id", m.Role)
		}
		if m.SessionID == "" {
			t.Errorf("%s never had its session id recorded", m.Role)
		}
		if m.LastError != "" {
			t.Errorf("%s recorded an error it did not have: %s", m.Role, m.LastError)
		}
	}
}

// Losing three working agents because a fourth named a binary that is not
// installed is worse than a loop with a gap in it.
func TestOneBadMemberDoesNotSinkTheLoop(t *testing.T) {
	h := newHarness(t)
	f := &fakeLauncher{
		sessionFor: map[string]string{"INVESTIGATION": "sess-inv", "ORCHESTRATOR": "sess-orc"},
		failFor:    map[string]error{"REVIEW": errors.New("exec: \"opencode\": executable file not found in $PATH")},
	}
	h.srv.SetLauncher(f)
	ctx := context.Background()
	loop := &store.Loop{Task: "one is broken", CWD: "/tmp/work"}
	if _, err := h.st.CreateCrew(ctx, loop, []store.MemberSpec{
		{Role: "INVESTIGATION", RunID: "run_investigation"},
		{Role: "REVIEW", RunID: "run_review"},
		{Role: "ORCHESTRATOR", RunID: "run_orchestrator"},
	}); err != nil {
		t.Fatalf("crew: %v", err)
	}

	h.srv.spawnCrew(ctx, loop.ID, loop.Task, loop.CWD, plansFor("ORCHESTRATOR", "INVESTIGATION", "REVIEW"))

	if len(f.calls) != 3 {
		t.Fatalf("every member must still be attempted: %v", f.roles())
	}
	byRole := map[string]*store.LoopMember{}
	roster, _ := h.st.ListLoopMembers(ctx, loop.ID)
	for _, m := range roster {
		byRole[m.Role] = m
	}
	if got := byRole["REVIEW"].LastError; !strings.Contains(got, "not found") {
		t.Errorf("the failure must be on the row, got %q", got)
	}
	if byRole["REVIEW"].Status != store.LoopMemberActive {
		t.Errorf("a failed spawn must leave the role on the roster with its token: %s", byRole["REVIEW"].Status)
	}
	if byRole["ORCHESTRATOR"].SessionID != "sess-orc" || byRole["INVESTIGATION"].SessionID != "sess-inv" {
		t.Error("the members that did start must be unaffected")
	}
}

// A process that never says who it is holds a transcript nobody has claimed,
// and the NEXT member's discovery can bind to it.
func TestAnUnidentifiedMemberIsStoppedNotLeftRunning(t *testing.T) {
	h := newHarness(t)
	f := &fakeLauncher{sessionFor: map[string]string{"ORCHESTRATOR": "sess-orc"}} // REVIEW reports nothing
	h.srv.SetLauncher(f)
	ctx := context.Background()
	loop := &store.Loop{Task: "silent member", CWD: "/tmp/work"}
	if _, err := h.st.CreateCrew(ctx, loop, []store.MemberSpec{
		{Role: "REVIEW", RunID: "run_review"},
		{Role: "ORCHESTRATOR", RunID: "run_orchestrator"},
	}); err != nil {
		t.Fatalf("crew: %v", err)
	}

	h.srv.spawnCrew(ctx, loop.ID, loop.Task, loop.CWD, plansFor("ORCHESTRATOR", "REVIEW"))

	if len(f.stopped) != 1 || f.stopped[0] != "run_review" {
		t.Fatalf("the unidentified member must be stopped, stopped = %v", f.stopped)
	}
	roster, _ := h.st.ListLoopMembers(ctx, loop.ID)
	for _, m := range roster {
		if m.Role == "REVIEW" && !strings.Contains(m.LastError, "never reported a session id") {
			t.Errorf("REVIEW's row must say why it has no process, got %q", m.LastError)
		}
	}
}

// The kernel counts BYTES. A brief measured in runes would pass a check and
// still be truncated by execve.
func TestAnOversizedBriefIsRefusedInsteadOfTruncated(t *testing.T) {
	h := newHarness(t)
	f := &fakeLauncher{sessionFor: map[string]string{"ORCHESTRATOR": "sess-orc"}}
	h.srv.SetLauncher(f)
	ctx := context.Background()
	loop := &store.Loop{Task: "huge brief", CWD: "/tmp/work"}
	if _, err := h.st.CreateCrew(ctx, loop, []store.MemberSpec{
		{Role: "ORCHESTRATOR", RunID: "run_orchestrator"},
	}); err != nil {
		t.Fatalf("crew: %v", err)
	}
	// multi-byte runes: half the rune count of the limit, well over its bytes
	huge := strings.Repeat("é", maxPromptBytes)
	h.srv.spawnCrew(ctx, loop.ID, loop.Task, loop.CWD, []memberPlan{
		{Role: "ORCHESTRATOR", Tool: "claude", RunID: "run_orchestrator", Prompt: huge},
	})

	if len(f.calls) != 0 {
		t.Fatal("an oversized brief must not reach execve at all")
	}
	roster, _ := h.st.ListLoopMembers(ctx, loop.ID)
	if !strings.Contains(roster[0].LastError, "argv budget") {
		t.Errorf("the row must say the brief was too big, got %q", roster[0].LastError)
	}
}

// Nothing spawns when no launcher is wired: the loop is still created and the
// engineer still gets a prompt per member to paste, which is how this worked
// before members were spawned.
func TestNoLauncherStillCreatesTheLoop(t *testing.T) {
	h := newHarness(t)
	h.srv.spawnCrew(context.Background(), "loop_x", "a task", "/tmp", plansFor("REVIEW"))
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// blockingLauncher lets a test observe the ordering between the HTTP response
// and the first spawn.
type blockingLauncher struct {
	started chan string
	release chan struct{}
}

func (b *blockingLauncher) Launch(_ context.Context, runID string, spec LaunchSpec) (LaunchResult, error) {
	b.started <- spec.Role
	<-b.release
	return LaunchResult{SessionID: "sess-" + runID, Identified: true}, nil
}
func (b *blockingLauncher) Stop(context.Context, string) error { return nil }

// Rows before processes, and the response before either finishes. The engine
// this replaced spawned first and inserted afterwards; its own comment records
// the cost: "a live, fully-permissioned claude member that no loop action
// could reach for over two minutes".
func TestCreateReturnsWithEveryRowLinkedBeforeAnythingSpawns(t *testing.T) {
	h := newHarness(t)
	b := &blockingLauncher{started: make(chan string, 8), release: make(chan struct{})}
	h.srv.SetLauncher(b)

	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "spawn on create", "task": "spawn every member as the loop is created",
		"cwd": "/tmp/work", "play": "audit",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	if body["spawning"] != true {
		t.Error("a deployment with a launcher must say it is spawning")
	}
	loop, _ := body["loop"].(map[string]any)
	loopID, _ := loop["id"].(string)
	if loopID == "" {
		t.Fatalf("no loop in the response: %v", body)
	}

	// The response is already back. Every row must exist, and every member
	// that will be spawned must already know its run id — the link is written
	// in the same transaction as the row, before any process exists.
	roster, err := h.st.ListLoopMembers(context.Background(), loopID)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(roster) < 2 {
		t.Fatalf("crew not staffed: %v", roster)
	}
	for _, m := range roster {
		if m.Role == store.RoleEngineer {
			if m.RunID != "" {
				t.Error("the engineer is an address, not a process; it must have no run")
			}
			continue
		}
		if m.RunID == "" {
			t.Errorf("%s has no run id, so nothing could link its process back to it", m.Role)
		}
		if m.SessionID != "" {
			t.Errorf("%s cannot have a session id before it has started", m.Role)
		}
	}

	// and only NOW do the processes come up
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing ever spawned")
	}
	close(b.release)
}

// memberEvents returns the loop events, by content. We read
// the content column directly — the writer puts the rendered
// sentence there, and the detail blob is just the typed
// LoopEvent for the audit log.
func memberEvents(t *testing.T, h *harness) []string {
	t.Helper()
	rows, err := h.st.DB().QueryContext(context.Background(),
		`SELECT content FROM loop_events ORDER BY seq`)
	if err != nil {
		t.Fatalf("loop_events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// Success was silent and failure was live, which is backwards: a member that
// FAILED updated the board instantly while four members that started perfectly
// changed nothing on screen, so the happy path was the one that looked broken.
func TestAStartedMemberIsAnEventTooNotJustAFailedOne(t *testing.T) {
	h := newHarness(t)
	f := &fakeLauncher{
		sessionFor: map[string]string{"INVESTIGATION": "sess-inv", "ORCHESTRATOR": "sess-orc"},
		failFor:    map[string]error{"REVIEW": errors.New("binary not found")},
	}
	h.srv.SetLauncher(f)
	ctx := context.Background()
	loop := &store.Loop{Task: "watch the board", CWD: "/tmp/work"}
	if _, err := h.st.CreateCrew(ctx, loop, []store.MemberSpec{
		{Role: "INVESTIGATION", RunID: "run_investigation"},
		{Role: "REVIEW", RunID: "run_review"},
		{Role: "ORCHESTRATOR", RunID: "run_orchestrator"},
	}); err != nil {
		t.Fatalf("crew: %v", err)
	}

	h.srv.spawnCrew(ctx, loop.ID, loop.Task, loop.CWD, plansFor("ORCHESTRATOR", "INVESTIGATION", "REVIEW"))

	events := strings.Join(memberEvents(t, h), "\n")
	for _, want := range []string{"INVESTIGATION started", "ORCHESTRATOR started"} {
		if !strings.Contains(events, want) {
			t.Errorf("no event for %q; the board learns nothing when a member starts:\n%s", want, events)
		}
	}
	if !strings.Contains(events, "REVIEW did not start") {
		t.Errorf("the failure event went missing:\n%s", events)
	}
}

// An event about a member that is not on this loop would be a lie.
func TestAStartWithNoMatchingRowEmitsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	loop := &store.Loop{Task: "no such run", CWD: "/tmp"}
	if _, err := h.st.CreateCrew(ctx, loop, []store.MemberSpec{{Role: "REVIEW", RunID: "run_review"}}); err != nil {
		t.Fatalf("crew: %v", err)
	}
	before := len(memberEvents(t, h))
	if err := h.st.MarkLoopMemberStarted(ctx, loop.ID, "REVIEW", "run_that_is_not_here", "sess-x"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if after := len(memberEvents(t, h)); after != before {
		t.Errorf("an unmatched run emitted %d extra events", after-before)
	}
}

// Every member of every loop used to be called by its role alone, so three
// loops made nine rows in the sessions list reading ORCHESTRATOR,
// INVESTIGATION, REVIEW three times over with nothing to tell them apart.
func TestAMemberSessionIsNamedAfterItsLoop(t *testing.T) {
	if got := memberTitle("Research Backend", "ORCHESTRATOR"); got != "Research Backend -> ORCHESTRATOR" {
		t.Errorf("got %q", got)
	}
	// A fan-out instance is a DIFFERENT agent and the list has to say so.
	if got := memberTitle("Research Backend", "INVESTIGATION#2"); got != "Research Backend -> INVESTIGATION#2" {
		t.Errorf("the fan-out suffix must survive, got %q", got)
	}
	// A title long enough to wrap pushes the role off the end, which is the
	// thing the name exists to show.
	long := memberTitle(
		"Investigate why the guest export intermittently drops rows on large events",
		"REVIEW")
	if !strings.HasSuffix(long, " -> REVIEW") {
		t.Errorf("the role must survive truncation, got %q", long)
	}
	if len(long) > maxTitleTask+len(" -> REVIEW")+4 {
		t.Errorf("title not truncated: %q", long)
	}
	if strings.Contains(long, "intermittently drops rows") {
		t.Errorf("the tail should have been cut, got %q", long)
	}
	// A loop with no task falls back to the bare role rather than a stray arrow.
	if got := memberTitle("", "REVIEW"); got != "REVIEW" {
		t.Errorf("got %q", got)
	}
	if got := memberTitle("   ", "REVIEW"); got != "REVIEW" {
		t.Errorf("whitespace is not a task, got %q", got)
	}
	// Newlines in a task would break the one-line list row.
	if got := memberTitle("two\nlines", "REVIEW"); got != "two lines -> REVIEW" {
		t.Errorf("got %q", got)
	}
}

// And the name the launcher is actually handed carries it.
func TestTheLauncherReceivesTheLoopNamedTitle(t *testing.T) {
	h := newHarness(t)
	f := &fakeLauncher{sessionFor: map[string]string{"REVIEW": "sess-rev"}}
	h.srv.SetLauncher(f)
	ctx := context.Background()
	// Named from the TITLE, not the instruction. The instruction is a
	// paragraph; truncating it to 42 characters produced session names that
	// were the first six words of a sentence.
	loop := &store.Loop{
		Title: "Research Backend",
		Task:  "Work out which backend generation still serves production and say so",
		CWD:   "/tmp/work",
	}
	if _, err := h.st.CreateCrew(ctx, loop, []store.MemberSpec{{Role: "REVIEW", RunID: "run_review"}}); err != nil {
		t.Fatalf("crew: %v", err)
	}
	h.srv.spawnCrew(ctx, loop.ID, loop.Title, loop.CWD, plansFor("REVIEW"))
	if len(f.calls) != 1 {
		t.Fatalf("want one spawn, got %d", len(f.calls))
	}
	if got := f.calls[0].Spec.Title; got != "Research Backend -> REVIEW" {
		t.Errorf("the session is named %q; the sessions list cannot tell two loops apart", got)
	}
}

// The whole point of two fields: the session name comes from the name, and
// the instruction never appears in it. With one field a member was called
// "Find why the guest export goes fla… -> REVIEW".
func TestTheSessionNameComesFromTheTitleAndNeverFromTheInstruction(t *testing.T) {
	h := newHarness(t)
	f := &fakeLauncher{sessionFor: map[string]string{"REVIEW": "sess-rev"}}
	h.srv.SetLauncher(f)

	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "Flaky export",
		"task":  "Find why the guest export goes flaky above 500 rows and fix it",
		"cwd":   "/tmp/work", "play": "audit",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	// spawnCrew runs detached, so poll a locked snapshot rather than sleep.
	var titles []string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if titles = f.titles(); len(titles) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(titles) == 0 {
		t.Fatal("nothing spawned")
	}
	for _, title := range titles {
		if !strings.HasPrefix(title, "Flaky export -> ") {
			t.Fatalf("member named %q; it must be named after the loop", title)
		}
		if strings.Contains(title, "guest export goes") {
			t.Fatalf("the instruction leaked into the session name: %q", title)
		}
	}
}
