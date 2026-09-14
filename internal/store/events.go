// events.go — loop activity as rows. Every event is a row in loop_events,
// written in the same transaction as the row it describes, so an event never
// describes a change that did not commit.
//
// Refusals are events too, and they are the reason this exists: a step
// violation the UI renders as a card is enforcement made visible, where a
// silent 409 is enforcement nobody sees.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	_ "modernc.org/sqlite" // ensure the driver is linked in test binaries
)

// LoopEventKind values, carried as the payload's subtype.
const (
	EvLoopCreated   = "loop.created"
	EvLoopEnded     = "loop.ended"
	EvPlayStarted   = "play.started"
	EvStepAdvanced  = "step.advanced"
	EvStepRefused   = "step.refused"
	EvRoundCapped   = "round.capped"
	EvRouteRefused  = "route.refused"
	EvMailPosted    = "mail.posted"
	EvMailAcked     = "mail.acked"
	EvMemberChanged = "member.changed"
	// EvMemberStranded is the loop saying nobody is reading a member's mail.
	// Recorded once per transition so the board refreshes and the row stays
	// diagnosable long after.
	EvMemberStranded = "member.stranded"
	// EvMailUndeliverable is a message whose lease ran out too many times
	// and that agentd stopped re-sending.
	EvMailUndeliverable = "mail.undeliverable"
	// EvLoopCapped is a loop agentd ended because a limit tripped.
	EvLoopCapped = "loop.capped"
	// EvAutoForwarded is a member's unsent output passed to the
	// orchestrator as an advisory.
	EvAutoForwarded = "mail.auto_forwarded"
)

// LoopEvent is one thing that happened to a loop.
type LoopEvent struct {
	Kind    string `json:"kind"`
	LoopID  string `json:"loopId"`
	Content string `json:"content"`
	Detail  any    `json:"detail,omitempty"`
}

// enqueueLoopEventTx appends one loop event to loop_events inside the
// caller's transaction, so the event and the change it describes commit
// together.
func enqueueLoopEventTx(ctx context.Context, tx *sql.Tx, ev LoopEvent, at int64) error {
	detail := map[string]any{}
	if ev.Detail != nil {
		if raw, err := json.Marshal(ev.Detail); err == nil {
			_ = json.Unmarshal(raw, &detail)
		}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		encoded = []byte("{}")
	}
	id := newMailID("ev")
	_, err = tx.ExecContext(ctx, `INSERT INTO loop_events
		(id, loop_id, kind, content, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, ev.LoopID, ev.Kind, ev.Content, string(encoded), at)
	return err
}

// PublishLoopEvent writes one loop event in its own transaction, for the
// refusals that happen before any row is written and therefore have no
// transaction to ride along in.
func (s *Store) PublishLoopEvent(ctx context.Context, ev LoopEvent) error {
	at := time.Now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) error {
		return enqueueLoopEventTx(ctx, tx, ev, at)
	})
}

// LoopRefusal is one refusal, kept as a row: a stall diagnosed a day later
// has to read the same as one watched live.
type LoopRefusal struct {
	ID        string         `json:"id"`
	LoopID    string         `json:"loopId"`
	Kind      string         `json:"kind"`
	FromRole  string         `json:"fromRole,omitempty"`
	ToRole    string         `json:"toRole,omitempty"`
	Play      string         `json:"play,omitempty"`
	StepID    string         `json:"stepId,omitempty"`
	Content   string         `json:"content"`
	Detail    map[string]any `json:"detail,omitempty"`
	CreatedAt int64          `json:"createdAt"`
}

// recordRefusalTx writes the refusal row and its event together, so the
// history and the live signal can never disagree about whether it happened.
func recordRefusalTx(ctx context.Context, tx *sql.Tx, ev LoopEvent, at int64) error {
	detail := map[string]any{}
	if ev.Detail != nil {
		if raw, err := json.Marshal(ev.Detail); err == nil {
			_ = json.Unmarshal(raw, &detail)
		}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		encoded = []byte("{}")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO loop_refusals
		(id, loop_id, kind, from_role, to_role, play, step_id, content, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newMailID("refusal"), ev.LoopID, ev.Kind,
		stringField(detail, "fromRole"), stringField(detail, "toRole"),
		stringField(detail, "play"), stringField(detail, "stepId"),
		ev.Content, string(encoded), at); err != nil {
		return err
	}
	return enqueueLoopEventTx(ctx, tx, ev, at)
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// RecordRefusal writes a refusal that has no transaction of its own to ride
// in, such as a routing refusal that never got as far as a row.
func (s *Store) RecordRefusal(ctx context.Context, ev LoopEvent) error {
	at := time.Now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) error {
		return recordRefusalTx(ctx, tx, ev, at)
	})
}

// ListLoopRefusals returns a loop's refusals oldest-first, so the board can
// show them from history rather than only the ones someone was watching for.
func (s *Store) ListLoopRefusals(ctx context.Context, loopID string, limit int) ([]*LoopRefusal, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, loop_id, kind, from_role, to_role,
		play, step_id, content, detail, created_at FROM loop_refusals
		WHERE loop_id = ? ORDER BY seq LIMIT ?`, loopID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*LoopRefusal{}
	for rows.Next() {
		r := &LoopRefusal{}
		var detail string
		if err := rows.Scan(&r.ID, &r.LoopID, &r.Kind, &r.FromRole, &r.ToRole,
			&r.Play, &r.StepID, &r.Content, &detail, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Detail = map[string]any{}
		_ = json.Unmarshal([]byte(detail), &r.Detail)
		out = append(out, r)
	}
	return out, rows.Err()
}
