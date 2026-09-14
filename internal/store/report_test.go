package store

import (
	"context"
	"testing"
	"time"
)

// A member's report reached the orchestrator only if the member chose to POST
// it. A live OpenCode investigator did the whole task, printed a complete PR
// count report into its own terminal, and stopped. The orchestrator's inbox
// stayed empty and the loop sat on its investigate step with no recovery
// path: the stranded detector only catches a member sitting on UNREAD mail,
// and this member had read its mail.

func seedEvent(t *testing.T, s *Store, sessionID, event, content string, ts int64, source string) {
	t.Helper()
	if err := s.InsertBatch(context.Background(), &Batch{
		Events: []Incoming{{
			SessionID: sessionID, Seq: ts, Event: event, Content: content,
			TS: ts, Source: source, UUID: event + "-" + itoa64(ts),
		}},
		Session: &SessionMeta{SessionID: sessionID, Project: "p", CWD: "/w",
			FilePath: "/" + sessionID + ".jsonl"},
	}); err != nil {
		t.Fatalf("seed %s@%d: %v", event, ts, err)
	}
}

func itoa64(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(digits[n%10]) + out
		n /= 10
	}
	return out
}

func TestLastReportSinceReadsTheMembersOwnAnswer(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	seedEvent(t, s, "sess-a", "assistant_message", "the PREVIOUS task's answer", 1000, "tailer")
	seedEvent(t, s, "sess-a", "tool_use", "", 2100, "tailer")
	seedEvent(t, s, "sess-a", "assistant_message", "## PR Count Report", 2200, "tailer")
	seedEvent(t, s, "sess-a", "tool_result", "331", 2300, "tailer")

	got, err := s.LastReportSince(ctx, "sess-a", 2000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Report != "## PR Count Report" {
		t.Fatalf("report = %q, want the answer produced after the brief", got.Report)
	}
	if got.ReportAt != 2200 {
		t.Errorf("reportAt = %d, want 2200", got.ReportAt)
	}
	// LastEventAt is every kind, not just answers: a member that wrote a
	// summary and then kept using tools has not finished.
	if got.LastEventAt != 2300 {
		t.Errorf("lastEventAt = %d, want the newest event of any kind", got.LastEventAt)
	}
}

// An answer from before the brief is the previous task's, and forwarding it
// would answer a question nobody asked.
func TestLastReportSinceIgnoresOutputFromBeforeTheBrief(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()

	seedEvent(t, s, "sess-b", "assistant_message", "answered the last brief", 1000, "tailer")
	got, err := s.LastReportSince(context.Background(), "sess-b", 5000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Report != "" {
		t.Fatalf("report = %q, want nothing since the brief", got.Report)
	}
}

// Empty assistant messages are real and common; forwarding one would post a
// blank report to the orchestrator.
func TestLastReportSinceSkipsEmptyOutput(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()

	seedEvent(t, s, "sess-c", "assistant_message", "the real answer", 2000, "tailer")
	seedEvent(t, s, "sess-c", "assistant_message", "   \n  ", 2500, "tailer")
	got, err := s.LastReportSince(context.Background(), "sess-c", 1000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Report != "the real answer" {
		t.Fatalf("report = %q, want the last NON-empty answer", got.Report)
	}
}

// Both engines, one query. OpenCode sessions are missing from the sessions
// INDEX, which is what the corpus gap is about; their events are present and
// stamped source='opencode'. Verified on the live database, where the report
// that went missing is 2,827 characters of assistant_message.
//
// The ordering is the trap: every OpenCode event row carries seq = 0, so
// ordering by seq returns an arbitrary row for exactly the engine this is
// meant to rescue.
func TestLastReportSinceWorksForOpenCodeWhereEverySeqIsZero(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	// Inserted NEWEST FIRST on purpose. Every seq is 0, so ordering by seq
	// leaves a tie that SQLite breaks by rowid, which is insertion order —
	// and insertion order here is the wrong answer. Only ordering by ts
	// gets it right, which is the whole point.
	for _, e := range []struct {
		content string
		ts      int64
	}{
		{"## PR Count Report", 2400},
		{"first pass", 2100},
	} {
		if err := s.InsertBatch(ctx, &Batch{
			Events: []Incoming{{
				SessionID: "ses_oc", Seq: 0, Event: "assistant_message",
				Content: e.content, TS: e.ts, Source: "opencode",
				UUID: "oc-" + itoa64(e.ts),
			}},
			Session: &SessionMeta{SessionID: "ses_oc", Project: "p", CWD: "/w",
				FilePath: "/oc.jsonl"},
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	got, err := s.LastReportSince(ctx, "ses_oc", 2000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Report != "## PR Count Report" {
		t.Fatalf("report = %q; ordering must be by ts, since every opencode seq is 0", got.Report)
	}
}

// The detection itself.
func TestFinishedUnreportedNeedsWorkQuietAndUnsent(t *testing.T) {
	now := time.Now().UnixMilli()
	quiet := now - QuietFor.Milliseconds() - 1000
	base := MemberHealth{
		Held: 1, OldestHeldAt: quiet - 10000, HeldBriefs: 1, OldestBriefAt: quiet - 10000,
		Report: "## PR Count Report", ReportAt: quiet, LastEventAt: quiet,
	}
	if !base.FinishedUnreported(now) {
		t.Fatal("a member holding a brief, with an answer, gone quiet, owes a report")
	}

	// Still mid-turn: it emitted something a moment ago.
	busy := base
	busy.LastEventAt = now - 1000
	if busy.FinishedUnreported(now) {
		t.Error("a member still emitting events is working, not finished")
	}

	// Produced nothing since the brief.
	silent := base
	silent.Report, silent.ReportAt = "", 0
	if silent.FinishedUnreported(now) {
		t.Error("no answer means nothing to forward")
	}

	// Its answer predates the brief.
	stale := base
	stale.ReportAt = base.OldestBriefAt - 5000
	if stale.FinishedUnreported(now) {
		t.Error("an answer from before the brief belongs to the previous task")
	}

	// Whitespace is not an answer.
	blank := base
	blank.Report = "   \n\t "
	if blank.FinishedUnreported(now) {
		t.Error("whitespace must never be forwarded as a report")
	}

	// Holding nothing: it has already been discharged.
	done := base
	done.Held, done.HeldBriefs = 0, 0
	if done.FinishedUnreported(now) {
		t.Error("a discharged member owes nothing")
	}

	// Holding mail that is not a step brief: a note from the engineer, a
	// stranded notice. Nothing in the play is waiting on an answer to it.
	notABrief := base
	notABrief.HeldBriefs, notABrief.OldestBriefAt = 0, 0
	if notABrief.FinishedUnreported(now) {
		t.Error("only a member holding a step brief can owe the play a report")
	}
}

// End to end through the sweep: the state, and the sweep signal carrying the
// work so the caller can deliver it.
func TestTheSweepReportsAMemberThatFinishedAndSaidNothing(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	loop, roles := mustLoop(t, s, "prove a finished member is noticed")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]
	mustStartPlay(t, s, loop.ID, "recon")

	now := time.Now().UnixMilli()
	// Briefing the investigator moves the play onto its step, and the engine
	// sends it the step brief it now owes an answer to.
	mustPost(t, s, orch, inv, "count the PRs")
	// The courier delivered it.
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET delivered_at = ? WHERE recipient_id = ?`, now-600000, inv.ID); err != nil {
		t.Fatalf("age the delivery: %v", err)
	}
	// It has a transcript, and it answered.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET session_id = ? WHERE id = ?`, "sess-inv", inv.ID); err != nil {
		t.Fatalf("bind session: %v", err)
	}
	seedEvent(t, s, "sess-inv", "assistant_message", "## PR Count Report\n4039 merged", now-300000, "opencode")

	res, err := s.sweepAt(ctx, now, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Unreported) != 1 {
		t.Fatalf("the sweep must surface exactly the silent member, got %+v", res.Unreported)
	}
	u := res.Unreported[0]
	if u.Role != "INVESTIGATION" || u.LoopID != loop.ID {
		t.Fatalf("wrong member: %+v", u)
	}
	// The work travels with the signal, because the caller has to be able to
	// deliver it without going back to the transcript itself.
	if u.Report != "## PR Count Report\n4039 merged" {
		t.Fatalf("report = %q, want the member's own output", u.Report)
	}
	if u.BriefAt == 0 {
		t.Error("the brief must be identified, so a caller can act once per brief")
	}
	// And it is NOT stranded: it read its mail. Calling it stranded is how
	// this detector would lose its meaning.
	for _, st := range res.Stranded {
		if st.MemberID == inv.ID {
			t.Fatalf("a member that read its brief is not stranded: %+v", st)
		}
	}
}

// The orchestrator reports to the human, not to itself.
func TestTheOrchestratorIsNeverAskedToReportToItself(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	_, roles := mustLoop(t, s, "the orchestrator is exempt from the guarantee")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	now := time.Now().UnixMilli()
	// Mail TO the orchestrator, delivered and undischarged, with output.
	m := mustPost(t, s, inv, orch, "here is what I found")
	if _, err := s.ClaimInbox(ctx, orch, ClaimOptions{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET delivered_at = ? WHERE id = ?`, now-600000, m.ID); err != nil {
		t.Fatalf("age: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET session_id = ? WHERE id = ?`, "sess-orch", orch.ID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	seedEvent(t, s, "sess-orch", "assistant_message", "thinking about it", now-300000, "tailer")

	res, err := s.sweepAt(ctx, now, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, u := range res.Unreported {
		if u.Role == RoleOrchestrator {
			t.Fatalf("the orchestrator must never be forwarded to itself: %+v", u)
		}
	}
}

// A member that DID post owes nothing, whatever its transcript says.
func TestAMemberThatPostedIsNotChased(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	_, roles := mustLoop(t, s, "a member that reported is left alone")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	now := time.Now().UnixMilli()
	brief := mustPost(t, s, orch, inv, "count the PRs")
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET delivered_at = ? WHERE id = ?`, now-600000, brief.ID); err != nil {
		t.Fatalf("age: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET session_id = ? WHERE id = ?`, "sess-inv2", inv.ID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	seedEvent(t, s, "sess-inv2", "assistant_message", "## PR Count Report", now-300000, "opencode")

	// It reports, the ordinary way.
	if _, err := s.PostMail(ctx, inv, orch, MailPost{Body: "## PR Count Report"}); err != nil {
		t.Fatalf("post: %v", err)
	}
	res, err := s.sweepAt(ctx, now, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, u := range res.Unreported {
		if u.MemberID == inv.ID {
			t.Fatalf("a member that posted must not be chased: %+v", u)
		}
	}
}

// A member holding only ordinary mail, with no step brief among it, is not
// chased for a report: nothing in the play is waiting on it, and forwarding
// its output would put words in its mouth about a question nobody asked.
func TestAMemberWithoutAStepBriefIsNotChased(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	_, roles := mustLoop(t, s, "only a brief makes a report owed")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	now := time.Now().UnixMilli()
	note := mustPost(t, s, orch, inv, "fyi, the build is green again")
	if _, err := s.ClaimInbox(ctx, inv, ClaimOptions{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET delivered_at = ? WHERE id = ?`, now-600000, note.ID); err != nil {
		t.Fatalf("age: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET session_id = ? WHERE id = ?`, "sess-inv3", inv.ID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	seedEvent(t, s, "sess-inv3", "assistant_message", "noted, thanks", now-300000, "opencode")

	res, err := s.sweepAt(ctx, now, DefaultLease, StrandedGrace)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, u := range res.Unreported {
		if u.MemberID == inv.ID {
			t.Fatalf("a member holding no step brief must not be chased: %+v", u)
		}
	}
}
