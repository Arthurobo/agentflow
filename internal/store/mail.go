// mail.go — loops, members, notes and loop state for the engine-owned mailbox
// (migration 0016). Delivery itself lives in mailbox.go.

package store

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Mail loop statuses.
const (
	LoopActive   = "active"
	LoopComplete = "complete"
	LoopKilled   = "killed"
	// LoopInterrupted is what the boot reaper writes: the loop did not
	// finish and nobody stopped it, the daemon that was running it went
	// away. Distinct from killed, which someone chose, and from complete,
	// which it never was. It is terminal — see Ended.
	LoopInterrupted = "interrupted"
	// LoopCapped means the loop hit one of its limits (messages, wall clock,
	// members) or its play ran out of rounds, and agentd ended it. Terminal,
	// like the others, and distinct from them so the board can say why.
	LoopCapped = "capped"
)

// Mail member statuses. Ending a loop parks members as idle with live tokens;
// only an explicit dismiss retires them.
const (
	LoopMemberActive    = "active"
	LoopMemberIdle      = "idle"
	LoopMemberDismissed = "dismissed"
)

// Reserved roles: every path terminates at the orchestrator, and the engineer
// is a participant rather than a side channel.
const (
	RoleOrchestrator = "ORCHESTRATOR"
	RoleEngineer     = "ENGINEER"
)

// DefaultLoopRoles is the roster a loop gets when the caller names none.
var DefaultLoopRoles = []string{RoleOrchestrator, RoleEngineer, "INVESTIGATION", "IMPLEMENTATION", "REVIEW"}

// DefaultPollIntervalSeconds is the hint a member gets for its poll cadence.
const DefaultPollIntervalSeconds = 60

// Loop limit defaults. A loop that runs away costs real money and real
// machine time while nobody is watching, so every loop has a ceiling on the
// messages it may exchange, how long it may run and how many members it may
// staff. A zero on the row means the default. When a limit trips the sweeper
// ends the loop as capped and stops its members.
const (
	DefaultMaxMessagesPerRound = 200
	DefaultMaxWallClockSeconds = int64(8 * 60 * 60) // 8 h
	DefaultMaxMembers          = 8
)

// Loop is one mission: a roster, a pipeline position and a lineage.
type Loop struct {
	ID string `json:"id"`
	// Title names the loop. It is what the list, the board header and every
	// member's session name read.
	Title string `json:"title,omitempty"`
	// Task is the instruction, delivered to the orchestrator as its first
	// message. It is not a name and nothing displays it as one.
	Task                string `json:"task,omitempty"`
	CWD                 string `json:"cwd,omitempty"`
	Status              string `json:"status"`
	Round               int    `json:"round"`
	State               string `json:"state,omitempty"`
	ParentLoopID        string `json:"parentLoopId,omitempty"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds,omitempty"`
	EndReason           string `json:"endReason,omitempty"`
	// Limits for this loop. 0 means the default. The message limit counts
	// every message the loop has carried, not one round's.
	MaxMessagesPerRound int   `json:"maxMessagesPerRound,omitempty"`
	MaxWallClockSeconds int64 `json:"maxWallClockSeconds,omitempty"`
	MaxMembers          int   `json:"maxMembers,omitempty"`
	// StartedAt is when the loop started, in unix SECONDS; the wall-clock
	// limit is measured from it.
	StartedAt int64 `json:"startedAt,omitempty"`
	// PlayEndedAt is when the play reached done or capped (unix ms), zero
	// while it is still running.
	PlayEndedAt int64 `json:"playEndedAt,omitempty"`
	CreatedAt   int64 `json:"createdAt,omitempty"`
	UpdatedAt   int64 `json:"updatedAt,omitempty"`
	CompletedAt int64 `json:"completedAt,omitempty"`

	// Play position (migration 0017). The engine owns these: only a message
	// that satisfies the current step moves them.
	Play          string   `json:"play,omitempty"`
	PlayStatus    string   `json:"playStatus,omitempty"`
	StepID        string   `json:"stepId,omitempty"`
	StepAwaiting  []string `json:"stepAwaiting,omitempty"`
	StepEnteredAt int64    `json:"stepEnteredAt,omitempty"`

	// Generation is the agentd boot that created this loop (migration
	// 0027). A loop whose generation is not the running
	// daemon's belongs to a previous life: its members' processes are gone
	// and nothing left in this process will ever move it, so the boot
	// reaper ends it. Empty means it predates the stamp, which counts as
	// foreign for the same reason.
	Generation string `json:"generation,omitempty"`
}

// PlayInProgress reports whether the loop is standing on a step that is still
// enforced.
func (l *Loop) PlayInProgress() bool {
	return l.Play != "" && l.PlayStatus == PlayRunning
}

// Ended reports whether the loop is no longer taking work.
func (l *Loop) Ended() bool {
	return l.Status == LoopComplete || l.Status == LoopKilled ||
		l.Status == LoopInterrupted || l.Status == LoopCapped
}

// MemberSpec is how a member is staffed: its address, and the tool and model
// it will run on. Tool and model are DATA this round; nothing launches from
// them yet.
type MemberSpec struct {
	Role                string `json:"role"`
	Tool                string `json:"tool,omitempty"`
	Model               string `json:"model,omitempty"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds,omitempty"`
	// RunID is the spawner run this member will be driven by, allocated by the
	// caller BEFORE the transaction so the row is born knowing it. Empty means
	// the member runs somewhere we did not start it.
	RunID string `json:"-"`
}

// RoleSpecs builds default specs for plain role names.
func RoleSpecs(roles ...string) []MemberSpec {
	out := make([]MemberSpec, 0, len(roles))
	for _, r := range roles {
		out = append(out, MemberSpec{Role: r})
	}
	return out
}

// LoopMember is one addressable participant. TokenHash is never serialized.
type LoopMember struct {
	ID                  string `json:"id"`
	LoopID              string `json:"loopId"`
	Role                string `json:"role"`
	Tool                string `json:"tool,omitempty"`
	Model               string `json:"model,omitempty"`
	Status              string `json:"status"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds,omitempty"`
	CharsIn             int64  `json:"charsIn"`
	CreatedAt           int64  `json:"createdAt,omitempty"`
	LastSeenAt          int64  `json:"lastSeenAt,omitempty"`
	// RunID is the spawner run this member is being driven by, empty when the
	// member runs somewhere we did not start it. This is the link, not
	// SessionID: a run id exists before the process does.
	RunID string `json:"runId,omitempty"`
	// SessionID is the engine's session id once discovery lands. Permanently
	// empty for OpenCode, which does not write to the claude corpus.
	SessionID string `json:"sessionId,omitempty"`
	// LastError is why this member has no process.
	LastError string `json:"lastError,omitempty"`
	// StrandedSince is when the sweep first decided nobody is reading this
	// member's mail. Zero means not stranded.
	StrandedSince int64 `json:"strandedSince,omitempty"`
	// LastPostedAt is the member's last outbound post. A delivery older than
	// this was demonstrably acted on.
	LastPostedAt int64 `json:"lastPostedAt,omitempty"`
	// LastPolledAt is the member's last read of its own inbox. NOT LastSeenAt,
	// which every authenticated call stamps: a member writing notes every
	// thirty seconds is alive and is still not reading its mail.
	LastPolledAt int64  `json:"lastPolledAt,omitempty"`
	TokenHash    string `json:"-"`
}

// LoopNote is one durable conclusion kept by role, not by session.
type LoopNote struct {
	ID        string `json:"id"`
	LoopID    string `json:"loopId"`
	Role      string `json:"role"`
	AuthorID  string `json:"authorId,omitempty"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

const mailLoopColumns = `id, title, task, cwd, status, round, state, parent_loop_id,
	poll_interval_seconds, end_reason, created_at, updated_at, completed_at,
	play, play_status, step_id, step_awaiting, step_entered_at, generation,
	max_messages_per_round, max_wall_clock_seconds, max_members, started_at, play_ended_at`

const mailMemberColumns = `id, loop_id, role, token_hash, status,
	poll_interval_seconds, chars_in, created_at, last_seen_at, tool, model,
	run_id, session_id, last_error, stranded_since, last_posted_at, last_polled_at`

// NewMailToken mints a member credential. Only the caller ever sees it.
func NewMailToken() string {
	var b [24]byte
	if _, err := crand.Read(b[:]); err != nil {
		return fmt.Sprintf("af_%d", time.Now().UnixNano())
	}
	return "af_" + base64.RawURLEncoding.EncodeToString(b[:])
}

func newMailID(prefix string) string {
	var b [6]byte
	if _, err := crand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// CreateLoop staffs a loop from plain role names, every member on the
// default tool.
func (s *Store) CreateLoop(ctx context.Context, l *Loop, roles []string) (map[string]string, error) {
	if len(roles) == 0 {
		roles = DefaultLoopRoles
	}
	return s.CreateCrew(ctx, l, RoleSpecs(roles...))
}

// ErrTooManyMembers refuses a crew larger than the loop's member limit. It is
// the caller's mistake, not the server's, so transports answer it with a 400.
var ErrTooManyMembers = errors.New("store: too many members")

// CreateCrew inserts the loop and its roster, returning the plaintext
// token per role. ENGINEER is an address, not a poller, so it gets none. An
// unknown tool or model pairing is refused here rather than discovered at
// launch.
func (s *Store) CreateCrew(ctx context.Context, l *Loop, crew []MemberSpec) (map[string]string, error) {
	crew, err := prepareCrew(l, crew)
	if err != nil {
		return nil, err
	}
	var tokens map[string]string
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		tokens, err = insertCrewTx(ctx, tx, l, crew, time.Now().UnixMilli())
		return err
	})
	if err != nil {
		return nil, err
	}
	return tokens, nil
}

// CrewRoles is the roster a crew will have once CreateCrew has normalised it:
// upper-cased roles with the ENGINEER added when the caller left him out. It
// lets a caller check the crew against a play before anything is written.
func CrewRoles(crew []MemberSpec) []string {
	out := make([]string, 0, len(crew)+1)
	haveEngineer := false
	for _, m := range crew {
		role := strings.ToUpper(strings.TrimSpace(m.Role))
		if role == "" {
			continue
		}
		if role == RoleEngineer {
			haveEngineer = true
		}
		out = append(out, role)
	}
	if !haveEngineer {
		out = append(out, RoleEngineer)
	}
	return out
}

// prepareCrew fills the loop's defaults and validates and normalises the
// crew. Everything that can refuse a create happens here, before any
// transaction, so a refused create writes nothing.
func prepareCrew(l *Loop, crew []MemberSpec) ([]MemberSpec, error) {
	if l.ID == "" {
		l.ID = newMailID("loop")
	}
	if l.Status == "" {
		l.Status = LoopActive
	}
	if l.PollIntervalSeconds <= 0 {
		l.PollIntervalSeconds = DefaultPollIntervalSeconds
	}
	if len(crew) == 0 {
		crew = RoleSpecs(DefaultLoopRoles...)
	}
	crew = append([]MemberSpec(nil), crew...)
	// Every loop has an ENGINEER, whatever the caller sent. He is an address
	// rather than a process, so a UI that lists the AGENTS has no reason to
	// include him and no reason to ask which engine he runs on; but the
	// surface resolves his member row to deliver anything he types, so a loop
	// created without one would silently refuse to let him speak to it.
	haveEngineer := false
	for i := range crew {
		if strings.ToUpper(strings.TrimSpace(crew[i].Role)) == RoleEngineer {
			haveEngineer = true
			break
		}
	}
	if !haveEngineer {
		crew = append(crew, MemberSpec{Role: RoleEngineer})
	}
	// The ENGINEER counts toward the limit. Without one a caller could ask
	// for a thousand roles and the launch would hold the database writer for
	// minutes.
	if max := effectiveMaxMembers(l.MaxMembers); len(crew) > max {
		return nil, fmt.Errorf("%w: %d members, this loop allows %d", ErrTooManyMembers, len(crew), max)
	}
	for i := range crew {
		crew[i].Role = strings.ToUpper(strings.TrimSpace(crew[i].Role))
		crew[i].Tool = NormaliseTool(crew[i].Tool)
		if err := ValidateRole(crew[i].Role); err != nil {
			return nil, err
		}
		if crew[i].Role == RoleEngineer {
			continue // an address, not a process: it runs on no tool
		}
		if err := ValidateToolModel(crew[i].Tool, crew[i].Model); err != nil {
			return nil, fmt.Errorf("%w (role %s)", err, crew[i].Role)
		}
	}
	return crew, nil
}

func effectiveMaxMembers(n int) int {
	if n <= 0 {
		return DefaultMaxMembers
	}
	return n
}

// insertCrewTx writes the loop row and one row per member inside the caller's
// transaction. The crew must already have been through prepareCrew.
func insertCrewTx(ctx context.Context, tx *sql.Tx, l *Loop, crew []MemberSpec, now int64) (map[string]string, error) {
	l.CreatedAt, l.UpdatedAt = now, now
	l.StartedAt = now / 1000
	tokens := map[string]string{}
	if _, err := tx.ExecContext(ctx, `INSERT INTO loops (`+mailLoopColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.Title, l.Task, l.CWD, l.Status, l.Round, l.State, l.ParentLoopID,
		l.PollIntervalSeconds, l.EndReason, l.CreatedAt, l.UpdatedAt, l.CompletedAt,
		l.Play, l.PlayStatus, l.StepID, encodeAwaiting(l.StepAwaiting), l.StepEnteredAt,
		l.Generation,
		l.MaxMessagesPerRound, l.MaxWallClockSeconds, l.MaxMembers, l.StartedAt, l.PlayEndedAt); err != nil {
		return nil, err
	}
	for _, spec := range crew {
		if spec.Role == "" {
			continue
		}
		hash, tool, model := "", spec.Tool, spec.Model
		if spec.Role == RoleEngineer {
			tool, model = "", ""
		} else {
			token := NewMailToken()
			tokens[spec.Role] = token
			hash = HashToken(token)
		}
		poll := spec.PollIntervalSeconds
		if poll <= 0 {
			poll = l.PollIntervalSeconds
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO loop_members (`+mailMemberColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newMailID(strings.ToLower(spec.Role)), l.ID, spec.Role, hash, LoopMemberActive,
			poll, 0, now, 0, tool, model, spec.RunID, "", "", 0, 0, 0); err != nil {
			return nil, err
		}
	}
	if err := enqueueLoopEventTx(ctx, tx, LoopEvent{
		Kind: EvLoopCreated, LoopID: l.ID,
		Content: "loop created: " + l.Task,
		Detail:  map[string]any{"task": l.Task, "cwd": l.CWD, "crew": crew},
	}, now); err != nil {
		return nil, err
	}
	return tokens, nil
}

// StartLoop creates a loop, delivers its task to the orchestrator and puts it
// on the entry step of a play, all in ONE transaction.
//
// These used to be three calls with a commit between each, so a play that
// refused the crew left an active loop with members, tokens and a delivered
// task behind a 400. Now a refusal at any point writes nothing.
func (s *Store) StartLoop(ctx context.Context, l *Loop, crew []MemberSpec, playName, task string) (map[string]string, error) {
	play, ok := LookupPlay(playName)
	if !ok {
		return nil, fmt.Errorf("%w: no play called %q", ErrPlayInvalid, playName)
	}
	crew, err := prepareCrew(l, crew)
	if err != nil {
		return nil, err
	}
	if err := play.ValidateRoster(CrewRoles(crew)); err != nil {
		return nil, err
	}
	var tokens map[string]string
	var woken []string
	err = s.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		var err error
		if tokens, err = insertCrewTx(ctx, tx, l, crew, now); err != nil {
			return err
		}
		if woken, err = postTaskTx(ctx, tx, l.ID, task, now); err != nil {
			return err
		}
		briefed, err := startPlayTx(ctx, tx, l.ID, play, now)
		woken = append(woken, briefed...)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.notifyMail(woken...)
	return tokens, nil
}

// ContinueResult is what ContinueLoop did.
type ContinueResult struct {
	// Tokens are the plaintext tokens of members created fresh for the
	// successor. Adopted roles keep their old token and are absent.
	Tokens map[string]string
	// Adopted are the roles carried over from the previous loop.
	Adopted []string
	// NotesCarried is how many notes were copied.
	NotesCarried int
}

// ContinueLoop ends the previous loop (when it is still open), creates its
// successor, adopts the previous loop's live members, optionally carries the
// notes, delivers the new task and starts the play, in one transaction.
func (s *Store) ContinueLoop(ctx context.Context, previousID string, successor *Loop, crew []MemberSpec,
	playName, task string, carryNotes bool) (*ContinueResult, error) {
	play, ok := LookupPlay(playName)
	if !ok {
		return nil, fmt.Errorf("%w: no play called %q", ErrPlayInvalid, playName)
	}
	crew, err := prepareCrew(successor, crew)
	if err != nil {
		return nil, err
	}
	if err := play.ValidateRoster(CrewRoles(crew)); err != nil {
		return nil, err
	}
	out := &ContinueResult{}
	var woken []string
	err = s.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		previous, err := scanLoop(tx.QueryRowContext(ctx,
			`SELECT `+mailLoopColumns+` FROM loops WHERE id = ?`, previousID))
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: %s", ErrMailNotFound, previousID)
		}
		if err != nil {
			return err
		}
		if !previous.Ended() {
			if _, err := endLoopTx(ctx, tx, previousID, LoopComplete, "continued into a successor", false, now); err != nil {
				return err
			}
		}
		if out.Tokens, err = insertCrewTx(ctx, tx, successor, crew, now); err != nil {
			return err
		}
		if out.Adopted, err = adoptLoopMembersTx(ctx, tx, previousID, successor.ID); err != nil {
			return err
		}
		// An adopted member keeps its token, so the one just minted for that
		// role is superseded and must not be handed out.
		for _, role := range out.Adopted {
			delete(out.Tokens, role)
		}
		if carryNotes {
			if out.NotesCarried, err = copyLoopNotesTx(ctx, tx, previousID, successor.ID); err != nil {
				return err
			}
		}
		if woken, err = postTaskTx(ctx, tx, successor.ID, task, now); err != nil {
			return err
		}
		briefed, err := startPlayTx(ctx, tx, successor.ID, play, now)
		woken = append(woken, briefed...)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.notifyMail(woken...)
	return out, nil
}

// postTaskTx posts the engineer's instruction to the orchestrator as mail,
// inside the caller's transaction. It is the same row PostMail would write:
// engineer to orchestrator is always routable, is never step-enforced and
// needs no mirror, so none of that machinery has anything to decide.
//
// Failing to find either end is a real failure: a loop whose orchestrator
// never got the instruction will sit idle or invent its own work.
func postTaskTx(ctx context.Context, tx *sql.Tx, loopID, task string, now int64) ([]string, error) {
	engineer, err := txMemberByRole(ctx, tx, loopID, RoleEngineer)
	if err != nil {
		return nil, err
	}
	orchestrator, err := txMemberByRole(ctx, tx, loopID, RoleOrchestrator)
	if err != nil {
		return nil, err
	}
	if engineer == nil || orchestrator == nil {
		return nil, fmt.Errorf("%w: loop %s is missing an engineer or an orchestrator", ErrPlayInvalid, loopID)
	}
	body := strings.TrimSpace(task)
	id := newMailID("msg")
	const subject = "Task from the engineer"
	if _, err := tx.ExecContext(ctx, `INSERT INTO loop_messages (`+mailMessageColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, loopID, engineer.ID, engineer.Role, orchestrator.ID, orchestrator.Role,
		subject, body, MailPending, 0, 0, "", now, 0, 0, "", "", "", ""); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE loop_members SET last_posted_at = ? WHERE id = ?`, now, engineer.ID); err != nil {
		return nil, err
	}
	if err := enqueueLoopEventTx(ctx, tx, LoopEvent{
		Kind: EvMailPosted, LoopID: loopID,
		Content: engineer.Role + " to " + orchestrator.Role,
		Detail: map[string]any{
			"messageId": id, "fromRole": engineer.Role, "toRole": orchestrator.Role,
			"subject": subject, "bodyChars": utf8.RuneCountInString(body),
		},
	}, now); err != nil {
		return nil, err
	}
	return []string{orchestrator.ID}, nil
}

// GetLoop returns one loop (nil when absent).
func (s *Store) GetLoop(ctx context.Context, id string) (*Loop, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+mailLoopColumns+` FROM loops WHERE id = ?`, id)
	l, err := scanLoop(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

// ListLoops returns loops newest-first, optionally filtered by status.
func (s *Store) ListLoops(ctx context.Context, status string, limit int) ([]*Loop, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + mailLoopColumns + ` FROM loops`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Loop{}
	for rows.Next() {
		l, err := scanLoop(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LoopLineage returns the loop id followed by its ancestors, nearest first.
func (s *Store) LoopLineage(ctx context.Context, loopID string) ([]string, error) {
	chain := []string{loopID}
	seen := map[string]bool{loopID: true}
	current := loopID
	for i := 0; i < 16; i++ {
		var parent string
		err := s.db.QueryRowContext(ctx,
			`SELECT parent_loop_id FROM loops WHERE id = ?`, current).Scan(&parent)
		if err == sql.ErrNoRows {
			break
		}
		if err != nil {
			return nil, err
		}
		if parent == "" || seen[parent] {
			break
		}
		chain = append(chain, parent)
		seen[parent] = true
		current = parent
	}
	return chain, nil
}

// ErrRoundIsEngineOwned refuses a hand-written round.
var ErrRoundIsEngineOwned = errors.New("store: the round is the engine's to count")

// SetLoopState writes the pipeline position a replacement orchestrator
// resumes from. Nil fields are left alone. The round is NOT writable here:
// only the loop-back edge counts a round, which is what makes "until review
// reports clean" countable rather than hopeful. A second writer makes it
// hopeful again.
func (s *Store) SetLoopState(ctx context.Context, loopID string, state *string, round *int) (*Loop, error) {
	if round != nil {
		return nil, fmt.Errorf("%w: the loop-back edge counts rounds, nobody writes one by hand", ErrRoundIsEngineOwned)
	}
	q := `UPDATE loops SET updated_at = ?`
	args := []any{time.Now().UnixMilli()}
	if state != nil {
		q += `, state = ?`
		args = append(args, *state)
	}
	if round != nil {
		q += `, round = ?`
		args = append(args, *round)
	}
	q += ` WHERE id = ?`
	args = append(args, loopID)
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return nil, err
	}
	return s.GetLoop(ctx, loopID)
}

// ListActiveLoopsNotInGeneration returns every loop still taking work whose
// generation is not gen: the loops a PREVIOUS agentd life left behind
// . The boot reconciler ends these before anything
// serves, exactly as it reaps the managed rows.
//
// It is the loop-level twin of ListActiveManagedSessionsNotInGeneration and
// treats an empty generation the same way: as foreign. Rows written before
// the stamp existed have one, and there is no way to tell "never stamped"
// from "stamped by a life we cannot name" — both are orphans. That is also
// what catches the loops already on disk, which is the whole cleanup.
//
// Passing an empty gen would match every stamped row and nothing else, which
// is never what a caller wants, so it is rejected rather than obeyed.
func (s *Store) ListActiveLoopsNotInGeneration(ctx context.Context, gen string) ([]*Loop, error) {
	if gen == "" {
		return nil, errors.New("store: refusing to reap loops against an empty generation")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+mailLoopColumns+` FROM loops
		 WHERE status NOT IN (?, ?, ?, ?)
		   AND COALESCE(generation, '') != ?
		 ORDER BY created_at ASC LIMIT 500`,
		LoopComplete, LoopKilled, LoopInterrupted, LoopCapped, gen)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Loop{}
	for rows.Next() {
		l, err := scanLoop(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// BudgetStatus is where a loop stands against its limits.
type BudgetStatus struct {
	Messages    int   `json:"messages"`
	MaxMessages int   `json:"maxMessages"`
	WallClock   int64 `json:"wallClockSeconds"`
	MaxWall     int64 `json:"maxWallClockSeconds"`
	Members     int   `json:"members"`
	MaxMembers  int   `json:"maxMembers"`
	// Tripped says a limit has been passed, and Reason says which.
	Tripped bool   `json:"tripped"`
	Reason  string `json:"reason,omitempty"`
}

// Budget reports where a loop stands against its message, wall-clock and
// member limits.
func (s *Store) Budget(ctx context.Context, loopID string) (BudgetStatus, error) {
	return s.budgetAt(ctx, loopID, time.Now().Unix())
}

func (s *Store) budgetAt(ctx context.Context, loopID string, nowSec int64) (BudgetStatus, error) {
	var b BudgetStatus
	var startedAt, createdAt int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT started_at, created_at, max_messages_per_round, max_wall_clock_seconds, max_members
		 FROM loops WHERE id = ?`, loopID,
	).Scan(&startedAt, &createdAt, &b.MaxMessages, &b.MaxWall, &b.MaxMembers); err != nil {
		return b, err
	}
	if b.MaxMessages <= 0 {
		b.MaxMessages = DefaultMaxMessagesPerRound
	}
	if b.MaxWall <= 0 {
		b.MaxWall = DefaultMaxWallClockSeconds
	}
	b.MaxMembers = effectiveMaxMembers(b.MaxMembers)
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM loop_messages WHERE loop_id = ? AND status != ?`, loopID, MailMirrored,
	).Scan(&b.Messages); err != nil {
		return b, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM loop_members WHERE loop_id = ? AND status != ?`, loopID, LoopMemberDismissed,
	).Scan(&b.Members); err != nil {
		return b, err
	}
	// A start time of zero is a row nothing stamped. Measuring from the
	// epoch would call it decades over budget, so fall back to its creation.
	if startedAt <= 0 {
		startedAt = createdAt / 1000
	}
	if startedAt > 0 {
		b.WallClock = nowSec - startedAt
	}
	switch {
	case b.Messages > b.MaxMessages:
		b.Tripped, b.Reason = true, fmt.Sprintf("it carried %d messages, over its limit of %d", b.Messages, b.MaxMessages)
	case startedAt > 0 && b.WallClock > b.MaxWall:
		b.Tripped, b.Reason = true, fmt.Sprintf("it ran for %s, over its limit of %s",
			time.Duration(b.WallClock)*time.Second, time.Duration(b.MaxWall)*time.Second)
	case b.Members > b.MaxMembers:
		b.Tripped, b.Reason = true, fmt.Sprintf("it has %d members, over its limit of %d", b.Members, b.MaxMembers)
	}
	return b, nil
}

// EndLoop stops the work: queued and delivered messages are cancelled so a
// restarting agent cannot pick up stale work, and members are parked as idle
// unless dismiss retires them. Returns the number of messages cancelled.
//
// It only writes rows. Stopping the members' processes is the caller's job,
// because the store never sees a process.
func (s *Store) EndLoop(ctx context.Context, loopID, status, reason string, dismiss bool) (int64, error) {
	var cancelled int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		cancelled, err = endLoopTx(ctx, tx, loopID, status, reason, dismiss, time.Now().UnixMilli())
		return err
	})
	return cancelled, err
}

func endLoopTx(ctx context.Context, tx *sql.Tx, loopID, status, reason string, dismiss bool, now int64) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE loops
		SET status = ?, end_reason = ?, completed_at = ?, updated_at = ? WHERE id = ?`,
		status, reason, now, now, loopID); err != nil {
		return 0, err
	}
	parked := LoopMemberIdle
	if dismiss {
		parked = LoopMemberDismissed
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE loop_members SET status = ? WHERE loop_id = ? AND status = ?`,
		parked, loopID, LoopMemberActive); err != nil {
		return 0, err
	}
	if dismiss {
		if _, err := tx.ExecContext(ctx,
			`UPDATE loop_members SET status = ? WHERE loop_id = ? AND status = ?`,
			LoopMemberDismissed, loopID, LoopMemberIdle); err != nil {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE loop_messages SET status = ?
		WHERE loop_id = ? AND status IN (?, ?)`,
		MailCancelled, loopID, MailPending, MailDelivered)
	if err != nil {
		return 0, err
	}
	cancelled, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return cancelled, enqueueLoopEventTx(ctx, tx, LoopEvent{
		Kind: EvLoopEnded, LoopID: loopID,
		Content: "loop " + status + ": " + orDefault(reason, "no reason given"),
		Detail: map[string]any{
			"status": status, "reason": reason,
			"dismissed": dismiss, "cancelledMessages": cancelled,
		},
	}, now)
}

// FinishLoop ends a loop that agentd itself decided is over: status is
// LoopCapped when a limit tripped, or LoopComplete when its play finished.
// Unlike EndLoop it only acts on a loop that is still active, so a sweep
// that loses a race with the engineer ending the loop never overwrites what
// he chose. Reports whether this call ended it.
func (s *Store) FinishLoop(ctx context.Context, loopID, status, reason string) (bool, error) {
	var ended bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		res, err := tx.ExecContext(ctx,
			`UPDATE loops SET updated_at = ? WHERE id = ? AND status = ?`, now, loopID, LoopActive)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		ended = true
		if _, err := endLoopTx(ctx, tx, loopID, status, reason, false, now); err != nil {
			return err
		}
		if status != LoopCapped {
			return nil
		}
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvLoopCapped, LoopID: loopID,
			Content: "agentd ended this loop: " + reason,
			Detail:  map[string]any{"reason": reason},
		}, now)
	})
	return ended, err
}

// CapLoop ends an active loop as capped and records why.
func (s *Store) CapLoop(ctx context.Context, loopID, reason string) (bool, error) {
	return s.FinishLoop(ctx, loopID, LoopCapped, reason)
}

// PlayWrapUpGrace is how long an orchestrator has, after its play finishes,
// to tell the engineer what happened before the loop is ended for it.
const PlayWrapUpGrace = 10 * time.Minute

// LoopToFinish is an active loop that should end now, with the runs of the
// members whose processes have to stop with it.
type LoopToFinish struct {
	LoopID string
	Status string
	Reason string
}

// LoopsToFinish lists the active loops agentd should end now: those over a
// limit, and those whose play has finished.
//
// A finished play is not ended the instant it finishes. Its last step is the
// orchestrator acting on the result, and ending the loop at that moment would
// cancel the very brief that asks it to report and stop the process before it
// could. So it is ended once the orchestrator has posted anything since the
// play finished (its report to the engineer), or once PlayWrapUpGrace has
// passed without one.
func (s *Store) LoopsToFinish(ctx context.Context) ([]LoopToFinish, error) {
	return s.loopsToFinishAt(ctx, time.Now().UnixMilli())
}

func (s *Store) loopsToFinishAt(ctx context.Context, now int64) ([]LoopToFinish, error) {
	loops, err := s.ListLoops(ctx, LoopActive, 500)
	if err != nil {
		return nil, err
	}
	out := []LoopToFinish{}
	for _, l := range loops {
		b, err := s.budgetAt(ctx, l.ID, now/1000)
		if err != nil {
			return nil, err
		}
		if b.Tripped {
			out = append(out, LoopToFinish{LoopID: l.ID, Status: LoopCapped, Reason: b.Reason})
			continue
		}
		if l.Play == "" || l.PlayStatus == PlayRunning || l.PlayStatus == "" {
			continue
		}
		endedAt := l.PlayEndedAt
		if endedAt <= 0 {
			endedAt = l.UpdatedAt
		}
		reported := false
		if orch, err := s.LoopMemberByRole(ctx, l.ID, RoleOrchestrator); err != nil {
			return nil, err
		} else if orch != nil && orch.LastPostedAt > endedAt {
			reported = true
		}
		if !reported && now-endedAt < PlayWrapUpGrace.Milliseconds() {
			continue
		}
		status, reason := LoopComplete, "the play "+l.Play+" finished"
		if l.PlayStatus == PlayCapped {
			status, reason = LoopCapped, "the play "+l.Play+" ran out of rounds"
		}
		out = append(out, LoopToFinish{LoopID: l.ID, Status: status, Reason: reason})
	}
	return out, nil
}

// LiveMemberRunIDs lists the runs of a loop's members that may still have a
// process: members not dismissed, with a run, whose run is not known to have
// ended. It is what a caller stops when the loop, or the whole crew, goes.
func (s *Store) LiveMemberRunIDs(ctx context.Context, loopID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.run_id FROM loop_members m
		LEFT JOIN managed_sessions ms ON ms.id = m.run_id
		WHERE m.loop_id = ? AND m.run_id != '' AND m.status != ?
		  AND COALESCE(ms.state, '') NOT IN ('finished', 'stopped', 'crashed')
		ORDER BY m.seq`, loopID, LoopMemberDismissed)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RunIsLive reports whether a run has a managed row in a state that is not
// terminal. A run with no row at all is not live.
func (s *Store) RunIsLive(ctx context.Context, runID string) (bool, error) {
	if runID == "" {
		return false, nil
	}
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT state FROM managed_sessions WHERE id = ?`, runID).Scan(&state)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !terminalRunState(state), nil
}

// AddLoopMember staffs one role mid-loop and returns its plaintext token.
func (s *Store) AddLoopMember(ctx context.Context, loopID string, spec MemberSpec) (*LoopMember, string, error) {
	role := strings.ToUpper(strings.TrimSpace(spec.Role))
	if err := ValidateRole(role); err != nil {
		return nil, "", err
	}
	tool, model := NormaliseTool(spec.Tool), spec.Model
	if role == RoleEngineer {
		tool, model = "", ""
	} else if err := ValidateToolModel(tool, model); err != nil {
		return nil, "", err
	}
	poll := spec.PollIntervalSeconds
	if poll <= 0 {
		poll = DefaultPollIntervalSeconds
	}
	token, hash := "", ""
	if role != RoleEngineer {
		token = NewMailToken()
		hash = HashToken(token)
	}
	id := newMailID(strings.ToLower(role))
	now := time.Now().UnixMilli()
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		// Write first so the count below is read under the write lock and
		// two concurrent adds cannot both fit under the limit.
		if _, err := tx.ExecContext(ctx,
			`UPDATE loops SET updated_at = ? WHERE id = ?`, now, loopID); err != nil {
			return err
		}
		var members, max int
		if err := tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM loop_members WHERE loop_id = ? AND status != ?),
			max_members FROM loops WHERE id = ?`,
			loopID, LoopMemberDismissed, loopID).Scan(&members, &max); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: %s", ErrMailNotFound, loopID)
			}
			return err
		}
		if max = effectiveMaxMembers(max); members+1 > max {
			return fmt.Errorf("%w: the loop already has %d members and allows %d", ErrTooManyMembers, members, max)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO loop_members (`+mailMemberColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, loopID, role, hash, LoopMemberActive, poll, 0, now, 0, tool, model,
			spec.RunID, "", "", 0, 0, 0); err != nil {
			return err
		}
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvMemberChanged, LoopID: loopID,
			Content: role + " joined the loop",
			Detail:  map[string]any{"role": role, "status": LoopMemberActive, "tool": tool, "model": model},
		}, now)
	}); err != nil {
		return nil, "", err
	}
	m, err := s.LoopMemberByID(ctx, id)
	return m, token, err
}

// ListLoopMembers returns a loop's roster oldest-first.
func (s *Store) ListLoopMembers(ctx context.Context, loopID string) ([]*LoopMember, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mailMemberColumns+`
		FROM loop_members WHERE loop_id = ? ORDER BY seq`, loopID)
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

// LoopMemberByID fetches one member (nil when absent).
func (s *Store) LoopMemberByID(ctx context.Context, id string) (*LoopMember, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+mailMemberColumns+` FROM loop_members WHERE id = ?`, id)
	m, err := scanLoopMember(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// LoopMemberByRole resolves one live member of a loop by its role.
//
// The tx-scoped twin has existed since the stranded announcement needed it;
// this is the same read for callers outside a transaction, which the report
// guarantee is.
func (s *Store) LoopMemberByRole(ctx context.Context, loopID, role string) (*LoopMember, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+mailMemberColumns+` FROM loop_members
		WHERE loop_id = ? AND role = ? AND status != ?`, loopID, role, LoopMemberDismissed)
	m, err := scanLoopMember(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// LoopMemberByToken authenticates a member by hash and stamps last_seen_at.
// An empty token never matches.
func (s *Store) LoopMemberByToken(ctx context.Context, token string) (*LoopMember, *Loop, error) {
	if token == "" {
		return nil, nil, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+mailMemberColumns+`
		FROM loop_members WHERE token_hash = ?`, HashToken(token))
	m, err := scanLoopMember(row)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET last_seen_at = ? WHERE id = ?`, now, m.ID); err != nil {
		return nil, nil, err
	}
	m.LastSeenAt = now
	l, err := s.GetLoop(ctx, m.LoopID)
	return m, l, err
}

// LoopMemberByRunID finds the member a spawned run belongs to, and its loop.
//
// The run is the link and it exists before the process does, which is why the
// join is on run_id and not session_id. Without it a member's terminal knew
// only its run id: the member name lived in memory, in the --name argv and in
// the corpus title record, and managed_sessions has no title column, so every
// member session was headed "Terminal session".
//
// Returns nil, nil for a run that is not a member of anything, which is the
// ordinary case for a session started from the run form.
func (s *Store) LoopMemberByRunID(ctx context.Context, runID string) (*LoopMember, *Loop, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, nil, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+mailMemberColumns+` FROM loop_members WHERE run_id = ? ORDER BY seq DESC LIMIT 1`, runID)
	m, err := scanLoopMember(row)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	l, err := s.GetLoop(ctx, m.LoopID)
	if err != nil || l == nil {
		return nil, nil, err
	}
	return m, l, nil
}

// ResolveLoopRecipient finds a live member of one loop by role or by id.
func (s *Store) ResolveLoopRecipient(ctx context.Context, loopID, to string) (*LoopMember, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+mailMemberColumns+` FROM loop_members
		WHERE loop_id = ? AND (id = ? OR role = ?) AND status != ?`,
		loopID, to, strings.ToUpper(strings.TrimSpace(to)), LoopMemberDismissed)
	m, err := scanLoopMember(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// RotateLoopMemberToken issues a new credential for the same member id, so a
// fresh session inherits the notes and the thread. The old token dies at once.
func (s *Store) RotateLoopMemberToken(ctx context.Context, memberID string) (string, error) {
	token := NewMailToken()
	res, err := s.db.ExecContext(ctx,
		`UPDATE loop_members SET token_hash = ?, status = ? WHERE id = ?`,
		HashToken(token), LoopMemberActive, memberID)
	if err != nil {
		return "", err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return "", fmt.Errorf("store: no mail member %s", memberID)
	}
	return token, nil
}

// SetLoopMemberStatus moves one role through the lifecycle.
func (s *Store) SetLoopMemberStatus(ctx context.Context, loopID, role, status string) (*LoopMember, error) {
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE loop_members SET status = ? WHERE loop_id = ? AND role = ?`,
			status, loopID, strings.ToUpper(role)); err != nil {
			return err
		}
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvMemberChanged, LoopID: loopID,
			Content: strings.ToUpper(role) + " is now " + status,
			Detail:  map[string]any{"role": strings.ToUpper(role), "status": status},
		}, time.Now().UnixMilli())
	}); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+mailMemberColumns+`
		FROM loop_members WHERE loop_id = ? AND role = ?`, loopID, strings.ToUpper(role))
	m, err := scanLoopMember(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// RetireLoopMember dismisses one role and takes it out of the play, in one
// transaction: the member row flips to dismissed, the role leaves the current
// step's awaiting list, and every message still pending for it or held by it
// is cancelled.
//
// Flipping the status alone left the step waiting for a member that would
// never answer, and left its held brief counted as work in progress forever.
// Returns the member as it now stands, or nil when there is no such role.
func (s *Store) RetireLoopMember(ctx context.Context, loopID, role string) (*LoopMember, error) {
	role = strings.ToUpper(strings.TrimSpace(role))
	var member *LoopMember
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		res, err := tx.ExecContext(ctx, `UPDATE loop_members SET status = ?
			WHERE loop_id = ? AND role = ? AND status != ?`,
			LoopMemberDismissed, loopID, role, LoopMemberDismissed)
		if err != nil {
			return err
		}
		flipped, err := res.RowsAffected()
		if err != nil {
			return err
		}
		m, err := scanLoopMember(tx.QueryRowContext(ctx, `SELECT `+mailMemberColumns+`
			FROM loop_members WHERE loop_id = ? AND role = ? ORDER BY seq DESC LIMIT 1`, loopID, role))
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		member = m
		if flipped == 0 {
			return nil // already retired; nothing else to undo
		}
		if err := removeFromAwaitingTx(ctx, tx, loopID, map[string]bool{role: true}); err != nil {
			return err
		}
		cancelled, err := cancelMailForTx(ctx, tx, []string{m.ID})
		if err != nil {
			return err
		}
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvMemberChanged, LoopID: loopID,
			Content: role + " is now " + LoopMemberDismissed,
			Detail: map[string]any{
				"role": role, "status": LoopMemberDismissed, "cancelledMessages": cancelled,
			},
		}, now)
	})
	return member, err
}

// DismissLoopMembers retires every live member of a loop. The members being
// dismissed are read BEFORE the flip, so they are the ones taken out of the
// play's awaiting list and whose pending and held mail is cancelled; reading
// them afterwards picked up whoever had been dismissed earlier instead.
func (s *Store) DismissLoopMembers(ctx context.Context, loopID string) (int64, error) {
	var affected int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		// Write first, to take the write lock before reading the roster.
		if _, err := tx.ExecContext(ctx,
			`UPDATE loops SET updated_at = ? WHERE id = ?`, now, loopID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, role FROM loop_members
			WHERE loop_id = ? AND status IN (?, ?)`, loopID, LoopMemberActive, LoopMemberIdle)
		if err != nil {
			return err
		}
		ids := []string{}
		roles := map[string]bool{}
		for rows.Next() {
			var id, role string
			if err := rows.Scan(&id, &role); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
			roles[strings.ToUpper(role)] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		if len(ids) == 0 {
			return nil
		}
		if err := removeFromAwaitingTx(ctx, tx, loopID, roles); err != nil {
			return err
		}
		if _, err := cancelMailForTx(ctx, tx, ids); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE loop_members SET status = ?
			 WHERE loop_id = ? AND status IN (?, ?)`,
			LoopMemberDismissed, loopID, LoopMemberActive, LoopMemberIdle)
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvMemberChanged, LoopID: loopID,
			Content: "every member was dismissed",
			Detail:  map[string]any{"status": LoopMemberDismissed, "count": affected},
		}, now)
	})
	return affected, err
}

// removeFromAwaitingTx takes roles out of the loop's current step.
func removeFromAwaitingTx(ctx context.Context, tx *sql.Tx, loopID string, roles map[string]bool) error {
	var awaiting string
	err := tx.QueryRowContext(ctx, `SELECT step_awaiting FROM loops WHERE id = ?`, loopID).Scan(&awaiting)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	current := decodeAwaiting(awaiting)
	kept := make([]string, 0, len(current))
	for _, r := range current {
		if !roles[strings.ToUpper(r)] {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(current) {
		return nil
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE loops SET step_awaiting = ? WHERE id = ?`, encodeAwaiting(kept), loopID)
	return err
}

// cancelMailForTx cancels every pending and held message addressed to the
// given members. Returns how many were cancelled.
func cancelMailForTx(ctx context.Context, tx *sql.Tx, memberIDs []string) (int64, error) {
	if len(memberIDs) == 0 {
		return 0, nil
	}
	args := []any{MailCancelled, MailPending, MailDelivered}
	for _, id := range memberIDs {
		args = append(args, id)
	}
	//nolint:gosec // G202: the concatenated fragment is a generated "?,?" list, never input
	res, err := tx.ExecContext(ctx, `UPDATE loop_messages SET status = ?
		WHERE status IN (?, ?) AND recipient_id IN (`+placeholders(len(memberIDs))+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AdoptLoopMembers moves the previous loop's live members into a successor,
// keeping their ids and tokens. Dismissed members are left behind and the
// successor's placeholder rows for the adopted roles are removed. Returns the
// adopted roles.
func (s *Store) AdoptLoopMembers(ctx context.Context, fromLoopID, toLoopID string) ([]string, error) {
	var roles []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		roles, err = adoptLoopMembersTx(ctx, tx, fromLoopID, toLoopID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return roles, nil
}

func adoptLoopMembersTx(ctx context.Context, tx *sql.Tx, fromLoopID, toLoopID string) ([]string, error) {
	roles := []string{}
	rows, err := tx.QueryContext(ctx, `SELECT id, role FROM loop_members
		WHERE loop_id = ? AND status IN (?, ?)`,
		fromLoopID, LoopMemberActive, LoopMemberIdle)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return roles, nil
	}
	args := []any{toLoopID}
	for _, r := range roles {
		args = append(args, r)
	}
	//nolint:gosec // G202: the concatenated fragment is a generated "?,?" list, never input
	if _, err := tx.ExecContext(ctx, `DELETE FROM loop_members
		WHERE loop_id = ? AND role IN (`+placeholders(len(roles))+`)`, args...); err != nil {
		return nil, err
	}
	args = []any{toLoopID, LoopMemberActive}
	for _, id := range ids {
		args = append(args, id)
	}
	//nolint:gosec // G202: same generated placeholder list, never input
	_, err = tx.ExecContext(ctx, `UPDATE loop_members SET loop_id = ?, status = ?
		WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	return roles, err
}

// ReassignLoopMemberRun gives a member a fresh run and a fresh token, for a
// member whose process is gone and is about to be started again. The old
// token dies at once, like a rotation; the session id and last error belong
// to the old process and are cleared.
func (s *Store) ReassignLoopMemberRun(ctx context.Context, memberID, runID string) (string, error) {
	token := NewMailToken()
	res, err := s.db.ExecContext(ctx, `UPDATE loop_members
		SET token_hash = ?, status = ?, run_id = ?, session_id = '', last_error = '', stranded_since = 0
		WHERE id = ?`, HashToken(token), LoopMemberActive, runID, memberID)
	if err != nil {
		return "", err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return "", fmt.Errorf("%w: no member %s", ErrMailNotFound, memberID)
	}
	return token, nil
}

// PostAdvisory puts a message from ENGINE into a member's inbox.
//
// It is not a post from anyone in the loop: it never passes step enforcement,
// never advances the play and never discharges anybody's brief. That is the
// point. It is how agentd tells the orchestrator something it noticed (a
// member's output that was never sent, say) without pretending the member
// said it.
func (s *Store) PostAdvisory(ctx context.Context, loopID, toRole, subject, body string, detail map[string]any) (*LoopMessage, error) {
	var msg *LoopMessage
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM loops WHERE id = ?`, loopID).Scan(&status); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: %s", ErrMailNotFound, loopID)
			}
			return err
		}
		if (&Loop{Status: status}).Ended() {
			return fmt.Errorf("%w: %s is %s", ErrLoopEnded, loopID, status)
		}
		to, err := txMemberByRole(ctx, tx, loopID, strings.ToUpper(toRole))
		if err != nil {
			return err
		}
		if to == nil {
			return fmt.Errorf("%w: no member %s in %s", ErrMailNotFound, toRole, loopID)
		}
		msg = &LoopMessage{
			ID: newMailID("msg"), LoopID: loopID, SenderRole: RoleEngine,
			RecipientID: to.ID, RecipientRole: to.Role, Subject: subject, Body: body,
			BodyChars: utf8.RuneCountInString(body), Status: MailPending, CreatedAt: now,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO loop_messages (`+mailMessageColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			msg.ID, loopID, "", RoleEngine, to.ID, to.Role, subject, body, MailPending,
			0, 0, "", now, 0, 0, "", "", "", ""); err != nil {
			return err
		}
		if detail == nil {
			detail = map[string]any{}
		}
		detail["messageId"], detail["toRole"] = msg.ID, to.Role
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvAutoForwarded, LoopID: loopID, Content: subject, Detail: detail,
		}, now)
	})
	if err != nil {
		return nil, err
	}
	s.notifyMail(msg.RecipientID)
	return msg, nil
}

// LoopMemberSignal is what a poller is told about its own standing: dismissed
// members stop, members of an ended loop go idle with a live token, everyone
// else keeps working.
func LoopMemberSignal(m *LoopMember, l *Loop) (stop, idle bool, reason string) {
	switch {
	case m == nil || m.Status == LoopMemberDismissed:
		return true, false, "You have been dismissed. Exit and stop polling."
	case l != nil && l.Ended():
		return false, true, fmt.Sprintf("This loop is %s (%s). Keep waiting: you stay assigned and your inbox reopens when the engineer starts the next loop.",
			l.Status, orDefault(l.EndReason, "no reason given"))
	default:
		return false, false, ""
	}
}

// MarkLoopMemberStarted records the engine session a spawned member landed on
// and clears any previous failure. The UPDATE is keyed by RUN id, not role:
// the run is what actually started, so a role whose run was replaced cannot
// have a stale spawn stamped onto it.
//
// It emits an event for the same reason MarkLoopMemberFailed does, and the
// asymmetry it fixes was worse than a missing feature: a member that FAILED
// updated the board instantly while four members that started perfectly
// changed nothing on screen at all, so the happy path was the one that looked
// broken.
func (s *Store) MarkLoopMemberStarted(ctx context.Context, loopID, role, runID, sessionID string) error {
	if runID == "" {
		return nil
	}
	now := time.Now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE loop_members SET session_id = ?, last_error = '' WHERE run_id = ?`,
			sessionID, runID)
		if err != nil {
			return err
		}
		// No row means this run is not a member of anything any more, and an
		// event about a member that is not there would be a lie.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return nil
		}
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvMemberChanged, LoopID: loopID,
			Content: role + " started",
			Detail:  map[string]any{"role": role, "runId": runID, "sessionId": sessionID},
		}, now)
	})
}

// MarkLoopMemberFailed records why a member has no process, on the row and on
// the loop's event stream in one transaction. It deliberately does NOT change
// the member's status: the token is still valid and the role is still on the
// roster, which is exactly the state a failed spawn leaves the engineer in.
func (s *Store) MarkLoopMemberFailed(ctx context.Context, loopID, role, reason string) error {
	now := time.Now().UnixMilli()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE loop_members SET last_error = ? WHERE loop_id = ? AND role = ?`,
			reason, loopID, role); err != nil {
			return err
		}
		return enqueueLoopEventTx(ctx, tx, LoopEvent{
			Kind: EvMemberChanged, LoopID: loopID,
			Content: role + " did not start: " + reason,
			Detail:  map[string]any{"role": role, "lastError": reason},
		}, now)
	})
}

// AppendLoopNote adds one durable conclusion to a role's memory.
func (s *Store) AppendLoopNote(ctx context.Context, loopID, role, authorID, body string) (*LoopNote, error) {
	n := &LoopNote{
		ID:        newMailID("note"),
		LoopID:    loopID,
		Role:      strings.ToUpper(role),
		AuthorID:  authorID,
		Body:      body,
		CreatedAt: time.Now().UnixMilli(),
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO loop_notes (id, loop_id, role, author_id, body, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, n.ID, n.LoopID, n.Role, n.AuthorID, n.Body, n.CreatedAt)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// LoopNotes returns a loop's notes oldest-first, optionally for one role.
func (s *Store) LoopNotes(ctx context.Context, loopID, role string, limit int) ([]*LoopNote, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT id, loop_id, role, author_id, body, created_at FROM loop_notes WHERE loop_id = ?`
	args := []any{loopID}
	if role != "" {
		q += ` AND role = ?`
		args = append(args, strings.ToUpper(role))
	}
	q += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*LoopNote{}
	for rows.Next() {
		n := &LoopNote{}
		if err := rows.Scan(&n.ID, &n.LoopID, &n.Role, &n.AuthorID, &n.Body, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountLoopNotes counts one role's notes.
func (s *Store) CountLoopNotes(ctx context.Context, loopID, role string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM loop_notes WHERE loop_id = ? AND role = ?`,
		loopID, strings.ToUpper(role)).Scan(&n)
	return n, err
}

// LoopNoteCounts returns each role's note count in one query, for the roster
// view.
func (s *Store) LoopNoteCounts(ctx context.Context, loopID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, COUNT(*) FROM loop_notes WHERE loop_id = ? GROUP BY role`, loopID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var role string
		var n int
		if err := rows.Scan(&role, &n); err != nil {
			return nil, err
		}
		out[role] = n
	}
	return out, rows.Err()
}

// CopyLoopNotes seeds a successor loop with the parent's role memory, marking
// where each note came from.
func (s *Store) CopyLoopNotes(ctx context.Context, fromLoopID, toLoopID string) (int, error) {
	var n int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = copyLoopNotesTx(ctx, tx, fromLoopID, toLoopID)
		return err
	})
	return n, err
}

func copyLoopNotesTx(ctx context.Context, tx *sql.Tx, fromLoopID, toLoopID string) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT role, author_id, body, created_at FROM loop_notes
		WHERE loop_id = ? ORDER BY seq LIMIT 1000`, fromLoopID)
	if err != nil {
		return 0, err
	}
	type note struct {
		role, author, body string
		at                 int64
	}
	var src []note
	for rows.Next() {
		var n note
		if err := rows.Scan(&n.role, &n.author, &n.body, &n.at); err != nil {
			_ = rows.Close()
			return 0, err
		}
		src = append(src, n)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()
	for _, n := range src {
		if _, err := tx.ExecContext(ctx, `INSERT INTO loop_notes
			(id, loop_id, role, author_id, body, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			newMailID("note"), toLoopID, n.role, n.author,
			fmt.Sprintf("[carried over from %s] %s", fromLoopID, n.body), n.at); err != nil {
			return 0, err
		}
	}
	return len(src), nil
}

// PurgeLoop deletes one ended loop with its members, messages and notes.
func (s *Store) PurgeLoop(ctx context.Context, loopID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM loop_notes WHERE loop_id = ?`,
			`DELETE FROM loop_messages WHERE loop_id = ?`,
			`DELETE FROM loop_members WHERE loop_id = ?`,
			`DELETE FROM loops WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, loopID); err != nil {
				return err
			}
		}
		return nil
	})
}

func scanLoop(sc interface{ Scan(dest ...any) error }) (*Loop, error) {
	l := &Loop{}
	var awaiting string
	err := sc.Scan(&l.ID, &l.Title, &l.Task, &l.CWD, &l.Status, &l.Round, &l.State, &l.ParentLoopID,
		&l.PollIntervalSeconds, &l.EndReason, &l.CreatedAt, &l.UpdatedAt, &l.CompletedAt,
		&l.Play, &l.PlayStatus, &l.StepID, &awaiting, &l.StepEnteredAt, &l.Generation,
		&l.MaxMessagesPerRound, &l.MaxWallClockSeconds, &l.MaxMembers, &l.StartedAt, &l.PlayEndedAt)
	l.StepAwaiting = decodeAwaiting(awaiting)
	return l, err
}

func scanLoopMember(sc interface{ Scan(dest ...any) error }) (*LoopMember, error) {
	m := &LoopMember{}
	err := sc.Scan(&m.ID, &m.LoopID, &m.Role, &m.TokenHash, &m.Status,
		&m.PollIntervalSeconds, &m.CharsIn, &m.CreatedAt, &m.LastSeenAt, &m.Tool, &m.Model,
		&m.RunID, &m.SessionID, &m.LastError, &m.StrandedSince, &m.LastPostedAt,
		&m.LastPolledAt)
	return m, err
}

func placeholders(n int) string {
	if n <= 0 {
		return "''"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
