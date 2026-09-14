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

// The courier is what replaced the blocking foreground poll. These tests pin
// the properties that make replacing it safe: the body reaches the terminal,
// it reaches it once, it reaches it in order, and a write that fails puts the
// mail back rather than swallowing it.

// fakePTY records what was submitted, and can be told to fail the way a run
// with no live PTY does.
type fakePTY struct {
	mu     sync.Mutex
	writes map[string][]string
	fail   error
}

func (f *fakePTY) submit(runID, text string) error {
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

func (f *fakePTY) all(runID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes[runID]...)
}

// courierFixture builds a loop whose INVESTIGATION member has a run on this
// host and whose REVIEW member runs somewhere else, which is the distinction
// the courier turns on.
func courierFixture(t *testing.T) (*store.Store, *fakePTY, *courier, *store.LoopMember, *store.LoopMember, *store.LoopMember) {
	t.Helper()
	st := reaperStore(t)
	ctx := context.Background()

	l := &store.Loop{Task: "deliver mail into a terminal instead of polling for it"}
	if _, err := st.CreateCrew(ctx, l, []store.MemberSpec{
		{Role: store.RoleOrchestrator, RunID: "run_orch"},
		{Role: "INVESTIGATION", RunID: "run_inv"},
		{Role: "REVIEW"}, // no run: it joined from somewhere else
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
	orch, inv, external := byRole[store.RoleOrchestrator], byRole["INVESTIGATION"], byRole["REVIEW"]
	if orch == nil || inv == nil || external == nil {
		t.Fatalf("the roster is missing a role: %v", byRole)
	}
	if inv.RunID != "run_inv" {
		t.Fatalf("the member must be born knowing its run, got %q", inv.RunID)
	}
	f := &fakePTY{}
	return st, f, newCourier(st, f.submit, nil, nil, quietLog()), orch, inv, external
}

func TestTheCourierPutsTheBodyIntoTheMembersTerminal(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()

	if _, err := st.PostMail(ctx, orch, inv, store.MailPost{
		Subject: "brief", Body: "look at the second table, the one with the guest ids",
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if n := c.deliverTo(ctx, inv); n != 1 {
		t.Fatalf("delivered %d, want 1", n)
	}
	got := f.all("run_inv")
	if len(got) != 1 {
		t.Fatalf("exactly one submit, got %d", len(got))
	}
	// The body verbatim, so nothing has to be fetched afterwards. This is
	// the deliberate break with the nudge, which carries a line and never a
	// body: the courier IS the delivery, not a pointer to one.
	if !strings.Contains(got[0], "look at the second table, the one with the guest ids") {
		t.Fatalf("the body must arrive whole: %q", got[0])
	}
	// And the facts a reply needs: who it is from, and the id ack names.
	if !strings.Contains(got[0], store.RoleOrchestrator) {
		t.Fatalf("the sender must be named: %q", got[0])
	}
	if !strings.Contains(got[0], "msg_") {
		t.Fatalf("the message id must travel with it: %q", got[0])
	}
	// It must not instruct a poll; that is the whole thing being removed.
	for _, banned := range []string{"WAIT", "while true", "curl"} {
		if strings.Contains(got[0], banned) {
			t.Fatalf("delivered mail must not tell anyone to poll (%q): %q", banned, got[0])
		}
	}
}

// Mail a member already holds is its work in progress. However long it takes
// to answer, later deliveries and sweeps must not paste the same brief into
// its prompt again; the body comes round a second time only when the lease
// runs out and the store puts it back on the queue.
func TestHeldMailIsNotPushedAgainUntilItsLeaseRunsOut(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()

	msg, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: "one brief only"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if n := c.deliverTo(ctx, inv); n != 1 {
		t.Fatalf("delivered %d, want 1", n)
	}
	// Long past the store's redelivery grace.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE loop_messages SET delivered_at = 1 WHERE recipient_id = ?`, inv.ID); err != nil {
		t.Fatalf("age the delivery: %v", err)
	}
	c.deliverTo(ctx, inv)
	c.sweep(ctx)
	if got := f.all("run_inv"); len(got) != 1 {
		t.Fatalf("held mail must not be pushed again, got %d submits", len(got))
	}

	// The lease runs out: the message is back on the queue and is sent again.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE loop_messages SET delivered_at = ? WHERE id = ?`, time.Now().Add(-2*time.Hour).UnixMilli(), msg.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n, err := st.RequeueExpiredMail(ctx, store.DefaultLease); err != nil || n != 1 {
		t.Fatalf("requeue: %d %v", n, err)
	}
	if n := c.deliverTo(ctx, inv); n != 1 {
		t.Fatalf("an expired lease must be delivered again, got %d", n)
	}
	if got := f.all("run_inv"); len(got) != 2 {
		t.Fatalf("want the second delivery after the lease ran out, got %d", len(got))
	}
}

func TestTheCourierDeliversAQueueInOrder(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()

	for _, body := range []string{"first", "second", "third"} {
		if _, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: body}); err != nil {
			t.Fatalf("post %s: %v", body, err)
		}
	}
	if n := c.deliverTo(ctx, inv); n != 3 {
		t.Fatalf("delivered %d, want 3", n)
	}
	got := f.all("run_inv")
	if len(got) != 3 {
		t.Fatalf("want three submits, got %d", len(got))
	}
	for i, want := range []string{"first", "second", "third"} {
		if !strings.Contains(got[i], want) {
			t.Fatalf("position %d is %q, want %q", i, got[i], want)
		}
	}
}

// A run whose PTY is gone must not swallow the mail. This is the failure the
// old brief warned about in the other direction: a backgrounded wait put
// messages in a job nobody read, and a write into nothing does the same.
func TestMailForADeadRunGoesBackOnTheQueue(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()
	f.fail = errors.New("tty: no live session for run_inv")

	if _, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: "nobody is home"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if n := c.deliverTo(ctx, inv); n != 0 {
		t.Fatalf("delivered %d into a dead terminal, want 0", n)
	}
	// Pending again, so the member's own fetch and the stranded detector
	// both still see it.
	health, err := st.LoopHealth(ctx, inv.LoopID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	for _, h := range health {
		if h.MemberID != inv.ID {
			continue
		}
		if h.Pending != 1 {
			t.Fatalf("pending = %d, want the message back on the queue: %+v", h.Pending, h)
		}
		if h.Held != 0 {
			t.Fatalf("held = %d; a failed write must not leave mail claimed by nobody", h.Held)
		}
	}
	// Then it succeeds once the terminal is back (and its backoff is over),
	// without anyone re-sending.
	f.fail = nil
	c.endBackoff(inv.ID)
	if n := c.deliverTo(ctx, inv); n != 1 {
		t.Fatalf("redelivered %d, want 1 once the terminal is back", n)
	}
	if got := f.all("run_inv"); len(got) != 1 || !strings.Contains(got[0], "nobody is home") {
		t.Fatalf("want the original body delivered once, got %v", got)
	}
}

// A whole queue must go back, in order, not just the message that failed.
func TestAFailedWriteReturnsTheRestOfTheQueueToo(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()
	f.fail = errors.New("tty: no live session")

	for _, body := range []string{"first", "second", "third"} {
		if _, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: body}); err != nil {
			t.Fatalf("post: %v", err)
		}
	}
	c.deliverTo(ctx, inv)

	var pending int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM loop_messages WHERE recipient_id = ? AND status = ?`,
		inv.ID, store.MailPending).Scan(&pending); err != nil {
		t.Fatalf("count: %v", err)
	}
	if pending != 3 {
		t.Fatalf("pending = %d, want all three back on the queue", pending)
	}

	f.fail = nil
	c.endBackoff(inv.ID)
	c.deliverTo(ctx, inv)
	got := f.all("run_inv")
	if len(got) != 3 {
		t.Fatalf("want three after recovery, got %d", len(got))
	}
	for i, want := range []string{"first", "second", "third"} {
		if !strings.Contains(got[i], want) {
			t.Fatalf("order lost at %d: %q want %q", i, got[i], want)
		}
	}
}

// A member running somewhere agentd did not spawn has no run id, and nothing
// may be written for it. Its mail stays pending for its own fetch.
func TestAMemberWithNoRunIsNotDeliveredTo(t *testing.T) {
	st, f, c, orch, _, external := courierFixture(t)
	ctx := context.Background()

	if _, err := st.PostMail(ctx, orch, external, store.MailPost{Body: "for somewhere else"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if n := c.deliverTo(ctx, external); n != 0 {
		t.Fatalf("delivered %d for a member with no process here", n)
	}
	if len(f.all("run_inv")) != 0 {
		t.Fatal("nothing may be written for a member agentd does not run")
	}
}

// The sweep is the safety net for producers that wake nothing. The store's
// own stranded announcement writes its row directly, so without this pass a
// notice about unread mail would itself go unread.
func TestTheSweepDeliversMailNothingWokeItFor(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()

	if _, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: "found by the sweep"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if n := c.sweep(ctx); n != 1 {
		t.Fatalf("the sweep delivered %d, want 1", n)
	}
	if got := f.all("run_inv"); len(got) != 1 || !strings.Contains(got[0], "found by the sweep") {
		t.Fatalf("want the body, got %v", got)
	}
}

// End to end through the wake channel: posting mail delivers it, with nobody
// polling anything.
func TestPostingMailReachesTheTerminalWithoutAnyonePolling(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	if _, err := st.PostMail(ctx, orch, inv, store.MailPost{
		Subject: "brief", Body: "nobody polled for this",
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.all("run_inv"); len(got) == 1 {
			if !strings.Contains(got[0], "nobody polled for this") {
				t.Fatalf("wrong body: %q", got[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("mail never reached the terminal: %v", f.all("run_inv"))
}

// endBackoff lets a test retry a member at once instead of waiting out the
// backoff a failed write put it in.
func (c *courier) endBackoff(memberID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.members[memberID]; ok {
		st.next = time.Time{}
	}
}

func deliveryCounters(t *testing.T, st *store.Store, member *store.LoopMember) (count int, chars int64, status string) {
	t.Helper()
	ctx := context.Background()
	if err := st.DB().QueryRowContext(ctx,
		`SELECT delivery_count, status FROM loop_messages WHERE recipient_id = ? ORDER BY seq LIMIT 1`,
		member.ID).Scan(&count, &status); err != nil {
		t.Fatalf("message: %v", err)
	}
	m, err := st.LoopMemberByID(ctx, member.ID)
	if err != nil || m == nil {
		t.Fatalf("member: %v", err)
	}
	return count, m.CharsIn, status
}

// A write that never reached the terminal must not count as a delivery: the
// member would look as if it had been handed its brief when it read nothing.
func TestAFailedWriteCountsNothing(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()
	f.fail = errors.New("tty: no live session")
	if _, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: "not delivered"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	c.deliverTo(ctx, inv)
	if count, chars, status := deliveryCounters(t, st, inv); count != 0 || chars != 0 || status != store.MailPending {
		t.Fatalf("a failed write counted: count=%d chars=%d status=%s", count, chars, status)
	}
	f.fail = nil
	c.endBackoff(inv.ID)
	c.deliverTo(ctx, inv)
	if count, chars, _ := deliveryCounters(t, st, inv); count != 1 || chars != int64(len("not delivered")) {
		t.Fatalf("a successful write counts once: count=%d chars=%d", count, chars)
	}
}

// A member whose writes keep failing is tried less and less often, not on
// every wake and every sweep.
func TestAFailingMemberBacksOff(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()
	f.fail = errors.New("tty: no live session")
	if _, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: "retry me"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	var attempts int
	counting := c.submit
	c.submit = func(runID, text string) error { attempts++; return counting(runID, text) }

	c.deliverTo(ctx, inv)
	c.deliverTo(ctx, inv)
	c.sweep(ctx)
	if attempts != 1 {
		t.Fatalf("a member in backoff must not be written to, got %d attempts", attempts)
	}
	c.mu.Lock()
	first := time.Until(c.members[inv.ID].next)
	c.mu.Unlock()
	c.endBackoff(inv.ID)
	c.deliverTo(ctx, inv)
	c.mu.Lock()
	second := time.Until(c.members[inv.ID].next)
	c.mu.Unlock()
	if attempts != 2 || second <= first {
		t.Fatalf("the backoff must grow: attempts=%d first=%v second=%v", attempts, first, second)
	}
	if backoff(100) != courierBackoffMax {
		t.Fatalf("the backoff is capped, got %v", backoff(100))
	}
}

// A loop over one of its limits gets nothing more written into its members'
// terminals, and nothing is left claimed: the mail stays on the queue.
func TestCourierReleasesMailWhenCapped(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE loops SET max_messages_per_round = 1 WHERE id = ?`, inv.LoopID); err != nil {
		t.Fatalf("cap: %v", err)
	}
	for _, body := range []string{"one", "two"} {
		if _, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: body}); err != nil {
			t.Fatalf("post: %v", err)
		}
	}
	if n := c.deliverTo(ctx, inv); n != 0 {
		t.Fatalf("a capped loop must get no deliveries, got %d", n)
	}
	if got := f.all("run_inv"); len(got) != 0 {
		t.Fatalf("nothing may be written for a capped loop: %v", got)
	}
	var held int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM loop_messages WHERE recipient_id = ? AND status != ?`,
		inv.ID, store.MailPending).Scan(&held); err != nil {
		t.Fatalf("count: %v", err)
	}
	if held != 0 {
		t.Fatalf("a capped delivery must leave no mail claimed, %d are", held)
	}
}

// One terminal that stops reading must not hold up every other member's
// mail: deliveries run per member, and a write has a deadline.
func TestAStuckTerminalDoesNotBlockOtherMembers(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unblock := make(chan struct{})
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	c.writeTimeout = 100 * time.Millisecond
	c.submit = func(runID, text string) error {
		if runID == "run_inv" {
			<-unblock
		}
		return f.submit(runID, text)
	}
	go c.Run(ctx)

	stuck, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: "into a stuck terminal"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	engineer, err := st.LoopMemberByRole(ctx, inv.LoopID, store.RoleEngineer)
	if err != nil || engineer == nil {
		t.Fatalf("engineer: %v", err)
	}
	if _, err := st.PostMail(ctx, engineer, orch, store.MailPost{Body: "for a healthy terminal"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(f.all("run_orch")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a stuck terminal held up another member's mail")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The stuck message stays held while its write may still land.
	m, _ := st.GetLoopMessage(ctx, inv.LoopID, stuck.ID)
	if m.Status != store.MailDelivered || m.DeliveryCount != 0 {
		t.Fatalf("a timed-out write keeps the message held and uncounted: %+v", m)
	}
	close(unblock)
	deadline = time.Now().Add(5 * time.Second)
	for {
		m, _ = st.GetLoopMessage(ctx, inv.LoopID, stuck.ID)
		if m.DeliveryCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a write that landed late must be counted once it lands: %+v", m)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A body is text for the member to read, never keystrokes. Ending the paste
// and typing a command, or any other control sequence, must arrive inert.
func TestAHostileBodyIsDeliveredInert(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()
	body := "hello\x1b[201~\r/quit\r\x1b[200~\x07bell\x00nul\x7fdel\u009bcsi\u0085nel\ttab\nline"
	if _, err := st.PostMail(ctx, orch, inv, store.MailPost{Subject: "sub\x1b]0;title\x07ject", Body: body}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if n := c.deliverTo(ctx, inv); n != 1 {
		t.Fatalf("delivered %d", n)
	}
	got := f.all("run_inv")[0]
	for _, r := range got {
		switch {
		case r == '\n' || r == '\t':
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			t.Fatalf("control character %U reached the terminal: %q", r, got)
		}
	}
	if strings.Contains(got, "\r") {
		t.Fatalf("a carriage return would submit the prompt: %q", got)
	}
	if !strings.Contains(got, "/quit") || !strings.Contains(got, "bell") {
		t.Fatalf("the text itself must still arrive: %q", got)
	}
}

// One message cannot flood a member's prompt: an oversized body is replaced
// with a pointer to the message.
func TestAnOversizedBodyBecomesAPointer(t *testing.T) {
	st, f, c, orch, inv, _ := courierFixture(t)
	ctx := context.Background()
	body := strings.Repeat("x", 17*1024)
	msg, err := st.PostMail(ctx, orch, inv, store.MailPost{Body: body})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if n := c.deliverTo(ctx, inv); n != 1 {
		t.Fatalf("delivered %d", n)
	}
	got := f.all("run_inv")[0]
	if strings.Contains(got, strings.Repeat("x", 1024)) || len(got) > 1024 {
		t.Fatalf("an oversized body must not be pasted: %d bytes", len(got))
	}
	if !strings.Contains(got, msg.ID) || !strings.Contains(got, "mail API") {
		t.Fatalf("the pointer must name the message and where to read it: %q", got)
	}
}
