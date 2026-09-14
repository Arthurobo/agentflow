package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

// The guarantee: a member's work reaches the orchestrator whether or not the
// member sends it. Ask once, then send it for them.

type fakeTerminal struct {
	mu      sync.Mutex
	writes  map[string][]string
	control map[string][]string
	fail    error
}

func (f *fakeTerminal) submit(runID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if f.writes == nil {
		f.writes = map[string][]string{}
	}
	f.writes[runID] = append(f.writes[runID], text)
	return nil
}

func (f *fakeTerminal) prompt(_ context.Context, runID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if f.control == nil {
		f.control = map[string][]string{}
	}
	f.control[runID] = append(f.control[runID], text)
	return nil
}

func (f *fakeTerminal) all(runID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(append([]string(nil), f.writes[runID]...), f.control[runID]...)
}

func reporterFixture(t *testing.T) (*store.Store, *fakeTerminal, *reporter, *store.LoopMember, *store.LoopMember) {
	t.Helper()
	st := reaperStore(t)
	ctx := context.Background()

	l := &store.Loop{Task: "guarantee a finished member's report reaches the orchestrator"}
	if _, err := st.CreateCrew(ctx, l, []store.MemberSpec{
		{Role: store.RoleOrchestrator, RunID: "run_orch"},
		{Role: "INVESTIGATION", RunID: "run_inv", Tool: store.ToolClaude},
	}); err != nil {
		t.Fatalf("loop: %v", err)
	}
	members, err := st.ListLoopMembers(ctx, l.ID)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	byRole := map[string]*store.LoopMember{}
	for _, m := range members {
		byRole[m.Role] = m
	}
	f := &fakeTerminal{}
	return st, f, newReporter(st, f.submit, f.prompt, quietLog()),
		byRole[store.RoleOrchestrator], byRole["INVESTIGATION"]
}

// unreported is one sweep's view of a member that owes a report.
//
// BriefAt is FIXED, not derived from the clock. It is when the brief was
// delivered — a stored value that is identical on every sweep — and deriving
// it from time.Now() would make each sweep look like a new brief, which is
// exactly the double-nudge these tests exist to forbid.
const fixedBriefAt int64 = 1_700_000_000_000

func unreported(inv *store.LoopMember, loopID, tool, report string) store.UnreportedMember {
	return store.UnreportedMember{
		LoopID: loopID, MemberID: inv.ID, Role: inv.Role,
		RunID: inv.RunID, Tool: tool, Report: report,
		BriefAt: fixedBriefAt,
	}
}

func orchInbox(t *testing.T, st *store.Store, orch *store.LoopMember) []*store.LoopMessage {
	t.Helper()
	msgs, err := st.ListLoopMessages(context.Background(), orch.LoopID, 100)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	out := []*store.LoopMessage{}
	for _, m := range msgs {
		if m.RecipientRole == store.RoleOrchestrator && strings.Contains(m.Subject, "auto-forwarded") {
			out = append(out, m)
		}
	}
	return out
}

// First remedy: ask. A member that reports its own findings has said what it
// thinks of them, which a capture can never do.
func TestTheFirstRemedyIsToAskTheMemberToReport(t *testing.T) {
	st, f, r, orch, inv := reporterFixture(t)
	ctx := context.Background()

	r.Handle(ctx, unreported(inv, orch.LoopID, store.ToolClaude, "## PR Count Report"))

	got := f.all("run_inv")
	if len(got) != 1 {
		t.Fatalf("exactly one nudge, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "not reported") || !strings.Contains(got[0], "ORCHESTRATOR") {
		t.Fatalf("the nudge must name the fact and the address: %q", got[0])
	}
	// Nothing is forwarded yet: the member has been given its chance.
	if n := len(orchInbox(t, st, orch)); n != 0 {
		t.Fatalf("nothing may be forwarded before the grace, got %d", n)
	}

	// And it is asked exactly once, however many times the sweep sees it.
	r.Handle(ctx, unreported(inv, orch.LoopID, store.ToolClaude, "## PR Count Report"))
	if got := f.all("run_inv"); len(got) != 1 {
		t.Fatalf("the nudge must fire once per brief, got %d", len(got))
	}
}

// The guarantee itself: after the grace, agentd posts the member's own output.
func TestAfterTheGraceAgentdForwardsTheReportItself(t *testing.T) {
	st, f, r, orch, inv := reporterFixture(t)
	ctx := context.Background()
	u := unreported(inv, orch.LoopID, store.ToolClaude, "## PR Count Report\n4039 merged")

	r.Handle(ctx, u)
	// The member ignored the nudge and the grace ran out.
	r.mu.Lock()
	for _, a := range r.acted {
		a.nudgedAt = time.Now().Add(-reportGrace - time.Second)
	}
	r.mu.Unlock()
	r.Handle(ctx, u)

	inbox := orchInbox(t, st, orch)
	if len(inbox) != 1 {
		t.Fatalf("the orchestrator must receive the work, got %d", len(inbox))
	}
	m := inbox[0]
	// The member's own words, whole.
	if !strings.Contains(m.Body, "## PR Count Report\n4039 merged") {
		t.Fatalf("the body must carry the report verbatim: %q", m.Body)
	}
	// Unmistakably a capture. The orchestrator must never read this as the
	// member speaking: nobody has said what they think of it.
	if !strings.Contains(m.Subject, "auto-forwarded") {
		t.Fatalf("the subject must say it was captured: %q", m.Subject)
	}
	if !strings.Contains(m.Body, "did not send it") {
		t.Fatalf("the body must say it was captured: %q", m.Body)
	}
	// An advisory from the engine, not a post as the member: a capture
	// nobody has looked at must not speak for the member, advance the play
	// or discharge the member's brief.
	if m.SenderRole != store.RoleEngine {
		t.Fatalf("sender = %q, want the engine", m.SenderRole)
	}
	after, err := st.LoopMemberByID(ctx, inv.ID)
	if err != nil || after == nil {
		t.Fatalf("reread: %+v %v", after, err)
	}
	if after.LastPostedAt != 0 {
		t.Fatal("the member said nothing, so it must not read as having posted")
	}
	if len(f.all("run_inv")) != 1 {
		t.Fatalf("the member is asked once, got %v", f.all("run_inv"))
	}
}

// The capture must not move the play on the member's behalf.
func TestAnAutoForwardDoesNotAdvanceThePlay(t *testing.T) {
	st, f, r, orch, inv := reporterFixture(t)
	ctx := context.Background()
	if _, err := st.StartPlay(ctx, orch.LoopID, "recon"); err != nil {
		t.Fatalf("play: %v", err)
	}
	if _, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: "investigate"}); err != nil {
		t.Fatalf("brief: %v", err)
	}
	before, _ := st.GetLoop(ctx, orch.LoopID)
	f.fail = errors.New("tty: gone") // cannot be asked, so it is forwarded at once
	r.Handle(ctx, unreported(inv, orch.LoopID, store.ToolClaude, "## findings"))
	if n := len(orchInbox(t, st, orch)); n != 1 {
		t.Fatalf("the orchestrator must get the advisory, got %d", n)
	}
	after, _ := st.GetLoop(ctx, orch.LoopID)
	if after.StepID != before.StepID || after.Round != before.Round {
		t.Fatalf("the play moved on a capture: %s/%d -> %s/%d", before.StepID, before.Round, after.StepID, after.Round)
	}
}

// A forward refused for good (the loop has ended) ends the attempt instead of
// being retried on every sweep.
func TestAPermanentRefusalEndsTheAttempt(t *testing.T) {
	st, f, r, orch, inv := reporterFixture(t)
	ctx := context.Background()
	if _, err := st.EndLoop(ctx, orch.LoopID, store.LoopKilled, "stop", false); err != nil {
		t.Fatalf("end: %v", err)
	}
	f.fail = errors.New("tty: gone")
	u := unreported(inv, orch.LoopID, store.ToolClaude, "## findings")
	r.Handle(ctx, u)
	r.mu.Lock()
	var done bool
	for _, a := range r.acted {
		done = a.done
	}
	r.mu.Unlock()
	if !done {
		t.Fatal("a forward refused because the loop ended must not be retried")
	}
	if n := len(orchInbox(t, st, orch)); n != 0 {
		t.Fatalf("nothing may reach an ended loop, got %d", n)
	}
}

// Attempts are forgotten by age, however few there are.
func TestReporterForgetsOldAttempts(t *testing.T) {
	_, _, r, orch, inv := reporterFixture(t)
	ctx := context.Background()
	r.Handle(ctx, unreported(inv, orch.LoopID, store.ToolClaude, "## findings"))
	r.mu.Lock()
	for _, a := range r.acted {
		a.at = time.Now().Add(-reporterMemory - time.Minute)
	}
	r.pruneLocked(time.Now())
	n := len(r.acted)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("an attempt older than the memory must be forgotten, %d left", n)
	}
}

// Exactly one forward per brief.
func TestTheForwardHappensOncePerBrief(t *testing.T) {
	st, _, r, orch, inv := reporterFixture(t)
	ctx := context.Background()
	u := unreported(inv, orch.LoopID, store.ToolClaude, "## PR Count Report")

	r.Handle(ctx, u)
	r.mu.Lock()
	for _, a := range r.acted {
		a.nudgedAt = time.Now().Add(-reportGrace - time.Second)
	}
	r.mu.Unlock()
	r.Handle(ctx, u)
	for i := 0; i < 3; i++ {
		r.Handle(ctx, u) // later sweeps
	}
	if n := len(orchInbox(t, st, orch)); n != 1 {
		t.Fatalf("exactly one forward per brief, got %d", n)
	}
}

// Never post nothing. An empty message to the orchestrator reads as an answer.
func TestEmptyOutputIsNeverForwardedOrNudgedFor(t *testing.T) {
	st, f, r, orch, inv := reporterFixture(t)
	ctx := context.Background()

	for _, body := range []string{"", "   ", "\n\t "} {
		u := unreported(inv, orch.LoopID, store.ToolClaude, body)
		r.Handle(ctx, u)
		r.mu.Lock()
		for _, a := range r.acted {
			a.nudgedAt = time.Now().Add(-reportGrace - time.Second)
		}
		r.mu.Unlock()
		r.Handle(ctx, u)
	}
	if n := len(orchInbox(t, st, orch)); n != 0 {
		t.Fatalf("empty output must never be forwarded, got %d", n)
	}
	if got := f.all("run_inv"); len(got) != 0 {
		t.Fatalf("nothing to report means nothing to ask about, got %v", got)
	}
}

// A member whose terminal is gone cannot be asked, so the guarantee falls
// straight through to the forward rather than waiting out a grace nobody is
// going to use.
func TestAMemberThatCannotBeAskedIsForwardedImmediately(t *testing.T) {
	st, f, r, orch, inv := reporterFixture(t)
	ctx := context.Background()
	f.fail = errors.New("tty: no live session for run_inv")

	r.Handle(ctx, unreported(inv, orch.LoopID, store.ToolClaude, "## PR Count Report"))

	inbox := orchInbox(t, st, orch)
	if len(inbox) != 1 {
		t.Fatalf("a dead member's work must still arrive, got %d", len(inbox))
	}
	if !strings.Contains(inbox[0].Body, "## PR Count Report") {
		t.Fatalf("body = %q", inbox[0].Body)
	}
}

// An OpenCode member has no PTY here; it is reached through its control API.
func TestAnOpenCodeMemberIsAskedThroughItsControlAPI(t *testing.T) {
	_, f, r, orch, inv := reporterFixture(t)

	r.Handle(context.Background(), unreported(inv, orch.LoopID, store.ToolOpenCode, "## PR Count Report"))

	f.mu.Lock()
	viaControl := len(f.control["run_inv"])
	viaPTY := len(f.writes["run_inv"])
	f.mu.Unlock()
	if viaControl != 1 {
		t.Fatalf("an opencode member is reached through the control API, got %d", viaControl)
	}
	if viaPTY != 0 {
		t.Fatalf("agentd holds no PTY for an opencode member, got %d writes", viaPTY)
	}
}
