// health.go — the four states a loop member can be in, computed on read from
// facts the server already has.
//
// The board could only say "running", which it derived from a session id and
// therefore said forever. A member that reported its work and stopped polling,
// a member that never polled at all, and a member mid-way through a twenty
// minute task were all "running".

package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// StrandedGrace is how long mail may sit unread before nobody reading it is
// a fact rather than a coincidence.
//
// It has to clear the whole recovery path: the gap between polls, the wait
// loop's retry after a connection error, and an agentd restart during which
// the registry is empty and every live member re-polls. 90 seconds covers all
// three with room, and a member that is genuinely working is never judged by
// it, because holding a brief makes it working rather than stranded.
const StrandedGrace = 90 * time.Second

// Member states. A member is in exactly one of these.
const (
	// MemberListening: a long poll for it is open right now.
	MemberListening = "listening"
	// MemberWorking: not polling, but holding a brief it has not discharged.
	// This is the twenty-minute task, and it is not a problem.
	MemberWorking = "working"
	// MemberOverdue: working, but past the lease with no extend. Amber, never
	// red: it may still be thinking.
	MemberOverdue = "overdue"
	// MemberStranded: holding nothing, not polling, with mail waiting. Or its
	// process is gone. Nobody will read what was sent to it.
	MemberStranded = "stranded"
	// MemberStarting: this host is spawning it. A process in flight is not a
	// mailbox question, and answering it here keeps one source of truth for
	// what a member is doing rather than two that can disagree.
	MemberStarting = "starting"
	// MemberExternal: no process here at all. It joined by token from
	// somewhere else, so nothing can be inferred from a run row.
	MemberExternal = "external"
	// MemberFinishedUnreported: holding a brief, produced an answer since it
	// arrived, and has gone quiet without posting. The work exists and the
	// orchestrator cannot see it.
	//
	// This is the gap the other states could not name. Stranded asks "is
	// anyone READING its mail" and this member read its mail. Working is
	// true of a member twenty minutes into a task and of one that finished
	// half an hour ago and said nothing, and the difference between those
	// two is the whole loop.
	MemberFinishedUnreported = "finished-unreported"
)

// QuietFor is how long a member must emit nothing before its turn is
// considered over.
//
// A member mid-turn is not silent: it emits tool calls and their results
// continuously, seconds apart. Silence is therefore the engine-agnostic
// evidence that a turn ended, and it needs no TUI introspection, which is
// the only other way to ask and is a screen-scrape.
const QuietFor = 60 * time.Second

// MemberHealth is one member's answer to "is anyone reading its mail".
type MemberHealth struct {
	MemberID string `json:"memberId"`
	Role     string `json:"role"`
	State    string `json:"state"`
	// Listening and ListeningSince come from the in-process registry.
	Listening      bool  `json:"listening"`
	ListeningSince int64 `json:"listeningSince,omitempty"`
	// Held is undischarged delivered mail: what it is working on.
	Held int `json:"held"`
	// HeldBriefs is how much of that is a step brief the engine sent, and
	// OldestBriefAt is when the oldest of those was delivered. Only a
	// member holding a brief can owe the play a report; a member holding a
	// note from the engineer or a stranded notice owes nobody anything.
	HeldBriefs    int   `json:"heldBriefs,omitempty"`
	OldestBriefAt int64 `json:"-"`
	// OldestHeldAt is when the oldest of those was delivered, for the lease.
	OldestHeldAt int64 `json:"oldestHeldAt,omitempty"`
	// OldestPendingAt is when the oldest message NOBODY has claimed arrived.
	OldestPendingAt int64 `json:"oldestPendingAt,omitempty"`
	Pending         int   `json:"pending"`
	StrandedSince   int64 `json:"strandedSince,omitempty"`
	LastSeenAt      int64 `json:"lastSeenAt,omitempty"`
	// LastPolledAt is the last read of THIS mailbox, which is the only kind
	// of contact that answers "is anyone reading it".
	LastPolledAt int64 `json:"lastPolledAt,omitempty"`
	// Report is the answer this member produced since its brief arrived, and
	// has not sent. Empty unless the member owes a report.
	Report string `json:"-"`
	// ReportAt is when that answer landed; LastEventAt is when the member
	// last did anything at all, which is how its turn is judged over.
	ReportAt    int64 `json:"reportAt,omitempty"`
	LastEventAt int64 `json:"lastEventAt,omitempty"`
	// SessionID is where the transcript for this member lives.
	SessionID string `json:"-"`
	// RunID and RunState link to the process, empty when it runs elsewhere.
	RunID    string `json:"runId,omitempty"`
	RunState string `json:"runState,omitempty"`
	// RunExitCode and RunError are how it ended, read only for a terminal run.
	RunExitCode int    `json:"runExitCode,omitempty"`
	RunError    string `json:"runError,omitempty"`
}

// AbnormalExit reports whether this member's process ended badly.
//
// The distinction matters because last_error is not a neutral field: the board
// checks it before any other verdict and paints the row red. A retired member,
// one the engineer stopped himself, and one that finished its work all leave a
// terminal run behind, and red that appears when nothing is wrong is red he
// stops reading.
func (h MemberHealth) AbnormalExit() bool {
	if h.RunState == "" || !terminalRunState(h.RunState) {
		return false
	}
	return h.RunState == "crashed" || h.RunExitCode != 0
}

// ExitReason is what to record about an abnormal exit.
func (h MemberHealth) ExitReason() string {
	if h.RunError != "" {
		return h.RunError
	}
	if h.RunExitCode != 0 {
		return fmt.Sprintf("its process exited with code %d", h.RunExitCode)
	}
	return "its process " + h.RunState
}

// FinishedUnreported reports whether this member owes the orchestrator work
// it has already done.
//
// Three facts, all of which must hold:
//
//  1. It produced a real answer AFTER the brief arrived. An assistant
//     message from before the brief is the previous task's, and forwarding
//     that would answer a question nobody asked.
//  2. It has gone quiet since. A member mid-turn emits tool calls and
//     results seconds apart, so silence for QuietFor is the turn ending.
//     Judged on the newest event of ANY kind, not on the answer itself:
//     a member that wrote a summary and then kept working is still working.
//  3. It has not posted. Held mail with the caller's own last_posted_at
//     already behind it is what "owes" means, and it is the check that
//     makes this stop firing the moment a report is sent by any route.
func (h MemberHealth) FinishedUnreported(now int64) bool {
	if h.HeldBriefs == 0 || strings.TrimSpace(h.Report) == "" {
		return false
	}
	if h.ReportAt <= 0 || h.ReportAt < h.OldestBriefAt {
		return false
	}
	quiet := h.LastEventAt
	if quiet < h.ReportAt {
		quiet = h.ReportAt
	}
	return now-quiet >= QuietFor.Milliseconds()
}

// LoopHealth computes health for every member of one loop.
func (s *Store) LoopHealth(ctx context.Context, loopID string) ([]MemberHealth, error) {
	return s.loopHealthAt(ctx, loopID, time.Now().UnixMilli(), DefaultLease, StrandedGrace)
}

// loopHealthAt is the testable form: the clock and both windows are arguments,
// so a test states the situation instead of sleeping through it.
func (s *Store) loopHealthAt(
	ctx context.Context, loopID string, now int64, lease, grace time.Duration,
) ([]MemberHealth, error) {
	members, err := s.ListLoopMembers(ctx, loopID)
	if err != nil {
		return nil, err
	}
	listening := s.ListeningNow()
	out := make([]MemberHealth, 0, len(members))
	for _, m := range members {

		if m.Role == RoleEngineer {
			continue // a human, not a poller
		}
		h := MemberHealth{
			MemberID: m.ID, Role: m.Role, StrandedSince: m.StrandedSince,
			LastSeenAt: m.LastSeenAt, LastPolledAt: m.LastPolledAt, RunID: m.RunID,
		}
		h.ListeningSince, h.Listening = listening[m.ID]

		row := s.db.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(MIN(CASE WHEN status = ? THEN delivered_at END), 0),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(MIN(CASE WHEN status = ? THEN created_at END), 0),
			COALESCE(SUM(CASE WHEN status = ? AND step_id != '' AND sender_role = ? THEN 1 ELSE 0 END), 0),
			COALESCE(MIN(CASE WHEN status = ? AND step_id != '' AND sender_role = ? THEN delivered_at END), 0)
			FROM loop_messages WHERE recipient_id = ?`,
			MailDelivered, MailDelivered, MailPending, MailPending,
			MailDelivered, RoleEngine, MailDelivered, RoleEngine, m.ID)
		if err := row.Scan(&h.Held, &h.OldestHeldAt, &h.Pending, &h.OldestPendingAt,
			&h.HeldBriefs, &h.OldestBriefAt); err != nil {
			return nil, err
		}
		if m.RunID != "" {
			err := s.db.QueryRowContext(ctx,
				`SELECT state, exit_code, last_error FROM managed_sessions WHERE id = ?`,
				m.RunID).Scan(&h.RunState, &h.RunExitCode, &h.RunError)
			if err != nil {
				h.RunState, h.RunExitCode, h.RunError = "", 0, ""
			}
		}
		h.SessionID = m.SessionID
		// Only for a member that could owe a report: it is holding a brief
		// it has not answered, it is not the orchestrator (which reports to
		// the human, not to itself), and there is a transcript to read. One
		// extra pair of indexed reads on the handful of rows that qualify,
		// rather than on every member of every loop.
		if h.HeldBriefs > 0 && h.OldestBriefAt > m.LastPostedAt &&
			!isOrchestratorRole(m.Role) && m.SessionID != "" {
			out, err := s.LastReportSince(ctx, m.SessionID, h.OldestBriefAt)
			if err != nil {
				return nil, err
			}
			h.Report, h.ReportAt, h.LastEventAt = out.Report, out.ReportAt, out.LastEventAt
		}
		h.State = memberStateFrom(h, now, lease, grace)
		// Process questions, answered only when the mailbox has nothing to
		// say. A member holding a brief is working wherever it runs, and an
		// external member CAN be stranded, which is exactly the case a nudge
		// cannot reach; neither may be overwritten by where the process is.
		if h.State == MemberWorking && h.Held == 0 {
			switch {
			case m.RunID != "" && m.SessionID == "":
				h.State = MemberStarting
			case m.RunID == "" && m.SessionID == "":
				h.State = MemberExternal
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// memberStateFrom is the whole decision, kept in one function so the order of
// the questions is visible.
//
// The order matters: listening beats everything because a held line is direct
// evidence; holding a brief beats stranded because that is what a member
// working for twenty minutes looks like and calling it stranded is the way
// this detector would lose its credibility.
func memberStateFrom(h MemberHealth, now int64, lease, grace time.Duration) string {
	if h.Listening {
		return MemberListening
	}
	// A dead process cannot read anything, whatever the queue says.
	if h.RunState != "" && terminalRunState(h.RunState) {
		return MemberStranded
	}
	if h.Held > 0 {
		// Finished but silent, checked BEFORE overdue: a member that
		// produced its answer an hour ago and never sent it is not late,
		// it is done and unheard, and saying "overdue" of it would send
		// the reader looking for a member that is still thinking.
		if h.FinishedUnreported(now) {
			return MemberFinishedUnreported
		}
		if h.OldestHeldAt > 0 && now-h.OldestHeldAt > lease.Milliseconds() {
			return MemberOverdue
		}
		return MemberWorking
	}
	// Holding nothing and not polling. Only a fact once mail has actually been
	// waiting, and once the member has not READ its inbox for the grace.
	//
	// last_polled_at, not last_seen_at. last_seen_at is stamped by every
	// authenticated call, so a member that reported, was correctly emptied by
	// the discharge, and then kept narrating its own work with a note every
	// thirty seconds would look healthy forever while a brief sat unread.
	// That is defect 14 surviving inside its own detector. Only a poll
	// answers the question this state is asking.
	if h.OldestPendingAt > 0 && now-h.OldestPendingAt > grace.Milliseconds() &&
		now-h.LastPolledAt > grace.Milliseconds() {
		return MemberStranded
	}
	return MemberWorking
}

func terminalRunState(state string) bool {
	switch state {
	case "finished", "stopped", "crashed":
		return true
	}
	return false
}

// MarkStranded records the transition once. It reports whether it was this
// call that recorded it, so the sweep notifies exactly once.
func (s *Store) MarkStranded(ctx context.Context, memberID string, at int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET stranded_since = ? WHERE id = ? AND stranded_since = 0`,
		at, memberID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClearStranded is the member answering for itself.
func (s *Store) ClearStranded(ctx context.Context, memberID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET stranded_since = 0 WHERE id = ? AND stranded_since != 0`, memberID)
	return err
}

// MarkPolled records that this member read its own inbox.
func (s *Store) MarkPolled(ctx context.Context, memberID string, at int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET last_polled_at = ? WHERE id = ?`, at, memberID)
	return err
}
