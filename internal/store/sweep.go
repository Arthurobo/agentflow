// sweep.go — the periodic answer to "is anyone reading this loop's mail".
//
// Nothing reaped. RequeueExpiredMail had no caller outside its own definition,
// so leases were decorative and rows delivered twelve hours ago were still
// held. And nothing noticed that a member had stopped polling, so the
// orchestrator's "waiting for their reply" was a hope rather than a fact.

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// StrandedMember is one transition the sweep recorded. The caller may act on
// it (a nudge), but the system is correct if it does nothing at all.
type StrandedMember struct {
	LoopID   string
	MemberID string
	Role     string
	RunID    string
	Tool     string
	// OldestPendingAt is when the unread message arrived, for the message the
	// orchestrator is told.
	OldestPendingAt int64
}

// UnreportedMember is a member that has done the work and not sent it: it is
// holding a brief, it produced an answer after that brief arrived, and it has
// gone quiet without posting.
//
// The Report travels with it because the whole point is that the caller can
// deliver the work itself if the member will not. The system is NOT correct
// if the caller does nothing: this is the one sweep signal that is a
// guarantee rather than an accelerator.
type UnreportedMember struct {
	LoopID   string
	MemberID string
	Role     string
	RunID    string
	Tool     string
	// Report is what the member produced and did not send.
	Report string
	// BriefAt identifies WHICH brief is owed, so a caller can act once per
	// brief rather than once per tick.
	BriefAt int64
}

// SweepResult is what one tick did.
type SweepResult struct {
	Requeued   int64
	Stranded   []StrandedMember
	Unreported []UnreportedMember
}

// Sweep expires leases and records stranded transitions across every open
// loop. Idempotent: a member already marked stranded is not re-marked and its
// orchestrator is not told twice.
func (s *Store) Sweep(ctx context.Context) (SweepResult, error) {
	return s.sweepAt(ctx, time.Now().UnixMilli(), DefaultLease, StrandedGrace)
}

func (s *Store) sweepAt(ctx context.Context, now int64, lease, grace time.Duration) (SweepResult, error) {
	var out SweepResult

	// Leases first. A member that died holding a brief has its brief returned
	// to the queue here, which is the long stop the redelivery window is not.
	n, err := s.RequeueExpiredMail(ctx, lease)
	if err != nil {
		return out, err
	}
	out.Requeued = n

	loops, err := s.ListLoops(ctx, LoopActive, 0)
	if err != nil {
		return out, err
	}
	for _, l := range loops {
		health, err := s.loopHealthAt(ctx, l.ID, now, lease, grace)
		if err != nil {
			return out, err
		}
		members, err := s.ListLoopMembers(ctx, l.ID)
		if err != nil {
			return out, err
		}
		byID := map[string]*LoopMember{}
		for _, m := range members {
			byID[m.ID] = m
		}
		for _, h := range health {
			// A member whose process ended BADLY says so on its row. Only
			// badly: a clean stop or finish is recorded by runState alone,
			// which already stops the member claiming to be alive without
			// claiming it broke.
			if h.AbnormalExit() {
				m := byID[h.MemberID]
				if m != nil && m.LastError == "" {
					if err := s.MarkLoopMemberFailed(ctx, l.ID, m.Role, h.ExitReason()); err != nil {
						return out, err
					}
				}
			}
			if h.State == MemberFinishedUnreported {
				m := byID[h.MemberID]
				u := UnreportedMember{
					LoopID: l.ID, MemberID: h.MemberID, Role: h.Role,
					Report: h.Report, BriefAt: h.OldestBriefAt,
				}
				if m != nil {
					u.RunID, u.Tool = m.RunID, m.Tool
				}
				out.Unreported = append(out.Unreported, u)
				continue
			}
			if h.State != MemberStranded {
				continue
			}
			first, err := s.MarkStranded(ctx, h.MemberID, now)
			if err != nil {
				return out, err
			}
			if !first {
				continue // already recorded; saying it again is noise
			}
			m := byID[h.MemberID]
			st := StrandedMember{
				LoopID: l.ID, MemberID: h.MemberID, Role: h.Role,
				OldestPendingAt: h.OldestPendingAt,
			}
			if m != nil {
				st.RunID, st.Tool = m.RunID, m.Tool
			}
			out.Stranded = append(out.Stranded, st)
			if err := s.announceStranded(ctx, l, h, now); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// announceStranded tells the orchestrator, and the board either way.
//
// Telling the orchestrator is the point: it ends "waiting for their reply"
// forever with a fact. ENGINE mail is outside step enforcement, exactly like
// the entry brief, so this cannot disturb the play. When the STRANDED member
// is the orchestrator there is nobody to tell, so only the event is written
// and the engineer reads it on the board.
func (s *Store) announceStranded(ctx context.Context, l *Loop, h MemberHealth, now int64) error {
	waited := "some time"
	if h.OldestPendingAt > 0 {
		waited = fmt.Sprintf("%d minutes", (now-h.OldestPendingAt)/60000)
	}
	notice := h.Role + " has not read its mail for " + waited +
		". It is not listening and is holding nothing, so nothing will move until it does." +
		" Tell the engineer, or retire and respawn the role."

	return s.tx(ctx, func(tx *sql.Tx) error {
		if !isOrchestratorRole(h.Role) {
			orch, err := txMemberByRole(ctx, tx, l.ID, RoleOrchestrator)
			if err != nil {
				return err
			}
			if orch != nil {
				if _, err := tx.ExecContext(ctx, `INSERT INTO loop_messages (`+mailMessageColumns+`)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					newMailID("msg"), l.ID, "", RoleEngine, orch.ID, orch.Role,
					"Stranded member: "+h.Role, notice, MailPending, 0, 0, "",
					now, 0, 0, "", "", "", ""); err != nil {
					return err
				}
			}
		}
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvMemberStranded, LoopID: l.ID, Content: notice,
			Detail: map[string]any{
				"memberId": h.MemberID, "role": h.Role,
				"pending": h.Pending, "oldestPendingAt": h.OldestPendingAt,
				"runState": h.RunState,
			},
		}, now)
	})
}

func txMemberByRole(ctx context.Context, tx *sql.Tx, loopID, role string) (*LoopMember, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+mailMemberColumns+` FROM loop_members
		WHERE loop_id = ? AND role = ? AND status != ?`, loopID, role, LoopMemberDismissed)
	m, err := scanLoopMember(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}
