package approvals

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

func newTestSvc(t *testing.T, opts ...Option) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
	return svc, st
}

func addApprovalSession(t *testing.T, st *store.Store, runID, sid, project string) {
	t.Helper()
	err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: runID, SessionID: sid, Project: project, CWD: "/tmp/ws",
		ApprovalsEnabled: true,
		State:            "running",
	})
	if err != nil {
		t.Fatalf("upsert session: %v", err)
	}
}

func params(sid, run, tool, input string) Params {
	return Params{SessionID: sid, RunID: run, ToolUseID: "toolu_x", ToolName: tool, ToolInput: input}
}

// TestDenyByDefaultOnTimeout — the : no answer by the deadline = DENY.
// 90s default is replaced with a 150ms wait for the test; no Decide is ever
// called, so the hook must receive denied.
func TestDenyByDefaultOnTimeout(t *testing.T) {
	svc, st := newTestSvc(t, WithWait(150*time.Millisecond))
	addApprovalSession(t, st, "run-1", "sess-1", "webapp")

	d, err := svc.Resolve(context.Background(), params("sess-1", "run-1", "Bash", `{"command":"curl -s https://danger.example"}`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !d.Routed {
		t.Fatal("routed = false, want true")
	}
	if d.Allowed {
		t.Fatal("deny-by-default violated: unansweres approval was allowed")
	}
	if d.Reason != "timeout" {
		t.Fatalf("reason = %q, want timeout", d.Reason)
	}
	// the audit row must be expired
	a, err := st.GetApproval(context.Background(), d.ID)
	if err != nil || a == nil {
		t.Fatalf("approval row missing: %v", err)
	}
	if a.State != store.ApprovalExpired {
		t.Fatalf("state = %s, want expired", a.State)
	}
}

// TestHumanAllowAndDeny — an explicit Decide routes the exact outcome back.
func TestHumanAllowAndDeny(t *testing.T) {
	svc, st := newTestSvc(t, WithWait(5*time.Second))
	addApprovalSession(t, st, "run-2", "sess-2", "webapp")

	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = svc.Decide(context.Background(), waitForID(t, st), true, "device:phone-1")
	}()
	d, err := svc.Resolve(context.Background(), params("sess-2", "run-2", "Bash", `echo ok`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !d.Allowed {
		t.Fatalf("human allow not honored: %+v", d)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = svc.Decide(context.Background(), waitForID(t, st), false, "device:phone-1")
	}()
	d2, err := svc.Resolve(context.Background(), params("sess-2", "run-2", "Edit", `write secret`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d2.Allowed {
		t.Fatalf("human deny not honored: %+v", d2)
	}
}

// waitForID reads the one pending approval's id.
func waitForID(t *testing.T, st *store.Store) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := st.ListApprovals(context.Background(), store.ApprovalPending, "", 10)
		if len(rows) > 0 {
			return rows[0].ID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pending approval appeared")
	return ""
}

// TestPolicyEngine — allow / deny / ask under standing policies; the most
// specific rule wins.
func TestPolicyEngine(t *testing.T) {
	svc, st := newTestSvc(t, WithWait(5*time.Second))
	addApprovalSession(t, st, "run-3", "sess-3", "webapp")
	ctx := context.Background()
	must := func(p *store.ApprovalPolicy) {
		t.Helper()
		if err := st.UpsertPolicy(ctx, p); err != nil {
			t.Fatalf("policy: %v", err)
		}
	}
	// deny Write with a `/etc/` actual pattern, allow curl first, ask the rest
	must(&store.ApprovalPolicy{Project: "webapp", Tool: "Bash", Pattern: `curl`, Action: "allow", Note: "safe curl"})
	must(&store.ApprovalPolicy{Project: "webapp", Tool: "Write", Pattern: `/etc/`, Action: "deny", Note: "never /etc"})

	d, _ := svc.Resolve(ctx, params("sess-3", "run-3", "Bash", `curl -s api.github.com`))
	if !d.Allowed || d.Reason != "policy:1" {
		t.Fatalf("policy allow failed: %+v", d)
	}
	d, _ = svc.Resolve(ctx, params("sess-3", "run-3", "Write", `{"file_path":"/etc/passwd","content":"x"}`))
	if d.Allowed || d.Reason != "policy:2" {
		t.Fatalf("policy deny failed: %+v", d)
	}
	// no policy matches -> routed to human (pending)
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = svc.Decide(ctx, waitForID(t, st), true, "device:phone-1")
	}()
	d, _ = svc.Resolve(ctx, params("sess-3", "run-3", "Bash", `rm -rf /tmp/cache`))
	if !d.Allowed {
		t.Fatalf("default-ask human path failed: %+v", d)
	}
}

// TestNoSpamOnBurst — 20 identical asks for the same (run, tool) open exactly
// ONE notification; every blocked call resolves with the same human decision.
func TestNoSpamOnBurst(t *testing.T) {
	var opens atomic.Int64
	svc, st := newTestSvc(t, WithWait(5*time.Second), WithOnOpen(func(*store.Approval) { opens.Add(1) }))
	addApprovalSession(t, st, "run-4", "sess-4", "webapp")
	ctx := context.Background()

	decided := make(chan string, 1)
	var wg sync.WaitGroup
	results := make([]Decision, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = svc.Resolve(ctx, params("sess-4", "run-4", "Bash", `rm -rf node_modules`))
		}(i)
	}
	// Wait until all 20 asks have registered as waiters on the single collapsed
	// approval before the human decides. A fixed sleep is load-sensitive: a
	// straggler that enters Resolve after the decision (and after recent[key]
	// is stamped) takes the cooldown branch and returns a spurious deny. This
	// polls the real waiter count, so the decision can never race a
	// not-yet-registered ask.
	var ids []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		ids = pendingIDs(t, st)
		if len(ids) == 1 && svc.waiterCount(ids[0]) == 20 {
			break
		}
		if time.Now().After(deadline) {
			got := -1
			if len(ids) == 1 {
				got = svc.waiterCount(ids[0])
			}
			t.Fatalf("burst did not collapse onto one approval with 20 waiters: pending=%d waiters=%d", len(ids), got)
		}
		time.Sleep(time.Millisecond)
	}
	_ = svc.Decide(ctx, ids[0], true, "device:phone-1")
	wg.Wait()
	close(decided)

	if opens.Load() != 1 {
		t.Fatalf("notification spam: onOpen fired %d times, want exactly 1", opens.Load())
	}
	for i := range results {
		if !results[i].Allowed {
			t.Fatalf("burst result[%d] not allowed after human allow: %+v", i, results[i])
		}
	}
}

// TestCooldownAutoDeny — after a decision, the next ask within cooldown is
// auto-denied with no new notification (deny-by-default, zero spam).
func TestCooldownAutoDeny(t *testing.T) {
	var opens atomic.Int64
	svc, st := newTestSvc(t, WithWait(5*time.Second), WithOnOpen(func(*store.Approval) { opens.Add(1) }))
	addApprovalSession(t, st, "run-5", "sess-5", "webapp")
	ctx := context.Background()
	if err := st.UpsertPolicy(ctx, &store.ApprovalPolicy{Tool: "Bash", Action: "ask", CooldownSec: 60}); err != nil {
		t.Fatalf("policy: %v", err)
	}

	// first ask -> open -> allow
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = svc.Decide(ctx, waitForID(t, st), true, "device:phone-1")
	}()
	d, err := svc.Resolve(ctx, params("sess-5", "run-5", "Bash", `echo one`))
	if err != nil || !d.Allowed {
		t.Fatalf("first ask should be allowed: %+v err=%v", d, err)
	}

	// second ask within cooldown -> auto-deny, no push
	d2, err := svc.Resolve(ctx, params("sess-5", "run-5", "Bash", `echo two`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d2.Allowed || d2.Reason != "cooldown" {
		t.Fatalf("cooldown ask should auto-deny: %+v", d2)
	}
	if opens.Load() != 1 {
		t.Fatalf("cooldown caused extra notifications: %d", opens.Load())
	}
}

// TestRoutingGate — a session that is NOT approvals-enabled never routes (the
// "full allowlists => never pings the phone" guarantee).
func TestRoutingGate(t *testing.T) {
	svc, st := newTestSvc(t)
	// session with ApprovalsEnabled=false
	err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: "run-6", SessionID: "sess-6", Project: "webapp", State: "running",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	routed, err := svc.Routed(context.Background(), "sess-6")
	if err != nil || routed {
		t.Fatalf("routed = %v (err %v), want false", routed, err)
	}
	d, err := svc.Resolve(context.Background(), params("sess-6", "run-6", "Bash", `ls`))
	if err != nil || d.Routed {
		t.Fatalf("non-routed session returned routed decision: %+v err=%v", d, err)
	}
}

func pendingIDs(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, _ := st.ListApprovals(context.Background(), store.ApprovalPending, "", 50)
	out := make([]string, 0, len(rows))
	for _, a := range rows {
		out = append(out, a.ID)
	}
	return out
}

// waiterCount reports how many goroutines are currently blocked on the
// broadcast for approval id (test-only; reads the same state under the same
// lock the service uses).
func (s *Service) waiterCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pd, ok := s.dec[id]; ok {
		return pd.waiters
	}
	return 0
}

// TestHumanDenyNotClobberedByTimeout — regression for the AFK-demo bug: a
// human DENY must stay a DENY in the audit trail; the timeout path must never
// overwrite a real decision with "expired".
func TestHumanDenyNotClobberedByTimeout(t *testing.T) {
	svc, st := newTestSvc(t, WithWait(5*time.Second))
	addApprovalSession(t, st, "run-7", "sess-7", "webapp")
	ctx := context.Background()
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = svc.Decide(ctx, waitForID(t, st), false, "device:phone-1")
	}()
	d, err := svc.Resolve(ctx, params("sess-7", "run-7", "Bash", `rm -f secret`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d.Allowed {
		t.Fatal("deny must not allow")
	}
	if d.Reason != "decision" {
		t.Fatalf("reason = %q, want decision (not timeout): %+v", d.Reason, d)
	}
	a, err := st.GetApproval(ctx, d.ID)
	if err != nil || a == nil {
		t.Fatalf("row missing: %v", err)
	}
	if a.State != store.ApprovalDenied {
		t.Fatalf("audit clobbered: state = %s, want denied", a.State)
	}
	if a.DecisionBy != "device:phone-1" {
		t.Fatalf("audit lost the decider: %q", a.DecisionBy)
	}
}

// A hook that disconnects mid-wait cancels the request context. The timeout
// decision must still be written; otherwise the row stays pending forever and
// every later ask for the same (run, tool) collapses onto a dead request.
func TestCancelledHookStillExpiresTheRow(t *testing.T) {
	svc, st := newTestSvc(t, WithWait(5*time.Second))
	addApprovalSession(t, st, "run-c", "sess-c", "webapp")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitForID(t, st)
		cancel()
	}()
	d, err := svc.Resolve(ctx, params("sess-c", "run-c", "Bash", `ls`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d.Allowed {
		t.Fatalf("a cancelled wait must deny: %+v", d)
	}
	a, err := st.GetApproval(context.Background(), d.ID)
	if err != nil || a == nil {
		t.Fatalf("row missing: %v", err)
	}
	if a.State != store.ApprovalExpired {
		t.Fatalf("state = %s, want expired", a.State)
	}
}

// A decision that arrives after the deadline must not rewrite the audit line
// and must tell the caller nothing happened.
func TestLateDecisionDoesNotOverwriteExpiry(t *testing.T) {
	svc, st := newTestSvc(t, WithWait(100*time.Millisecond))
	addApprovalSession(t, st, "run-l", "sess-l", "webapp")
	d, err := svc.Resolve(context.Background(), params("sess-l", "run-l", "Bash", `ls`))
	if err != nil || d.Reason != "timeout" {
		t.Fatalf("resolve: %+v %v", d, err)
	}
	if err := svc.Decide(context.Background(), d.ID, true, "device:late"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("late decide err = %v, want ErrNotPending", err)
	}
	a, _ := st.GetApproval(context.Background(), d.ID)
	if a == nil || a.State != store.ApprovalExpired || a.DecisionBy != "timeout" {
		t.Fatalf("late decision rewrote the row: %+v", a)
	}
}

// A decision made the instant the approval opens (here: from inside onOpen,
// before the hook's goroutine starts waiting) must reach the waiter.
func TestImmediateDecisionIsNotLost(t *testing.T) {
	var svc *Service
	svc, st := newTestSvc(t, WithWait(2*time.Second), WithOnOpen(func(a *store.Approval) {
		if err := svc.Decide(context.Background(), a.ID, true, "device:fast"); err != nil {
			t.Errorf("decide: %v", err)
		}
	}))
	addApprovalSession(t, st, "run-i", "sess-i", "webapp")
	start := time.Now()
	d, err := svc.Resolve(context.Background(), params("sess-i", "run-i", "Bash", `ls`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !d.Allowed || d.Reason != "decision" {
		t.Fatalf("immediate allow lost: %+v", d)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("waited %v for a decision that was already made", time.Since(start))
	}
	a, _ := st.GetApproval(context.Background(), d.ID)
	if a == nil || a.State != store.ApprovalApproved {
		t.Fatalf("audit row = %+v, want approved", a)
	}
}

// Three asks share one approval. One of the collapsed callers gives up; the
// decision must still wake the other two straight away.
func TestOneWaiterLeavingKeepsTheOthersWakeUp(t *testing.T) {
	svc, st := newTestSvc(t, WithWait(3*time.Second))
	addApprovalSession(t, st, "run-w", "sess-w", "webapp")
	bg := context.Background()

	type res struct {
		d   Decision
		err error
	}
	parent := make(chan res, 1)
	go func() {
		d, err := svc.Resolve(bg, params("sess-w", "run-w", "Bash", `one`))
		parent <- res{d, err}
	}()
	id := waitForID(t, st)

	leaverCtx, leave := context.WithCancel(bg)
	leaver := make(chan struct{})
	go func() {
		_, _ = svc.Resolve(leaverCtx, params("sess-w", "run-w", "Bash", `two`))
		close(leaver)
	}()
	stayer := make(chan res, 1)
	go func() {
		d, err := svc.Resolve(bg, params("sess-w", "run-w", "Bash", `three`))
		stayer <- res{d, err}
	}()
	// Both collapsed callers are waiting once their collided rows exist.
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, _ := st.ListApprovals(bg, store.ApprovalCollided, "", 10)
		if len(rows) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("collapsed asks never arrived: %d", len(rows))
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	leave()
	<-leaver

	start := time.Now()
	if err := svc.Decide(bg, id, true, "device:phone"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	for name, ch := range map[string]chan res{"parent": parent, "stayer": stayer} {
		select {
		case r := <-ch:
			if r.err != nil || !r.d.Allowed {
				t.Fatalf("%s: %+v %v", name, r.d, r.err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s never woke after the decision (%v)", name, time.Since(start))
		}
	}
}

// A pending row whose deadline has passed (nothing marked it expired, e.g. a
// restart mid-wait) is not something a new ask may collapse onto.
func TestStalePendingRowIsNotACollapseTarget(t *testing.T) {
	var opens atomic.Int64
	svc, st := newTestSvc(t, WithWait(2*time.Second), WithOnOpen(func(*store.Approval) { opens.Add(1) }))
	addApprovalSession(t, st, "run-s", "sess-s", "webapp")
	bg := context.Background()
	past := time.Now().Add(-time.Minute).UnixMilli()
	if err := st.UpsertApproval(bg, &store.Approval{
		ID: "ap-stale", SessionID: "sess-s", RunID: "run-s", ToolName: "Bash",
		State: store.ApprovalPending, CreatedAt: past - 1000, ExpiresAt: past,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			rows, _ := st.ListApprovals(bg, store.ApprovalPending, "", 10)
			for _, a := range rows {
				if a.ID != "ap-stale" {
					_ = svc.Decide(bg, a.ID, true, "device:phone")
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	d, err := svc.Resolve(bg, params("sess-s", "run-s", "Bash", `ls`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if d.ID == "ap-stale" || !d.Allowed || opens.Load() != 1 {
		t.Fatalf("ask collapsed onto a stale row: %+v opens=%d", d, opens.Load())
	}
}
