// mailbox_test.go — the delivery guarantees of migration 0016. Each test here
// fails if its guarantee is removed: atomic claim, leases, lost-delivery
// recovery, verbatim forwarding, routing, and never truncating a body.
package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustLoop(t *testing.T, s *Store, task string) (*Loop, map[string]*LoopMember) {
	t.Helper()
	ctx := context.Background()
	l := &Loop{Task: task, CWD: "/tmp/target", MaxMembers: 64}
	if _, err := s.CreateLoop(ctx, l, nil); err != nil {
		t.Fatalf("create loop: %v", err)
	}
	members, err := s.ListLoopMembers(ctx, l.ID)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	byRole := map[string]*LoopMember{}
	for _, m := range members {
		byRole[m.Role] = m
	}
	return l, byRole
}

func mustPost(t *testing.T, s *Store, from, to *LoopMember, body string) *LoopMessage {
	t.Helper()
	m, err := s.PostMail(context.Background(), from, to, MailPost{Body: body})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return m
}

// Two pollers on one token must never both receive the same message. The
// claim is deliberately ONE stamping UPDATE: a select-then-update claim takes
// a read snapshot it must later upgrade, which under this many pollers does
// not merely risk handing one brief out twice, it fails outright with
// SQLITE_BUSY_SNAPSHOT. The drain loop below is what makes that visible, so
// the claims counter is asserted too: a run where one poller swallowed the
// queue proves nothing.
func TestConcurrentPollersNeverGetTheSameMessage(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "concurrency")
	review := roles["REVIEW"]

	const total = 200
	for i := 0; i < total; i++ {
		mustPost(t, s, roles[RoleOrchestrator], review, "brief")
	}

	const pollers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]int{}
	claims := 0
	start := make(chan struct{})
	for i := 0; i < pollers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				got, err := s.ClaimInbox(ctx, review, ClaimOptions{Limit: 3})
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				claims++
				for _, m := range got {
					seen[m.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if claims <= pollers {
		t.Fatalf("only %d claims for %d pollers: the pollers never overlapped, so this proves nothing", claims, pollers)
	}
	if len(seen) != total {
		t.Fatalf("claimed %d distinct messages, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("message %s handed out %d times, want exactly 1", id, n)
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT delivery_count FROM loop_messages WHERE recipient_role = 'REVIEW'`)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != 1 {
			t.Fatalf("delivery_count = %d, want 1", n)
		}
	}
}

// A message held under a live lease stays held; one whose lease has run out
// comes back to the queue.
func TestLeaseHoldsUntilItExpires(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "leases")
	review := roles["REVIEW"]

	long, err := s.PostMail(ctx, roles[RoleOrchestrator], review,
		MailPost{Body: "long job", LeaseSeconds: 3600})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	short := mustPost(t, s, roles[RoleOrchestrator], review, "short job")

	claimed, err := s.ClaimInbox(ctx, review, ClaimOptions{})
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim: %v (%d messages)", err, len(claimed))
	}

	// A zero default lease expires everything that carries no lease of its
	// own; the explicit hour-long lease survives.
	n, err := s.RequeueExpiredMail(ctx, 0)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n != 1 {
		t.Fatalf("requeued %d, want only the message without its own lease", n)
	}
	back, err := s.GetLoopMessage(ctx, short.LoopID, short.ID)
	if err != nil || back.Status != MailPending {
		t.Fatalf("short job status = %v (%v), want pending", back.Status, err)
	}
	held, err := s.GetLoopMessage(ctx, long.LoopID, long.ID)
	if err != nil || held.Status != MailDelivered {
		t.Fatalf("leased job status = %v (%v), want delivered", held.Status, err)
	}

	ok, err := s.ExtendMailLease(ctx, review.ID, long.ID, 7200)
	if err != nil || !ok {
		t.Fatalf("extend: %v %v", ok, err)
	}
	if _, err := s.AckMail(ctx, review.ID, long.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, err := s.RequeueExpiredMail(ctx, 0); err != nil || n != 0 {
		t.Fatalf("acked work must never requeue: %d %v", n, err)
	}
	if ok, _ := s.ExtendMailLease(ctx, review.ID, "msg_missing", 60); ok {
		t.Fatal("extending an unknown message must report false")
	}
}

// A member that polls while holding an unacked message never worked on it: the
// delivery was lost, so it is handed back flagged redelivered.
func TestLostDeliveryComesBackFlaggedRedelivered(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "lost delivery")
	review := roles["REVIEW"]
	posted := mustPost(t, s, roles[RoleOrchestrator], review, "the report")

	noGrace := time.Duration(0)
	first, err := s.ClaimInbox(ctx, review, ClaimOptions{RedeliverAfter: &noGrace})
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %v (%d)", err, len(first))
	}
	if first[0].Redelivered {
		t.Fatal("a first delivery is not a redelivery")
	}

	again, err := s.ClaimInbox(ctx, review, ClaimOptions{RedeliverAfter: &noGrace})
	if err != nil || len(again) != 1 {
		t.Fatalf("second claim: %v (%d) — a lost delivery must come back", err, len(again))
	}
	if again[0].ID != posted.ID || !again[0].Redelivered {
		t.Fatalf("want %s flagged redelivered, got %+v", posted.ID, again[0])
	}
	if again[0].Body != "the report" {
		t.Fatalf("redelivery must carry the whole body, got %q", again[0].Body)
	}

	if _, err := s.AckMail(ctx, review.ID, posted.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	after, err := s.ClaimInbox(ctx, review, ClaimOptions{RedeliverAfter: &noGrace})
	if err != nil || len(after) != 0 {
		t.Fatalf("an acked message must not come back: %v %d", err, len(after))
	}
}

// The lost-delivery path must not reopen the double-claim race: inside the
// grace window concurrent pollers still see each message once.
func TestRedeliveryGraceStillBlocksConcurrentPollers(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "grace")
	review := roles["REVIEW"]
	for i := 0; i < 8; i++ {
		mustPost(t, s, roles[RoleOrchestrator], review, "brief")
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ClaimInbox(ctx, review, ClaimOptions{})
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			for _, m := range got {
				seen[m.ID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 8 {
		t.Fatalf("claimed %d distinct, want 8", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("message %s handed out %d times", id, n)
		}
	}
}

// Forwarding copies the body server side. Retyping a report is how evidence
// gets lost, so verbatim has to be a fact rather than an instruction.
func TestForwardCopiesTheBodyByteForByte(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "forwarding")
	orchestrator, investigation, review := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["REVIEW"]

	report := "F1. finding\n" + strings.Repeat("evidence line — with a multibyte dash\n", 4000)
	reported := mustPost(t, s, investigation, orchestrator, report)

	received, err := s.ClaimInbox(ctx, orchestrator, ClaimOptions{})
	if err != nil || len(received) != 1 {
		t.Fatalf("claim: %v (%d)", err, len(received))
	}
	if received[0].Body != report {
		t.Fatal("a claimed report must be byte-identical to what was sent")
	}

	forwarded, err := s.ForwardMail(ctx, orchestrator, review, MailForward{OriginalID: reported.ID, Subject: "round 1 findings"})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if forwarded.ForwardedFrom != reported.ID {
		t.Fatalf("forwarded_from = %q, want %q", forwarded.ForwardedFrom, reported.ID)
	}
	atReview, err := s.ClaimInbox(ctx, review, ClaimOptions{})
	if err != nil || len(atReview) != 1 {
		t.Fatalf("review claim: %v (%d)", err, len(atReview))
	}
	if atReview[0].Body != report {
		t.Fatalf("forwarded body differs: %d vs %d chars", len(atReview[0].Body), len(report))
	}
	if atReview[0].ForwardedFrom != reported.ID {
		t.Fatal("the copy must record what it was copied from")
	}

	if _, err := s.ForwardMail(ctx, orchestrator, review, MailForward{OriginalID: "msg_missing"}); err == nil {
		t.Fatal("forwarding an unknown id must fail")
	}

	// A loop outside the lineage cannot be reached.
	_, other := mustLoop(t, s, "unrelated")
	stranger := mustPost(t, s, other[RoleOrchestrator], other["REVIEW"], "not yours")
	if _, err := s.ForwardMail(ctx, orchestrator, review, MailForward{OriginalID: stranger.ID}); err == nil {
		t.Fatal("forwarding across unrelated loops must fail")
	}
}

// Every path terminates at the orchestrator.
func TestWorkersMayOnlyMessageTheOrchestrator(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "routing")

	if _, err := s.PostMail(ctx, roles["INVESTIGATION"], roles["REVIEW"], MailPost{Body: "psst"}); err == nil {
		t.Fatal("worker to worker must be refused")
	}
	// A worker MAY narrate to the engineer: the chat is a place the engineer
	// looks, not a push. It is mirrored to the orchestrator, so reaching the
	// engineer costs the orchestrator no sight of its own loop.
	if _, err := s.PostMail(ctx, roles["INVESTIGATION"], roles[RoleEngineer], MailPost{Body: "narrating"}); err != nil {
		t.Fatalf("a worker must be able to narrate to the engineer: %v", err)
	}
	if _, err := s.PostMail(ctx, roles["INVESTIGATION"], roles[RoleOrchestrator], MailPost{Body: "report"}); err != nil {
		t.Fatalf("worker to orchestrator must be allowed: %v", err)
	}
	if _, err := s.PostMail(ctx, roles[RoleOrchestrator], roles["REVIEW"], MailPost{Body: "brief"}); err != nil {
		t.Fatalf("orchestrator to worker must be allowed: %v", err)
	}
	if _, err := s.PostMail(ctx, roles[RoleOrchestrator], roles[RoleEngineer], MailPost{Body: "progress"}); err != nil {
		t.Fatalf("orchestrator to engineer must be allowed: %v", err)
	}
	if _, err := s.PostMail(ctx, roles[RoleEngineer], roles["IMPLEMENTATION"], MailPost{Body: "do this"}); err != nil {
		t.Fatalf("the engineer may address anyone: %v", err)
	}
	if _, err := s.PostMail(ctx, roles[RoleOrchestrator], roles[RoleOrchestrator], MailPost{Body: "hi"}); err == nil {
		t.Fatal("messaging yourself must be refused")
	}
}

// No route truncates. Either the whole body or a pointer with its size.
func TestNothingIsEverTruncated(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "truncation")

	body := strings.Repeat("é", 500_000)
	posted := mustPost(t, s, roles[RoleOrchestrator], roles["REVIEW"], body)
	if posted.BodyChars != 500_000 {
		t.Fatalf("body_chars = %d, want the rune count 500000", posted.BodyChars)
	}

	claimed, err := s.ClaimInbox(ctx, roles["REVIEW"], ClaimOptions{})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (%d)", err, len(claimed))
	}
	if claimed[0].Body != body || claimed[0].BodyChars != 500_000 {
		t.Fatal("the inbox must return the whole body")
	}
	got, err := s.GetLoopMessage(ctx, loop.ID, posted.ID)
	if err != nil || got.Body != body {
		t.Fatalf("get by id must return the whole body: %v", err)
	}
	transcript, err := s.ListLoopMessages(ctx, loop.ID, 0)
	if err != nil || len(transcript) != 1 || transcript[0].Body != body {
		t.Fatalf("the transcript must return whole bodies: %v", err)
	}

	// The archive is the pointer form: sizes, never a partial body.
	index, err := s.MailArchive(ctx, []string{loop.ID}, 0)
	if err != nil || len(index) != 1 {
		t.Fatalf("archive: %v (%d)", err, len(index))
	}
	if index[0].BodyChars != 500_000 {
		t.Fatalf("archive body_chars = %d, want 500000", index[0].BodyChars)
	}
}

// Ending a loop stops the work: nothing queued survives and writes are refused.
func TestEndedLoopCancelsQueuedWorkAndRefusesWrites(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "ending")
	review := roles["REVIEW"]

	mustPost(t, s, roles[RoleOrchestrator], review, "queued")
	held := mustPost(t, s, roles[RoleOrchestrator], review, "in flight")
	if _, err := s.ClaimInbox(ctx, review, ClaimOptions{Limit: 1}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	cancelled, err := s.EndLoop(ctx, loop.ID, LoopComplete, "round one done", false)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if cancelled != 2 {
		t.Fatalf("cancelled %d messages, want 2", cancelled)
	}
	after, err := s.ClaimInbox(ctx, review, ClaimOptions{})
	if err != nil || len(after) != 0 {
		t.Fatalf("a restarting agent must not pick up stale work: %v %d", err, len(after))
	}
	dead, err := s.GetLoopMessage(ctx, loop.ID, held.ID)
	if err != nil || dead.Status != MailCancelled {
		t.Fatalf("in-flight message status = %v (%v), want cancelled", dead.Status, err)
	}
	if _, err := s.PostMail(ctx, roles[RoleOrchestrator], review, MailPost{Body: "more work"}); err == nil {
		t.Fatal("an ended loop must refuse writes")
	}
}

// A member's own history is what a replacement session reads instead of
// inheriting a veteran's context.
func TestThreadIsTheMembersOwnHistory(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "thread")
	orchestrator, investigation, review := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["REVIEW"]

	mustPost(t, s, orchestrator, investigation, "brief")
	mustPost(t, s, investigation, orchestrator, "report")
	mustPost(t, s, orchestrator, review, "not yours")

	thread, err := s.MailThread(ctx, investigation.ID, 0)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if len(thread) != 2 {
		t.Fatalf("thread has %d messages, want the 2 this member sent or received", len(thread))
	}
	if thread[0].Body != "brief" || thread[1].Body != "report" {
		t.Fatalf("thread must read oldest-first: %q then %q", thread[0].Body, thread[1].Body)
	}
}

// Ordering must survive a burst. created_at is UnixMilli and the ids are
// random hex, so a whole burst of messages shares one timestamp with no
// tiebreak: ordering by created_at alone returns them shuffled. Every read
// orders by the insertion sequence instead.
func TestOrderingSurvivesASameMillisecondBurst(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "ordering")
	orchestrator, investigation := roles[RoleOrchestrator], roles["INVESTIGATION"]

	const burst = 40
	want := make([]string, 0, burst)
	for i := 0; i < burst; i++ {
		body := fmt.Sprintf("m%02d", i)
		want = append(want, body)
		if i%2 == 0 {
			mustPost(t, s, orchestrator, investigation, body)
		} else {
			mustPost(t, s, investigation, orchestrator, body)
		}
	}

	var stamps int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT created_at) FROM loop_messages WHERE loop_id = ?`, loop.ID).Scan(&stamps); err != nil {
		t.Fatalf("stamps: %v", err)
	}
	if stamps == burst {
		t.Skip("this machine stamped every message a distinct millisecond; the burst proves nothing here")
	}

	thread, err := s.MailThread(ctx, investigation.ID, burst)
	if err != nil || len(thread) != burst {
		t.Fatalf("thread: %v (%d)", err, len(thread))
	}
	for i, m := range thread {
		if m.Body != want[i] {
			t.Fatalf("thread position %d = %q, want %q — same-millisecond rows must keep insertion order", i, m.Body, want[i])
		}
	}

	transcript, err := s.ListLoopMessages(ctx, loop.ID, burst)
	if err != nil || len(transcript) != burst {
		t.Fatalf("transcript: %v (%d)", err, len(transcript))
	}
	for i, m := range transcript {
		if m.Body != want[i] {
			t.Fatalf("transcript position %d = %q, want %q", i, m.Body, want[i])
		}
	}

	index, err := s.MailArchive(ctx, []string{loop.ID}, burst)
	if err != nil || len(index) != burst {
		t.Fatalf("archive: %v (%d)", err, len(index))
	}
	if index[0].ID != transcript[0].ID || index[burst-1].ID != transcript[burst-1].ID {
		t.Fatal("the archive index must agree with the transcript order")
	}

	// The claim hands them out in the same order.
	claimed, err := s.ClaimInbox(ctx, investigation, ClaimOptions{Limit: burst})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	prev := -1
	for _, m := range claimed {
		var got int
		if _, err := fmt.Sscanf(m.Body, "m%d", &got); err != nil {
			t.Fatalf("body %q: %v", m.Body, err)
		}
		if got <= prev {
			t.Fatalf("claim returned %q after m%02d — out of order", m.Body, prev)
		}
		prev = got
	}
}

// The step's own advance is the ack.
//
// Both investigators in the live loops reported their work and neither acked.
// Their briefs stayed delivered forever and came back on every poll, forty-one
// times in one case, each re-wake burning context on work already done. The
// ack was a voluntary second step at exactly the moment a model feels
// finished, so it was the step that got forgotten.
//
// A post that discharges the current step is proof the sender read its brief
// and acted, which is a stronger fact than the ack it skipped.
func TestAReportThatDischargesTheStepAcksWhatTheSenderHeld(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "recon")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]
	mustStartPlay(t, s, loop.ID, "recon")

	// The investigator claims the orchestrator's brief and does NOT ack it.
	send(t, s, loop.ID, orch, inv, "", "investigate")
	held, err := s.ClaimInbox(ctx, inv, ClaimOptions{Limit: 10})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(held) == 0 {
		t.Fatal("the investigator must have been briefed")
	}
	for _, m := range held {
		if m.Status != MailDelivered {
			t.Fatalf("a claim without AutoAck leaves the message delivered, got %q", m.Status)
		}
	}

	// It reports, which discharges the step. Nothing calls Ack.
	send(t, s, loop.ID, inv, orch, "", "conclude")

	for _, m := range held {
		got, err := s.GetLoopMessage(ctx, loop.ID, m.ID)
		if err != nil || got == nil {
			t.Fatalf("reread %s: %v", m.ID, err)
		}
		if got.Status != MailAcked {
			t.Fatalf("the report must discharge %s, still %q", m.ID, got.Status)
		}
		if got.AckedAt == 0 {
			t.Fatalf("%s was acked with no timestamp", m.ID)
		}
	}
}

// The discharge acks what the sender HELD. Mail it never received is not
// held, and marking that acked would silently swallow the next instruction.
//
// The waiting message is written BEFORE the report, so it is in the table
// while the discharge runs. Written after, no mutation of the discharge could
// touch it and the test would prove nothing.
func TestTheDischargeAcksOnlyWhatTheSenderActuallyHeld(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "recon")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]
	mustStartPlay(t, s, loop.ID, "recon")
	send(t, s, loop.ID, orch, inv, "", "investigate")
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{Limit: 10}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// The engineer writes while the investigator is still working. This sits
	// pending: it was never claimed, so it was never read.
	waiting := mustPost(t, s, roles[RoleEngineer], inv, "one more thing before you finish")
	if waiting.Status != MailPending {
		t.Fatalf("precondition: the extra message must be pending, got %q", waiting.Status)
	}

	// Now the report discharges the step.
	send(t, s, loop.ID, inv, orch, "", "conclude")

	got, err := s.GetLoopMessage(ctx, loop.ID, waiting.ID)
	if err != nil || got == nil {
		t.Fatalf("reread: %v", err)
	}
	if got.Status != MailPending {
		t.Fatalf("mail the member never received must be untouched, got %q", got.Status)
	}
	next, err := s.ClaimInbox(ctx, inv, ClaimOptions{Limit: 10})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	found := false
	for _, m := range next {
		if m.ID == waiting.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the waiting message must still be deliverable after the discharge")
	}
}

// The redelivery storm. A delivered message older than the grace came back on
// every poll forever; the fix is that a member which has POSTED since the
// delivery demonstrably acted on it, so it is not redelivered.
func TestADeliveryOlderThanTheSendersLastPostIsNotRedelivered(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	_, roles := mustLoop(t, s, "storm")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	// No play here on purpose: this is the redelivery window, not the
	// discharge, and the two must hold independently.
	brief := mustPost(t, s, orch, inv, "look into the export")
	zero := time.Duration(0)
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{Limit: 10, RedeliverAfter: &zero}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Before it posts anything, the grace expiring DOES bring the brief back:
	// that is the lost-delivery recovery and it must survive.
	again, err := s.ClaimInbox(ctx, inv, ClaimOptions{Limit: 10, RedeliverAfter: &zero})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(again) != 1 || again[0].ID != brief.ID {
		t.Fatalf("a lost delivery must still come back, got %d", len(again))
	}

	// It posts. Now the same brief is history.
	mustPost(t, s, inv, orch, "here is what I found")
	third, err := s.ClaimInbox(ctx, inv, ClaimOptions{Limit: 10, RedeliverAfter: &zero})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, m := range third {
		if m.ID == brief.ID {
			t.Fatalf("the brief came back after the member acted on it: delivery %d", m.DeliveryCount)
		}
	}

	// And HasClaimableMail agrees, or a long-poll would wake for mail that
	// ClaimInbox then refuses to hand over, which is a spin.
	has, err := s.HasClaimableMail(ctx, inv, &zero)
	if err != nil {
		t.Fatalf("has: %v", err)
	}
	if has {
		t.Fatal("HasClaimableMail must not promise mail ClaimInbox will not give")
	}
}
