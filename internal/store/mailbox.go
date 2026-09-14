// mailbox.go — delivery for the engine-owned loop mailbox (migration 0016).
// Members pull with an atomic claim and acknowledge explicitly; nothing is
// inferred from a transcript and no body is ever truncated.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Mail message statuses.
const (
	MailPending   = "pending"
	MailDelivered = "delivered"
	MailAcked     = "acked"
	MailCancelled = "cancelled"
	// MailMirrored is visible but never deliverable. A mirror exists so the
	// orchestrator can SEE what passed between the engineer and a worker;
	// waking it to read a copy it need not act on would put the loop's
	// largest context cost on every progress note, which is the exact cost
	// this design exists to avoid. ClaimInbox never selects this status.
	MailMirrored = "mirrored"
)

// DefaultLease is how long a claimed message may go unacked before it returns
// to the queue.
const DefaultLease = 30 * time.Minute

// DefaultRedeliverAfter is the grace before a held, unacked message is handed
// back to a member that polls again. An agent that is genuinely working is not
// polling, so a poll inside this window means the delivery was lost.
const DefaultRedeliverAfter = 60 * time.Second

// Delivery errors, mapped to status codes by the transport.
var (
	ErrMailNotFound      = errors.New("store: no such mail message")
	ErrMailRouteRefused  = errors.New("store: route refused")
	ErrLoopEnded         = errors.New("store: loop has ended")
	ErrMailSenderRetired = errors.New("store: sender was dismissed")
)

// LoopMessage is one envelope. Body is always whole.
type LoopMessage struct {
	ID            string `json:"id"`
	LoopID        string `json:"loopId"`
	SenderID      string `json:"senderId,omitempty"`
	SenderRole    string `json:"senderRole,omitempty"`
	RecipientID   string `json:"recipientId,omitempty"`
	RecipientRole string `json:"recipientRole,omitempty"`
	Subject       string `json:"subject,omitempty"`
	Body          string `json:"body"`
	BodyChars     int    `json:"bodyChars"`
	Status        string `json:"status"`
	DeliveryCount int    `json:"deliveryCount"`
	LeaseSeconds  int    `json:"leaseSeconds,omitempty"`
	ForwardedFrom string `json:"forwardedFrom,omitempty"`
	MirrorOf      string `json:"mirrorOf,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	StepID        string `json:"stepId,omitempty"`
	Redelivered   bool   `json:"redelivered,omitempty"`
	// IdempotencyKey is the sender's own retry key. Never rendered; it exists
	// so a post whose connection died is not written twice.
	IdempotencyKey string `json:"-"`
	CreatedAt      int64  `json:"createdAt,omitempty"`
	DeliveredAt    int64  `json:"deliveredAt,omitempty"`
	AckedAt        int64  `json:"ackedAt,omitempty"`
}

// MailArchiveEntry is a pointer to a message: who sent it and how big it is,
// with no body, so choosing something to forward costs almost nothing.
type MailArchiveEntry struct {
	ID            string `json:"id"`
	LoopID        string `json:"loopId"`
	SenderRole    string `json:"senderRole,omitempty"`
	RecipientRole string `json:"recipientRole,omitempty"`
	Subject       string `json:"subject,omitempty"`
	BodyChars     int    `json:"bodyChars"`
	Status        string `json:"status"`
	CreatedAt     int64  `json:"createdAt,omitempty"`
}

// MailPost is the content of one send. Outcome is the sender's verdict and is
// what selects a guarded edge when the loop is running a play.
type MailPost struct {
	Subject      string
	Body         string
	LeaseSeconds int
	Outcome      string
	// IdempotencyKey, when set, makes the post replayable: a retry after a
	// connection died mid-call returns the original row instead of writing a
	// second copy of a report.
	IdempotencyKey string
}

// MailForward names an earlier message to copy verbatim, with the envelope the
// copy travels under.
type MailForward struct {
	OriginalID   string
	Subject      string
	LeaseSeconds int
	Outcome      string
}

// ClaimOptions tunes one inbox poll.
type ClaimOptions struct {
	Limit   int
	AutoAck bool
	// RedeliverAfter overrides the lost-delivery grace; nil means the
	// default. A zero duration makes every held message eligible at once,
	// which is how the lost-delivery path is tested.
	RedeliverAfter *time.Duration
	// Push marks a claim made by a deliverer that writes the bodies into the
	// member's terminal rather than handing them to the member's own poll.
	// Only pending mail is taken: held mail is what the member is already
	// working on, and pasting it again every minute would be a second copy
	// of the same brief in the same prompt box. The delivery counters are
	// left alone until MarkDelivered confirms the write happened.
	Push bool
}

const mailMessageColumns = `id, loop_id, sender_id, sender_role, recipient_id, recipient_role,
	subject, body, status, delivery_count, lease_seconds, forwarded_from,
	created_at, delivered_at, acked_at, step_id, outcome, mirror_of, idempotency_key`

// MailRouteAllowed enforces the routing policy. A worker may address the
// orchestrator, and it may narrate to the engineer, who is a participant
// rather than a side channel. It may never address another worker. Every
// engineer-to-worker and worker-to-engineer message is mirrored to the
// orchestrator (see mirrorToOrchestrator), so reaching the engineer directly
// costs the orchestrator no sight of its own loop.
func MailRouteAllowed(senderRole, recipientRole string) error {
	sender := BaseRole(senderRole)
	recipient := BaseRole(recipientRole)
	if sender == RoleOrchestrator || sender == RoleEngineer ||
		recipient == RoleOrchestrator || recipient == RoleEngineer {
		return nil
	}
	return fmt.Errorf("%w: %s may message %s or %s and nobody else; the orchestrator routes the rest",
		ErrMailRouteRefused, sender, RoleOrchestrator, RoleEngineer)
}

// needsMirror reports whether this message goes past the orchestrator and so
// must be copied to it.
func needsMirror(senderRole, recipientRole string) bool {
	sender, recipient := BaseRole(senderRole), BaseRole(recipientRole)
	if sender == RoleOrchestrator || recipient == RoleOrchestrator {
		return false
	}
	return sender == RoleEngineer || recipient == RoleEngineer
}

// mirrorToOrchestrator copies a message that travelled between the engineer
// and a worker into the orchestrator's inbox, marked as a mirror so it is not
// mistaken for a report. Returns the orchestrator's member id when one was
// written.
func mirrorToOrchestrator(ctx context.Context, tx *sql.Tx, m *LoopMessage, at int64) (string, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, role FROM loop_members
		WHERE loop_id = ? AND role = ? AND status != ?`,
		m.LoopID, RoleOrchestrator, LoopMemberDismissed)
	var id, role string
	if err := row.Scan(&id, &role); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	subject := "mirror: " + m.SenderRole + " to " + m.RecipientRole
	if m.Subject != "" {
		subject += " (" + m.Subject + ")"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO loop_messages (`+mailMessageColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newMailID("msg"), m.LoopID, m.SenderID, m.SenderRole, id, role,
		subject, m.Body, MailMirrored, 0, 0, "", at, 0, 0, "", "", m.ID, "")
	return id, err
}

// PostMail records one message for the recipient to pull.
func (s *Store) PostMail(ctx context.Context, sender, recipient *LoopMember, p MailPost) (*LoopMessage, error) {
	return s.postMail(ctx, sender, recipient, p, "")
}

// ForwardMail copies an earlier message body server side, byte for byte, so a
// report reaches its next reader without anyone retyping or even reading it.
// The original is looked up across the sender's loop lineage.
func (s *Store) ForwardMail(ctx context.Context, sender, recipient *LoopMember, f MailForward) (*LoopMessage, error) {
	lineage, err := s.LoopLineage(ctx, sender.LoopID)
	if err != nil {
		return nil, err
	}
	original, err := s.FindLoopMessage(ctx, lineage, f.OriginalID)
	if err != nil {
		return nil, err
	}
	if original == nil {
		return nil, fmt.Errorf("%w: %s is not in this loop or the loops it came from", ErrMailNotFound, f.OriginalID)
	}
	return s.postMail(ctx, sender, recipient, MailPost{
		Subject: f.Subject, Body: original.Body, LeaseSeconds: f.LeaseSeconds, Outcome: f.Outcome,
	}, original.ID)
}

func (s *Store) postMail(ctx context.Context, sender, recipient *LoopMember, p MailPost, forwardedFrom string) (*LoopMessage, error) {
	if sender == nil || recipient == nil {
		return nil, ErrMailNotFound
	}
	if sender.ID == recipient.ID {
		return nil, fmt.Errorf("%w: cannot message yourself", ErrMailRouteRefused)
	}
	if sender.Status == LoopMemberDismissed {
		return nil, ErrMailSenderRetired
	}
	// A retry is not a second message. The report is committed before the
	// caller re-enters the queue, so a connection that dies after the write
	// looks identical to one that died before it; the key is how the client
	// tells us which happened.
	if p.IdempotencyKey != "" {
		if prior, err := s.mailByIdempotencyKey(ctx, sender.ID, p.IdempotencyKey); err != nil {
			return nil, err
		} else if prior != nil {
			return prior, nil
		}
	}
	if err := MailRouteAllowed(sender.Role, recipient.Role); err != nil {
		_ = s.RecordRefusal(ctx, LoopEvent{
			Kind: EvRouteRefused, LoopID: sender.LoopID,
			Content: sender.Role + " to " + recipient.Role + " refused",
			Detail:  map[string]any{"fromRole": sender.Role, "toRole": recipient.Role, "reason": err.Error()},
		})
		return nil, err
	}
	m := &LoopMessage{
		ID:             newMailID("msg"),
		LoopID:         sender.LoopID,
		SenderID:       sender.ID,
		SenderRole:     sender.Role,
		RecipientID:    recipient.ID,
		RecipientRole:  recipient.Role,
		Subject:        p.Subject,
		Body:           p.Body,
		BodyChars:      utf8.RuneCountInString(p.Body),
		Status:         MailPending,
		LeaseSeconds:   p.LeaseSeconds,
		ForwardedFrom:  forwardedFrom,
		Outcome:        p.Outcome,
		IdempotencyKey: p.IdempotencyKey,
		CreatedAt:      time.Now().UnixMilli(),
	}
	var capErr error
	woken := []string{recipient.ID}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// Write FIRST, on the row this post is about. That takes the write
		// lock before anything is read, so concurrent posts to one loop
		// serialize instead of both reading a step, both deciding they
		// drain it, and one arrival being lost. A transaction that reads
		// before it writes cannot even do that safely here: under WAL the
		// second reader deadlocks upgrading its snapshot.
		if _, err := tx.ExecContext(ctx,
			`UPDATE loops SET updated_at = ? WHERE id = ?`, m.CreatedAt, sender.LoopID); err != nil {
			return err
		}
		loop, err := scanLoop(tx.QueryRowContext(ctx,
			`SELECT `+mailLoopColumns+` FROM loops WHERE id = ?`, sender.LoopID))
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: %s", ErrMailNotFound, sender.LoopID)
		}
		if err != nil {
			return err
		}
		if loop.Ended() {
			return fmt.Errorf("%w: %s is %s. Stop polling and exit", ErrLoopEnded, loop.ID, loop.Status)
		}
		var entries []rosterEntry
		if loop.PlayInProgress() {
			if entries, err = txRosterEntries(ctx, tx, loop.ID); err != nil {
				return err
			}
		}
		move, err := enforceStep(loop, rosterRoles(entries), sender.Role, recipient.Role, p.Outcome)
		if err != nil {
			// A step violation is an event the UI renders as a card, so it
			// has to be committed. Rolling the whole transaction back would
			// discard the only record that enforcement did anything.
			var violation *StepViolation
			if errors.As(err, &violation) {
				capErr = err
				return recordRefusalTx(ctx, tx, LoopEvent{
					Kind: EvStepRefused, LoopID: loop.ID,
					Content: violation.Error(), Detail: violation,
				}, m.CreatedAt)
			}
			return err
		}
		if move != nil && move.Refuse != nil {
			capErr = move.Refuse
			if _, err := tx.ExecContext(ctx, `UPDATE loops
				SET step_id = ?, step_awaiting = ?, play_status = ?, play_ended_at = ? WHERE id = ?`,
				move.StepID, encodeAwaiting(move.Awaiting), move.Status,
				playEndedAt(move.Status, loop.PlayEndedAt, m.CreatedAt), loop.ID); err != nil {
				return err
			}
			return recordRefusalTx(ctx, tx, LoopEvent{
				Kind: EvRoundCapped, LoopID: loop.ID,
				Content: move.Refuse.Error(), Detail: move.Refuse,
			}, m.CreatedAt)
		}
		if move != nil {
			m.StepID = loop.StepID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO loop_messages (`+mailMessageColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.LoopID, m.SenderID, m.SenderRole, m.RecipientID, m.RecipientRole,
			m.Subject, m.Body, m.Status, 0, m.LeaseSeconds, m.ForwardedFrom,
			m.CreatedAt, 0, 0, m.StepID, m.Outcome, "", m.IdempotencyKey); err != nil {
			return err
		}
		if needsMirror(sender.Role, recipient.Role) {
			// Deliberately NOT added to woken: a mirror is for sight, not
			// for action, and waking the orchestrator is the cost this
			// avoids.
			if _, err := mirrorToOrchestrator(ctx, tx, m, m.CreatedAt); err != nil {
				return err
			}
		}
		// Completion is the ENGINE's fact, not the agent's call.
		//
		// Acking was a voluntary second step the models skipped: both
		// investigators reported their work and neither acked, so their
		// briefs stayed delivered and came back on every poll, forty-one
		// times in one case. But a post that discharges the current step is
		// proof the sender read its brief and acted on it, which is a
		// stronger fact than the ack it forgot. So the engine acks for it.
		//
		// Scoped to deliveries made BEFORE this post: a brief that arrived
		// while the report was in flight has not been read and must not be
		// swept up as though it had.
		if move != nil {
			res, err := tx.ExecContext(ctx, `UPDATE loop_messages
				SET status = ?, acked_at = ?
				WHERE recipient_id = ? AND status = ? AND delivered_at <= ?`,
				MailAcked, m.CreatedAt, sender.ID, MailDelivered, m.CreatedAt)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				if err := enqueueLoopEventTx(ctx, tx, LoopEvent{
					Kind: EvMailAcked, LoopID: m.LoopID,
					Content: sender.Role + " discharged " + strconv.FormatInt(n, 10) + " held message(s)",
					Detail: map[string]any{
						"memberId": sender.ID, "role": sender.Role,
						"count": n, "reason": "discharged", "stepId": m.StepID,
					},
				}, m.CreatedAt); err != nil {
					return err
				}
			}
		}
		// The sender demonstrably acted, whatever the step says. Recorded for
		// every post, because the redelivery window reads it.
		if _, err := tx.ExecContext(ctx,
			`UPDATE loop_members SET last_posted_at = ? WHERE id = ?`,
			m.CreatedAt, sender.ID); err != nil {
			return err
		}
		if err := enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvMailPosted, LoopID: m.LoopID,
			Content: m.SenderRole + " to " + m.RecipientRole,
			Detail: map[string]any{
				"messageId": m.ID, "fromRole": m.SenderRole, "toRole": m.RecipientRole,
				"subject": m.Subject, "bodyChars": m.BodyChars, "outcome": m.Outcome,
				"stepId": m.StepID, "forwardedFrom": m.ForwardedFrom,
			},
		}, m.CreatedAt); err != nil {
			return err
		}
		if move == nil {
			return nil
		}
		entered := loop.StepEnteredAt
		if move.Entered {
			entered = m.CreatedAt
		}
		if _, err := tx.ExecContext(ctx, `UPDATE loops
			SET step_id = ?, step_awaiting = ?, play_status = ?, step_entered_at = ?, round = round + ?,
			    play_ended_at = ?
			WHERE id = ?`,
			move.StepID, encodeAwaiting(move.Awaiting), move.Status, entered, move.RoundInc,
			playEndedAt(move.Status, loop.PlayEndedAt, m.CreatedAt), loop.ID); err != nil {
			return err
		}
		if !move.Entered {
			return nil
		}
		play, ok := LookupPlay(loop.Play)
		if !ok {
			return nil
		}
		briefed, err := queueStepBrief(ctx, tx, loop.ID, play, play.Step(move.StepID), entries, m.CreatedAt)
		if err != nil {
			return err
		}
		woken = append(woken, briefed...)
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvStepAdvanced, LoopID: loop.ID,
			Content: loop.Play + " advanced to " + move.StepID,
			Detail: map[string]any{
				"play": loop.Play, "fromStep": loop.StepID, "toStep": move.StepID,
				"awaiting": move.Awaiting, "playStatus": move.Status, "roundInc": move.RoundInc,
			},
		}, m.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	if capErr != nil {
		return nil, capErr
	}
	s.notifyMail(woken...)
	return m, nil
}

// playEndedAt is the play_ended_at a move leaves behind: stamped the first
// time the play stops running, and kept after that.
func playEndedAt(status string, current, now int64) int64 {
	if status == PlayRunning {
		return 0
	}
	if current > 0 {
		return current
	}
	return now
}

// HasClaimableMail is the cheap read a long poll does before reaching for the
// claim, so a parked poller costs one EXISTS rather than a write transaction
// every tick.
func (s *Store) HasClaimableMail(ctx context.Context, member *LoopMember, redeliverAfter *time.Duration) (bool, error) {
	if member == nil || member.Status == LoopMemberDismissed {
		return false, nil
	}
	grace := DefaultRedeliverAfter
	if redeliverAfter != nil {
		grace = *redeliverAfter
	}
	cutoff := time.Now().UnixMilli() - grace.Milliseconds()
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM loop_messages WHERE recipient_id = ?
		 AND (status = ?
		   OR (status = ? AND delivered_at <= ?
		       AND delivered_at > (SELECT last_posted_at FROM loop_members WHERE id = ?))))`,
		member.ID, MailPending, MailDelivered, cutoff, member.ID).Scan(&found)
	return found == 1, err
}

// FindQuotedMail reports whether a body contains an earlier message from the
// lineage in full. Retyping a report is how evidence gets lost, so the sender
// is handed the id to forward instead.
func (s *Store) FindQuotedMail(ctx context.Context, loopIDs []string, body string, minChars int) (*MailArchiveEntry, error) {
	if len(loopIDs) == 0 || minChars <= 0 || utf8.RuneCountInString(body) <= minChars {
		return nil, nil
	}
	args := []any{}
	for _, id := range loopIDs {
		args = append(args, id)
	}
	args = append(args, minChars)
	//nolint:gosec // G202: the concatenated fragment is a generated "?,?" list, never input
	rows, err := s.db.QueryContext(ctx, `SELECT id, loop_id, sender_role, recipient_role,
		subject, length(body), status, created_at, body FROM loop_messages
		WHERE loop_id IN (`+placeholders(len(loopIDs))+`) AND length(body) >= ?
		ORDER BY length(body) DESC LIMIT 50`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		e := &MailArchiveEntry{}
		var candidate string
		if err := rows.Scan(&e.ID, &e.LoopID, &e.SenderRole, &e.RecipientRole,
			&e.Subject, &e.BodyChars, &e.Status, &e.CreatedAt, &candidate); err != nil {
			return nil, err
		}
		if strings.Contains(body, candidate) {
			return e, nil
		}
	}
	return nil, rows.Err()
}

// ClaimInbox hands the member its waiting messages. The claim is one UPDATE
// stamping a fresh claim id followed by a read back of that id, so two
// concurrent pollers on the same token can never receive the same message.
func (s *Store) ClaimInbox(ctx context.Context, member *LoopMember, opts ClaimOptions) ([]*LoopMessage, error) {
	if member == nil || member.Status == LoopMemberDismissed {
		return []*LoopMessage{}, nil
	}
	limit := opts.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	grace := DefaultRedeliverAfter
	if opts.RedeliverAfter != nil {
		grace = *opts.RedeliverAfter
	}
	now := time.Now().UnixMilli()
	lostCutoff := now - grace.Milliseconds()
	claimID := newMailID("claim")
	status, ackedAt := MailDelivered, int64(0)
	if opts.AutoAck {
		status, ackedAt = MailAcked, now
	}

	countNow := 1
	if opts.Push {
		// Nothing held is eligible, and nothing is counted until the write
		// is confirmed.
		lostCutoff, countNow = 0, 0
	}

	out := []*LoopMessage{}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE loop_messages
			SET status = ?, delivered_at = ?, acked_at = ?, claim_id = ?,
			    delivery_count = delivery_count + ?
			WHERE id IN (
				SELECT id FROM loop_messages
				 WHERE recipient_id = ?
				   AND (status = ?
				     OR (status = ? AND delivered_at > 0 AND delivered_at <= ?
				         AND delivered_at > (SELECT last_posted_at FROM loop_members WHERE id = ?)))
				 ORDER BY seq LIMIT ?)`,
			status, now, ackedAt, claimID, countNow,
			member.ID, MailPending, MailDelivered, lostCutoff, member.ID, limit); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+mailMessageColumns+`
			FROM loop_messages WHERE claim_id = ? ORDER BY seq`, claimID)
		if err != nil {
			return err
		}
		for rows.Next() {
			m, err := scanLoopMessage(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			if opts.Push {
				m.Redelivered = m.DeliveryCount > 0
			} else {
				m.Redelivered = m.DeliveryCount > 1
			}
			out = append(out, m)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		if len(out) == 0 || opts.Push {
			return nil
		}
		chars := 0
		for _, m := range out {
			chars += m.BodyChars
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE loop_members SET chars_in = chars_in + ? WHERE id = ?`, chars, member.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AckMail confirms the recipient finished the work. Reports whether this call
// was the one that acked it.
func (s *Store) AckMail(ctx context.Context, memberID, messageID string) (bool, error) {
	now := time.Now().UnixMilli()
	var acked bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE loop_messages
			SET status = ?, acked_at = ? WHERE id = ? AND recipient_id = ? AND status != ?`,
			MailAcked, now, messageID, memberID, MailAcked)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		acked = true
		var loopID, role string
		if err := tx.QueryRowContext(ctx,
			`SELECT loop_id, recipient_role FROM loop_messages WHERE id = ?`, messageID).
			Scan(&loopID, &role); err != nil {
			return err
		}
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvMailAcked, LoopID: loopID,
			Content: role + " acked its brief",
			Detail:  map[string]any{"messageId": messageID, "role": role},
		}, now)
	})
	return acked, err
}

// MarkDelivered confirms that a push delivery reached the member's terminal.
//
// The counters move here and not at the claim: a write that failed never
// reached anyone, and counting it made a member that had read nothing look
// like it had been handed its brief several times. The lease also starts
// now, from the moment the member could first see the body. A member that
// has just been handed mail is by definition no longer stranded.
func (s *Store) MarkDelivered(ctx context.Context, memberID string, messageIDs []string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) error {
		args := make([]any, 0, len(messageIDs)+3)
		args = append(args, now, memberID, MailDelivered)
		for _, id := range messageIDs {
			args = append(args, id)
		}
		in := placeholders(len(messageIDs))
		//nolint:gosec // G202: placeholders() emits only "?" separators
		if _, err := tx.ExecContext(ctx, `UPDATE loop_messages
			SET delivery_count = delivery_count + 1, delivered_at = ?
			WHERE recipient_id = ? AND status = ? AND id IN (`+in+`)`, args...); err != nil {
			return err
		}
		var chars int64
		//nolint:gosec // G202: same generated placeholder list, never input
		rows, err := tx.QueryContext(ctx, `SELECT body FROM loop_messages
			WHERE recipient_id = ? AND id IN (`+in+`)`, append([]any{memberID}, args[3:]...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				_ = rows.Close()
				return err
			}
			chars += int64(utf8.RuneCountInString(body))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		_, err = tx.ExecContext(ctx,
			`UPDATE loop_members SET chars_in = chars_in + ?, stranded_since = 0 WHERE id = ?`,
			chars, memberID)
		return err
	})
}

// MaxLeaseExtension bounds one extend call. A member can extend again, and a
// long task should; what it cannot do is park a brief for a week with one
// call and hide a dead member behind it.
const MaxLeaseExtension = time.Hour

// ExtendMailLease restarts the lease on a held message for work that runs
// longer than the default.
func (s *Store) ExtendMailLease(ctx context.Context, memberID, messageID string, seconds int) (bool, error) {
	if seconds <= 0 {
		seconds = int(DefaultLease.Seconds())
	}
	if max := int(MaxLeaseExtension.Seconds()); seconds > max {
		seconds = max
	}
	res, err := s.db.ExecContext(ctx, `UPDATE loop_messages
		SET delivered_at = ?, lease_seconds = ?
		WHERE id = ? AND recipient_id = ? AND status = ?`,
		time.Now().UnixMilli(), seconds, messageID, memberID, MailDelivered)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MaxLeaseExpiries is how many times a message's lease may run out before
// agentd stops putting it back on the queue. Each expiry means the member was
// handed the body and did nothing with it for a whole lease; re-sending it a
// fourth time is not going to change that, and it costs a full brief of
// context every time.
const MaxLeaseExpiries = 3

// RequeueExpiredMail returns held messages whose lease has run out to the
// queue. defaultLease is applied to messages that carry no lease of their own
// and is used verbatim, so a zero expires them immediately.
//
// Every expiry is counted on the message. The one that reaches
// MaxLeaseExpiries is not requeued: it is cancelled, the loop records an
// event, and the orchestrator is told, instead of the body going round
// forever with nobody noticing. Returns how many messages went back on the
// queue.
func (s *Store) RequeueExpiredMail(ctx context.Context, defaultLease time.Duration) (int64, error) {
	now := time.Now().UnixMilli()
	var requeued int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// Write first so this transaction holds the write lock before it
		// reads anything; the rows it reads back are exactly the ones it
		// just touched.
		claim := newMailID("expire")
		res, err := tx.ExecContext(ctx, `UPDATE loop_messages
			SET status = ?, delivered_at = 0, claim_id = ?, lease_expiries = lease_expiries + 1
			WHERE status = ? AND delivered_at > 0
			  AND delivered_at + (CASE WHEN lease_seconds > 0 THEN lease_seconds * 1000 ELSE ? END) <= ?`,
			MailPending, claim, MailDelivered, defaultLease.Milliseconds(), now)
		if err != nil {
			return err
		}
		if requeued, err = res.RowsAffected(); err != nil || requeued == 0 {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, loop_id, recipient_role, sender_role, subject, lease_expiries
			FROM loop_messages WHERE claim_id = ? AND lease_expiries >= ?`, claim, MaxLeaseExpiries)
		if err != nil {
			return err
		}
		type expired struct {
			id, loopID, recipient, sender, subject string
			expiries                               int
		}
		var given []expired
		for rows.Next() {
			var e expired
			if err := rows.Scan(&e.id, &e.loopID, &e.recipient, &e.sender, &e.subject, &e.expiries); err != nil {
				_ = rows.Close()
				return err
			}
			given = append(given, e)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		if _, err := tx.ExecContext(ctx,
			`UPDATE loop_messages SET claim_id = '' WHERE claim_id = ?`, claim); err != nil {
			return err
		}
		for _, e := range given {
			if _, err := tx.ExecContext(ctx,
				`UPDATE loop_messages SET status = ? WHERE id = ?`, MailCancelled, e.id); err != nil {
				return err
			}
			requeued--
			notice := fmt.Sprintf("%s was handed message %s (%q from %s) %d times and never finished it, "+
				"so it is no longer being re-sent. Decide what to do with that work: tell the engineer, "+
				"or retire and respawn the role.", e.recipient, e.id, e.subject, e.sender, e.expiries)
			if !isOrchestratorRole(e.recipient) && e.sender != RoleEngine {
				orch, err := txMemberByRole(ctx, tx, e.loopID, RoleOrchestrator)
				if err != nil {
					return err
				}
				if orch != nil {
					if _, err := tx.ExecContext(ctx, `INSERT INTO loop_messages (`+mailMessageColumns+`)
						VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
						newMailID("msg"), e.loopID, "", RoleEngine, orch.ID, orch.Role,
						"Undeliverable: "+e.recipient, notice, MailPending, 0, 0, "",
						now, 0, 0, "", "", "", ""); err != nil {
						return err
					}
				}
			}
			if err := enqueueLoopEventTx(ctx, tx, LoopEvent{
				Kind: EvMailUndeliverable, LoopID: e.loopID, Content: notice,
				Detail: map[string]any{
					"messageId": e.id, "role": e.recipient, "leaseExpiries": e.expiries,
				},
			}, now); err != nil {
				return err
			}
		}
		return nil
	})
	return requeued, err
}

// MembersAwaitingDelivery lists members with mail nobody has taken, in loops
// that are still open, restricted to members this host has a live run for.
//
// It is the courier's safety sweep. The watch channel carries a member id the
// instant mail commits, which is the fast path; this is the slow one, and it
// exists because not every producer notifies. A stranded announcement is
// written straight to the table by the sweep itself and wakes nothing, so
// without this pass the orchestrator's copy of "nobody is reading X's mail"
// would sit pending forever — a notice about unread mail, unread.
//
// A member whose run has finished, stopped or crashed is left out: there is
// no terminal to write into, and trying every few seconds only churns the
// queue. A run with no row yet is still being started and stays in.
func (s *Store) MembersAwaitingDelivery(ctx context.Context, limit int) ([]*LoopMember, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// The only concatenated part is the package's constant column list; every
	// value is a bound parameter.
	//nolint:gosec // G202: the concatenation is a constant column list
	query := `SELECT ` + qualifyColumns("m", mailMemberColumns) + ` FROM loop_members m
		 LEFT JOIN managed_sessions ms ON ms.id = m.run_id
		 WHERE m.run_id != '' AND m.status != ?
		   AND COALESCE(ms.state, '') NOT IN ('finished', 'stopped', 'crashed')
		   AND EXISTS (SELECT 1 FROM loops l WHERE l.id = m.loop_id AND l.status = ?)
		   AND EXISTS (SELECT 1 FROM loop_messages msg
		               WHERE msg.recipient_id = m.id AND msg.status = ?)
		 ORDER BY m.created_at ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query,
		LoopMemberDismissed, LoopActive, MailPending, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*LoopMember{}
	for rows.Next() {
		m, err := scanLoopMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReleaseMail puts claimed messages back on the queue, undoing a claim whose
// delivery did not happen.
//
// The courier claims before it injects, because claiming is what stops two
// deliverers racing for the same body. When the injection then fails — the
// PTY is gone, the member's process died between the claim and the write —
// the message must go back to pending rather than sit "delivered" to nobody.
// Leaving it claimed is how mail gets swallowed: the member never sees it,
// the stranded detector sees a member holding a brief and calls it working,
// and nothing ever moves.
//
// Only messages still in the delivered state are released; one the member has
// already acked is finished and is not disturbed.
func (s *Store) ReleaseMail(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, MailPending, MailDelivered)
	for _, id := range ids {
		args = append(args, id)
	}
	//nolint:gosec // placeholders() emits only "?" separators
	res, err := s.db.ExecContext(ctx, `UPDATE loop_messages
		SET status = ?, delivered_at = 0, claim_id = ''
		WHERE status = ? AND id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetLoopMessage reads one whole message from a loop.
func (s *Store) GetLoopMessage(ctx context.Context, loopID, messageID string) (*LoopMessage, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+mailMessageColumns+`
		FROM loop_messages WHERE id = ? AND loop_id = ?`, messageID, loopID)
	m, err := scanLoopMessage(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// FindLoopMessage reads one whole message from any loop in the given set, so a
// lineage can reach its own history and nothing else.
func (s *Store) FindLoopMessage(ctx context.Context, loopIDs []string, messageID string) (*LoopMessage, error) {
	if len(loopIDs) == 0 {
		return nil, nil
	}
	args := []any{messageID}
	for _, id := range loopIDs {
		args = append(args, id)
	}
	//nolint:gosec // G202: the concatenated fragment is a generated "?,?" list, never input
	row := s.db.QueryRowContext(ctx, `SELECT `+mailMessageColumns+` FROM loop_messages
		WHERE id = ? AND loop_id IN (`+placeholders(len(loopIDs))+`)`, args...)
	m, err := scanLoopMessage(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// MailArchive indexes a lineage's messages without their bodies.
func (s *Store) MailArchive(ctx context.Context, loopIDs []string, limit int) ([]*MailArchiveEntry, error) {
	if len(loopIDs) == 0 {
		return []*MailArchiveEntry{}, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args := []any{}
	for _, id := range loopIDs {
		args = append(args, id)
	}
	args = append(args, limit)
	//nolint:gosec // G202: same generated placeholder list, never input
	rows, err := s.db.QueryContext(ctx, `SELECT id, loop_id, sender_role, recipient_role,
		subject, length(body), status, created_at FROM loop_messages
		WHERE loop_id IN (`+placeholders(len(loopIDs))+`)
		ORDER BY seq LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*MailArchiveEntry{}
	for rows.Next() {
		e := &MailArchiveEntry{}
		if err := rows.Scan(&e.ID, &e.LoopID, &e.SenderRole, &e.RecipientRole,
			&e.Subject, &e.BodyChars, &e.Status, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MailThread returns one member's own message history, oldest-first, so a
// fresh session catches up without replaying a veteran session's context.
func (s *Store) MailThread(ctx context.Context, memberID string, limit int) ([]*LoopMessage, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+mailMessageColumns+` FROM loop_messages
		WHERE sender_id = ? OR recipient_id = ?
		ORDER BY seq DESC LIMIT ?`, memberID, memberID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*LoopMessage{}
	for rows.Next() {
		m, err := scanLoopMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ListLoopMessages returns a loop's whole transcript, oldest-first.
func (s *Store) ListLoopMessages(ctx context.Context, loopID string, limit int) ([]*LoopMessage, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+mailMessageColumns+` FROM loop_messages
		WHERE loop_id = ? ORDER BY seq LIMIT ?`, loopID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*LoopMessage{}
	for rows.Next() {
		m, err := scanLoopMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountMailByStatus summarizes a loop's queue.
func (s *Store) CountMailByStatus(ctx context.Context, loopID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM loop_messages WHERE loop_id = ? GROUP BY status`, loopID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// qualifyColumns prefixes every column in a column list with a table alias,
// for the queries that join a table whose columns would otherwise collide.
func qualifyColumns(alias, columns string) string {
	fields := strings.Split(columns, ",")
	for i, f := range fields {
		fields[i] = alias + "." + strings.TrimSpace(f)
	}
	return strings.Join(fields, ", ")
}

func scanLoopMessage(sc interface{ Scan(dest ...any) error }) (*LoopMessage, error) {
	m := &LoopMessage{}
	err := sc.Scan(&m.ID, &m.LoopID, &m.SenderID, &m.SenderRole, &m.RecipientID, &m.RecipientRole,
		&m.Subject, &m.Body, &m.Status, &m.DeliveryCount, &m.LeaseSeconds, &m.ForwardedFrom,
		&m.CreatedAt, &m.DeliveredAt, &m.AckedAt, &m.StepID, &m.Outcome, &m.MirrorOf,
		&m.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	m.BodyChars = utf8.RuneCountInString(m.Body)
	return m, nil
}

// mailByIdempotencyKey finds a sender's earlier post under the same key. The
// unique index makes this exact rather than a guess, and it is the reason a
// racing duplicate fails the insert instead of writing twice.
func (s *Store) mailByIdempotencyKey(ctx context.Context, senderID, key string) (*LoopMessage, error) {
	if senderID == "" || key == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+mailMessageColumns+`
		FROM loop_messages WHERE sender_id = ? AND idempotency_key = ?`, senderID, key)
	m, err := scanLoopMessage(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}
