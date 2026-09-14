// steps.go — where a loop stands in its play, and the enforcement that makes
// the sequence a contract. A message that does not belong to the current step
// is refused with a named error carrying the step it violated, next to the
// routing check, so no later transport can route around either.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Play statuses on a loop.
const (
	PlayRunning = "running"
	PlayDone    = "done"
	// PlayCapped means the cycle hit its round cap. The play stops
	// advancing and the state says why, instead of trading rounds forever.
	PlayCapped = "capped"
)

// ErrMailStepViolation is the sentinel behind every step refusal.
var ErrMailStepViolation = errors.New("store: step violation")

// ErrRoundCapReached is the sentinel behind a refused loop-back.
var ErrRoundCapReached = errors.New("store: round cap reached")

// RoundCapReached is the refusal a transport renders when a cycle has run out
// of rounds. The loop state is left saying capped, so this surfaces rather
// than being retried in silence.
type RoundCapReached struct {
	Play      string `json:"play"`
	StepID    string `json:"stepId"`
	Round     int    `json:"round"`
	MaxRounds int    `json:"maxRounds"`
	FromRole  string `json:"fromRole"`
	ToRole    string `json:"toRole"`
}

func (e *RoundCapReached) Error() string {
	return fmt.Sprintf("store: round cap reached: %s has taken its loop-back %d time(s), the cap is %d; "+
		"%s to %s refused and the play is capped, decide instead of cycling",
		e.Play, e.Round, e.MaxRounds, e.FromRole, e.ToRole)
}

// Unwrap lets callers match with errors.Is(err, ErrRoundCapReached).
func (e *RoundCapReached) Unwrap() error { return ErrRoundCapReached }

// StepViolation is a refusal a transport can render: it names the play, the
// step, who the step was waiting for and what it would have accepted.
type StepViolation struct {
	Play      string   `json:"play"`
	StepID    string   `json:"stepId"`
	StepActor string   `json:"stepActor"`
	Awaiting  []string `json:"awaiting,omitempty"`
	Allowed   []string `json:"allowed,omitempty"`
	FromRole  string   `json:"fromRole"`
	ToRole    string   `json:"toRole"`
	Outcome   string   `json:"outcome,omitempty"`
	Reason    string   `json:"reason"`
}

func (e *StepViolation) Error() string {
	msg := fmt.Sprintf("store: step violation: %s is on step %s (%s): %s to %s refused, %s",
		e.Play, e.StepID, e.StepActor, e.FromRole, e.ToRole, e.Reason)
	if len(e.Awaiting) > 0 {
		msg += fmt.Sprintf("; waiting for %s", strings.Join(e.Awaiting, ", "))
	}
	if len(e.Allowed) > 0 {
		msg += fmt.Sprintf("; this step accepts %s", strings.Join(e.Allowed, ", "))
	}
	return msg
}

// Unwrap lets callers match with errors.Is(err, ErrMailStepViolation).
func (e *StepViolation) Unwrap() error { return ErrMailStepViolation }

// stepMove is the loop state a message moves the play to.
type stepMove struct {
	StepID   string
	Awaiting []string
	Status   string
	RoundInc int
	Entered  bool
	// Refuse, when set, means the message is refused AND this state change
	// is still committed. That is how the cap surfaces: rolling the whole
	// thing back would leave the loop looking ready for another round.
	Refuse error
}

// StartPlay puts a loop on the entry step of a play, refusing a play the loop
// cannot staff. The engine owns the position from here: nothing but a message
// that satisfies the current step can move it.
func (s *Store) StartPlay(ctx context.Context, loopID, playName string) (*Loop, error) {
	play, ok := LookupPlay(playName)
	if !ok {
		return nil, fmt.Errorf("%w: no play called %q", ErrPlayInvalid, playName)
	}
	loop, err := s.GetLoop(ctx, loopID)
	if err != nil {
		return nil, err
	}
	if loop == nil {
		return nil, fmt.Errorf("%w: %s", ErrMailNotFound, loopID)
	}
	if loop.Ended() {
		return nil, fmt.Errorf("%w: %s is %s", ErrLoopEnded, loop.ID, loop.Status)
	}
	roster, err := s.mailRoster(ctx, loopID)
	if err != nil {
		return nil, err
	}
	if err := play.ValidateRoster(roster); err != nil {
		return nil, err
	}
	var notified []string
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		notified, err = startPlayTx(ctx, tx, loopID, play, time.Now().UnixMilli())
		return err
	})
	if err != nil {
		return nil, err
	}
	s.notifyMail(notified...)
	return s.GetLoop(ctx, loopID)
}

// startPlayTx puts the loop on the play's entry step and briefs whoever holds
// it, inside the caller's transaction. The caller has already checked that
// the roster can staff the play. Returns the member ids that now have mail.
func startPlayTx(ctx context.Context, tx *sql.Tx, loopID string, play *Play, now int64) ([]string, error) {
	entries, err := txRosterEntries(ctx, tx, loopID)
	if err != nil {
		return nil, err
	}
	entry := play.Step(play.Entry)
	awaiting := stepObligations(play, entry, rosterRoles(entries))
	if _, err := tx.ExecContext(ctx, `UPDATE loops
		SET play = ?, play_status = ?, step_id = ?, step_awaiting = ?, step_entered_at = ?,
		    play_ended_at = 0, updated_at = ?
		WHERE id = ?`,
		play.Name, PlayRunning, entry.ID, encodeAwaiting(awaiting), now, now, loopID); err != nil {
		return nil, err
	}
	notified, err := queueStepBrief(ctx, tx, loopID, play, entry, entries, now)
	if err != nil {
		return nil, err
	}
	return notified, enqueueLoopEventTx(ctx, tx, LoopEvent{
		Kind: EvPlayStarted, LoopID: loopID,
		Content: "play " + play.Name + " started at " + entry.ID,
		Detail: map[string]any{
			"play": play.Name, "stepId": entry.ID,
			"awaiting": awaiting, "maxRounds": play.EffectiveMaxRounds(),
		},
	}, now)
}

// DeliveredStepBrief returns the brief the member on the current step was
// ACTUALLY sent, falling back to the play definition when no message exists.
//
// CurrentStepBrief resolves from the live definition, so after an edit the
// board showed the new brief next to a message row containing the old one with
// nothing saying they differ. The row is what the agent read; the definition
// is only what it would read if briefed now.
func (s *Store) DeliveredStepBrief(ctx context.Context, loop *Loop) (string, bool) {
	if loop == nil || loop.StepID == "" {
		return CurrentStepBrief(loop)
	}
	var body string
	err := s.db.QueryRowContext(ctx,
		`SELECT body FROM loop_messages
		 WHERE loop_id = ? AND step_id = ? AND sender_role = ? AND status != ?
		 ORDER BY seq DESC LIMIT 1`,
		loop.ID, loop.StepID, RoleEngine, MailCancelled).Scan(&body)
	if err == nil && body != "" {
		return body, true
	}
	return CurrentStepBrief(loop)
}

// CurrentStepBrief returns the brief queued for the step the loop stands on.
// It is the step-specific expectation, which is why a role holding two steps
// in one play needs no hedged prompt.
func CurrentStepBrief(loop *Loop) (string, bool) {
	if loop == nil || loop.Play == "" || loop.StepID == "" {
		return "", false
	}
	play, ok := LookupPlay(loop.Play)
	if !ok {
		return "", false
	}
	s := play.Step(loop.StepID)
	if s == nil {
		return "", false
	}
	return s.Brief, true
}

// mailRoster lists the roles of a loop's live members.
func (s *Store) mailRoster(ctx context.Context, loopID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role FROM loop_members WHERE loop_id = ? AND status != ? ORDER BY seq`,
		loopID, LoopMemberDismissed)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// rosterEntry is a live member's address and identity.
type rosterEntry struct {
	ID   string
	Role string
}

func txRosterEntries(ctx context.Context, tx *sql.Tx, loopID string) ([]rosterEntry, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, role FROM loop_members WHERE loop_id = ? AND status != ? ORDER BY seq`,
		loopID, LoopMemberDismissed)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []rosterEntry{}
	for rows.Next() {
		var e rosterEntry
		if err := rows.Scan(&e.ID, &e.Role); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func rosterRoles(entries []rosterEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Role)
	}
	return out
}

// RoleEngine is the sender of a message the engine wrote itself.
const RoleEngine = "ENGINE"

// queueStepBrief delivers a step's brief to whoever holds it, as a real
// message, at the moment the step becomes current. Exposing the text and
// hoping somebody fetches it is not delivery: an agent that boots after the
// step opened would never see it. A fan-in step briefs every member of the
// family. Returns the member ids that now have mail.
func queueStepBrief(ctx context.Context, tx *sql.Tx, loopID string, p *Play, step *PlayStep, roster []rosterEntry, at int64) ([]string, error) {
	if step == nil || step.Brief == "" {
		return nil, nil
	}
	family := BaseRole(step.Actor)
	notified := []string{}
	for _, m := range roster {
		if BaseRole(m.Role) != family {
			continue
		}
		id := newMailID("msg")
		if _, err := tx.ExecContext(ctx, `INSERT INTO loop_messages (`+mailMessageColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, loopID, "", RoleEngine, m.ID, m.Role,
			p.Title+": "+step.ID, step.Brief, MailPending, 0, 0, "",
			at, 0, 0, step.ID, "", "", ""); err != nil {
			return nil, err
		}
		notified = append(notified, m.ID)
	}
	return notified, nil
}

// enforceStep decides whether a message belongs to the loop's current step and
// what state the play moves to. A loop with no running play is unenforced, and
// so is anything to or from the engineer, who is a participant rather than a
// step in the sequence.
func enforceStep(loop *Loop, roster []string, fromRole, toRole, outcome string) (*stepMove, error) {
	if loop.Play == "" {
		return nil, nil
	}
	if isEngineerRole(fromRole) || isEngineerRole(toRole) {
		return nil, nil
	}
	if loop.PlayStatus != PlayRunning {
		// A finished or capped play has nothing left to hand between
		// members. Letting mail through unenforced here is how a loop kept
		// trading messages, and money, after its play said it was over; the
		// orchestrator can still talk to the engineer, which is the one
		// conversation left to have.
		return nil, &StepViolation{
			Play: loop.Play, StepID: loop.StepID,
			FromRole: strings.ToUpper(fromRole), ToRole: strings.ToUpper(toRole), Outcome: outcome,
			Reason: "the play is " + orDefault(loop.PlayStatus, "not running") +
				", so nothing more is routed between members; report to the ENGINEER",
		}
	}
	play, ok := LookupPlay(loop.Play)
	if !ok {
		return nil, fmt.Errorf("%w: loop %s references unknown play %q", ErrPlayInvalid, loop.ID, loop.Play)
	}
	step := play.Step(loop.StepID)
	if step == nil {
		return nil, fmt.Errorf("%w: loop %s stands on unknown step %q of %s",
			ErrPlayInvalid, loop.ID, loop.StepID, loop.Play)
	}
	from, to := strings.ToUpper(fromRole), strings.ToUpper(toRole)
	awaiting := append([]string{}, loop.StepAwaiting...)
	refuse := func(reason string, allowed []string) error {
		return &StepViolation{
			Play: play.Name, StepID: step.ID, StepActor: step.Actor,
			Awaiting: awaiting, Allowed: allowed,
			FromRole: from, ToRole: to, Outcome: outcome, Reason: reason,
		}
	}

	fanningOut := len(step.Next) == 1 && step.Next[0].FanOut
	var discharged string
	var chosen *PlayEdge

	if fanningOut {
		if BaseRole(from) != BaseRole(step.Actor) {
			return nil, refuse("this step is held by "+step.Actor, awaiting)
		}
		if !containsRole(awaiting, to) {
			return nil, refuse("every member of the fan-out is briefed exactly once", awaiting)
		}
		discharged, chosen = to, &step.Next[0]
	} else {
		if !containsRole(awaiting, from) {
			return nil, refuse("it is not this member's turn", nil)
		}
		discharged = from
		allowed := make([]string, 0, len(step.Next))
		guarded := false
		for i := range step.Next {
			target := play.Step(step.Next[i].To)
			allowed = append(allowed, target.Actor)
			if BaseRole(target.Actor) == BaseRole(to) && step.Next[i].When != "" {
				guarded = true
				if step.Next[i].When == outcome {
					chosen = &step.Next[i]
					break
				}
			}
		}
		if chosen == nil {
			for i := range step.Next {
				target := play.Step(step.Next[i].To)
				if BaseRole(target.Actor) == BaseRole(to) && step.Next[i].When == "" {
					chosen = &step.Next[i]
					break
				}
			}
		}
		if chosen == nil {
			if guarded {
				return nil, refuse(fmt.Sprintf("outcome %q matches no edge; this step decides on %s",
					outcome, strings.Join(stepGuards(step), " or ")), allowed)
			}
			return nil, refuse("this step does not hand the work to "+to, allowed)
		}
	}

	remaining := removeRole(awaiting, discharged)
	if len(remaining) > 0 {
		return &stepMove{StepID: step.ID, Awaiting: remaining, Status: PlayRunning}, nil
	}
	next := play.Step(chosen.To)
	if chosen.Loop {
		if capped := play.EffectiveMaxRounds(); loop.Round >= capped {
			return &stepMove{
				StepID: loop.StepID, Awaiting: awaiting, Status: PlayCapped,
				Refuse: &RoundCapReached{
					Play: play.Name, StepID: step.ID, Round: loop.Round,
					MaxRounds: capped, FromRole: from, ToRole: to,
				},
			}, nil
		}
	}
	mv := &stepMove{StepID: next.ID, Status: PlayRunning, Entered: true}
	if chosen.Loop {
		mv.RoundInc = 1
	}
	if next.Terminal() {
		mv.Status = PlayDone
	} else {
		mv.Awaiting = stepObligations(play, next, roster)
	}
	return mv, nil
}

func stepGuards(s *PlayStep) []string {
	out := []string{}
	for _, e := range s.Next {
		if e.When != "" {
			out = append(out, e.When)
		}
	}
	return out
}

func containsRole(list []string, role string) bool {
	for _, r := range list {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

func removeRole(list []string, role string) []string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		if strings.EqualFold(r, role) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func encodeAwaiting(roles []string) string {
	if len(roles) == 0 {
		return ""
	}
	b, err := json.Marshal(roles)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeAwaiting(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}
