package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ManagedSession is the agentd-owned record of a spawned claude session
// (migration 0004). It is the source of truth for the spawner state machine
// and what the relay reports to the live server fleet registry.
type ManagedSession struct {
	ID               string  `json:"id"`                  // agentd run key
	SessionID        string  `json:"sessionId,omitempty"` // claude session id (from system/init)
	Kind             string  `json:"kind"`                // one_shot | chat
	CWD              string  `json:"cwd,omitempty"`
	Project          string  `json:"project,omitempty"`
	Model            string  `json:"model,omitempty"`
	Prompt           string  `json:"prompt,omitempty"`
	ResumeFrom       string  `json:"resumeFrom,omitempty"`
	State            string  `json:"state"` // starting | running | awaiting | finished | stopped | crashed
	PID              int     `json:"pid,omitempty"`
	StartedAt        int64   `json:"startedAt,omitempty"`
	EndedAt          int64   `json:"endedAt,omitempty"`
	ExitCode         int     `json:"exitCode,omitempty"`
	EventCount       int64   `json:"eventCount"`
	PermissionDenial int64   `json:"permissionDenials,omitempty"`
	TotalCostUSD     float64 `json:"totalCostUsd,omitempty"`
	TerminalReason   string  `json:"terminalReason,omitempty"`
	LastError        string  `json:"lastError,omitempty"`
	CreatedBy        string  `json:"createdBy,omitempty"`
	UpdatedAt        int64   `json:"updatedAt,omitempty"`
	// ApprovalsEnabled: whether this run's tool calls
	// route to the remote-approval hook.
	ApprovalsEnabled bool `json:"approvalsEnabled,omitempty"`
	// Pgid is the OS process group id of the spawned claude (the kill
	// unit is the group). Set when the spawner registers the process.
	Pgid int `json:"pgid,omitempty"`
	// ProcStartTicks is /proc/<pid>/stat field 22 captured at spawn time.
	// The boot reconciler uses it to identity-check before signaling on a
	// pre-restart run, so a recycled PID can never kill the wrong process.
	ProcStartTicks int64 `json:"procStartTicks,omitempty"`
	// Generation is the agentd boot id that spawned this run. Every
	// active row whose generation != current is treated by the reconciler
	// as an orphan (its supervisor is dead) and its parent loop is
	// marked `interrupted`.
	Generation string `json:"generation,omitempty"`
	// Title is the name the run was given at spawn. Engine-agnostic and
	// available immediately, unlike a transcript-derived title.
	Title string `json:"title,omitempty"`
	// StopReason records WHY the process left the running state: user_cancel
	// / user_pause / user_stop / member_kill / orphaned (reconciler) /
	// superseded (session-id reused by a fresh spawn). Empty while alive.
	StopReason string `json:"stopReason,omitempty"`
	// Engine is the dual-engine identity (migration 0015): "claude" or
	// "opencode". Engine is a property of the session, not a UI mode.
	// Empty values are normalised to "claude" at write time.
	Engine string `json:"engine,omitempty"`
	// ControlPort is the local loopback port the OpenCode TUI serves its
	// control API on. 0 for claude runs and for OpenCode runs
	// whose control API is unavailable.
	ControlPort int `json:"controlPort,omitempty"`
	// ControlBase is the full base URL the control client should use, e.g.
	// "http://127.0.0.1:47312". Empty when ControlPort is 0.
	ControlBase string `json:"controlBase,omitempty"`
}

// UpsertManagedSession inserts or replaces a managed session row.
func (s *Store) UpsertManagedSession(ctx context.Context, m *ManagedSession) error {
	if m.UpdatedAt == 0 {
		m.UpdatedAt = time.Now().UnixMilli()
	}
	aen := 0
	if m.ApprovalsEnabled {
		aen = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO managed_sessions (
		id, session_id, kind, cwd, project, model, prompt, resume_from, state, pid,
		started_at, ended_at, exit_code, event_count, permission_denials,
		total_cost_usd, terminal_reason, last_error, created_by, updated_at,
		approvals_enabled, pgid, proc_start_ticks, generation, stop_reason,
		engine, control_port, control_base, title)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (id) DO UPDATE SET
		-- A writer that does not know the session id must not erase it. The
		-- OpenCode id arrives through the control API after the TUI is up,
		-- and a status persist rebuilt from a snapshot taken before that
		-- used to blank it, so the run lost its session at exit.
		session_id = COALESCE(NULLIF(excluded.session_id, ''), managed_sessions.session_id),
		kind = excluded.kind,
		cwd = excluded.cwd,
		project = excluded.project,
		model = excluded.model,
		prompt = excluded.prompt,
		resume_from = excluded.resume_from,
		state = excluded.state,
		pid = excluded.pid,
		started_at = excluded.started_at,
		ended_at = excluded.ended_at,
		exit_code = excluded.exit_code,
		event_count = excluded.event_count,
		permission_denials = excluded.permission_denials,
		approvals_enabled = excluded.approvals_enabled,
		total_cost_usd = excluded.total_cost_usd,
		terminal_reason = excluded.terminal_reason,
		last_error = excluded.last_error,
		created_by = excluded.created_by,
		updated_at = excluded.updated_at,
		-- Kill identity is PRESERVED when the writer does
		-- not carry it, instead of being blanked.
		--
		-- Only the spawn path knows the generation, the pgid and the start
		-- ticks; every other writer rebuilds the row from a spawner.Session,
		-- which has no field for any of them. Assigning excluded.* blindly
		-- meant the first status persist after spawn wiped the stamp off a
		-- live run, and a blank generation is indistinguishable from "never
		-- stamped" — so the boot reconciler could never tell a run from a
		-- previous agentd life from a current one. A writer that does not
		-- know these values must not claim they are empty.
		--
		-- An explicit non-empty value still wins, which is what lets the
		-- spawn path stamp and terminate() record its reason.
		pgid = CASE WHEN excluded.pgid != 0
			THEN excluded.pgid ELSE COALESCE(managed_sessions.pgid, 0) END,
		proc_start_ticks = CASE WHEN excluded.proc_start_ticks != 0
			THEN excluded.proc_start_ticks ELSE COALESCE(managed_sessions.proc_start_ticks, 0) END,
		generation = CASE WHEN excluded.generation != ''
			THEN excluded.generation ELSE COALESCE(managed_sessions.generation, '') END,
		-- stop_reason describes why a run was STOPPED, so it only means
		-- anything on a terminal row. Preserving it unconditionally would
		-- carry it onto a live one: a tty restart reuses the same run id
		-- (ensureTTY -> StartTTY with the existing key), so the row would
		-- come back live still wearing the reason it died of last time,
		-- and mailapi reports that field. An explicit reason wins; a
		-- terminal row keeps what it has; a row being written as live
		-- clears it.
		stop_reason = CASE
			WHEN excluded.stop_reason != '' THEN excluded.stop_reason
			WHEN excluded.state IN ('finished', 'stopped', 'crashed')
				THEN COALESCE(managed_sessions.stop_reason, '')
			ELSE '' END,
		engine = excluded.engine,
		control_port = excluded.control_port,
		control_base = excluded.control_base,
		-- Same preserve rule as the kill identity: a writer that does not
		-- carry the title must not blank the one the spawn set. Every
		-- status persist rebuilds the row from a struct that may not know
		-- it, and the name is not theirs to erase.
		title = CASE WHEN excluded.title != ''
			THEN excluded.title ELSE COALESCE(managed_sessions.title, '') END`,
		m.ID, m.SessionID, m.Kind, m.CWD, m.Project, m.Model, m.Prompt, m.ResumeFrom, m.State,
		m.PID, m.StartedAt, m.EndedAt, m.ExitCode, m.EventCount, m.PermissionDenial,
		m.TotalCostUSD, m.TerminalReason, m.LastError, m.CreatedBy, m.UpdatedAt, aen,
		m.Pgid, m.ProcStartTicks, m.Generation, m.StopReason,
		normaliseEngine(m.Engine), m.ControlPort, m.ControlBase, m.Title)
	return err
}

// ListActiveManagedSessionsNotInGeneration returns every non-terminal
// managed row whose generation is not gen: the runs a PREVIOUS agentd life
// left behind. The boot reconciler reaps these before
// anything serves.
//
// An empty generation counts as foreign. Rows written before the stamp
// existed have one, and so does anything a writer blanked — there is no way
// to tell "never stamped" from "stamped by a life we cannot name", and both
// are orphans. Passing an empty gen would therefore match every stamped row
// and nothing else, which is never what a caller wants; it is rejected.
func (s *Store) ListActiveManagedSessionsNotInGeneration(ctx context.Context, gen string) ([]*ManagedSession, error) {
	if gen == "" {
		return nil, errors.New("store: refusing to reap against an empty generation")
	}
	q := `SELECT id, session_id, kind, cwd, project, model, prompt, resume_from, state, pid,
		started_at, ended_at, exit_code, event_count, permission_denials,
		total_cost_usd, terminal_reason, last_error, created_by, updated_at,
		approvals_enabled, pgid, proc_start_ticks, generation, stop_reason,
		engine, control_port, control_base, title
	FROM managed_sessions
	WHERE state NOT IN ('finished', 'stopped', 'crashed')
	  AND COALESCE(generation, '') != ?
	ORDER BY started_at ASC LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q, gen)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*ManagedSession{}
	for rows.Next() {
		m, err := scanManagedSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetManagedSession returns one managed session by id (nil when absent).
func (s *Store) GetManagedSession(ctx context.Context, id string) (*ManagedSession, error) {
	return s.getManagedSessionBy(ctx, `id = ?`, id)
}

// GetManagedSessionBySessionID resolves a managed run by its claude session id.
//
// One session can have had several runs (every resume is a new one), so the
// answer is ordered: a run still alive wins, then the most recently updated.
// Without an order SQLite returned whichever row it met first, which could be
// a long-dead run while a live one held the same session.
func (s *Store) GetManagedSessionBySessionID(ctx context.Context, sid string) (*ManagedSession, error) {
	return s.getManagedSessionBy(ctx, `session_id = ?
		ORDER BY CASE WHEN state IN ('finished', 'stopped', 'crashed') THEN 1 ELSE 0 END, updated_at DESC
		LIMIT 1`, sid)
}

func (s *Store) getManagedSessionBy(ctx context.Context, where, arg string) (*ManagedSession, error) {
	// nolint:gosec // G202: `where` is one of the two literal clauses above, never user input.
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, kind, cwd, project, model, prompt,
		resume_from, state, pid, started_at, ended_at, exit_code, event_count,
		permission_denials, total_cost_usd, terminal_reason, last_error, created_by, updated_at,
		approvals_enabled, pgid, proc_start_ticks, generation, stop_reason,
		engine, control_port, control_base, title
	FROM managed_sessions WHERE `+where, arg)
	m, err := scanManagedSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// ListManagedSessions returns managed sessions ordered by most recent update.
func (s *Store) ListManagedSessions(ctx context.Context, state string, limit int) ([]*ManagedSession, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, session_id, kind, cwd, project, model, prompt, resume_from, state, pid,
		started_at, ended_at, exit_code, event_count, permission_denials,
		total_cost_usd, terminal_reason, last_error, created_by, updated_at,
		approvals_enabled, pgid, proc_start_ticks, generation, stop_reason,
		engine, control_port, control_base, title
	FROM managed_sessions`
	args := []any{}
	if state != "" {
		q += ` WHERE state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*ManagedSession{}
	for rows.Next() {
		m, err := scanManagedSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteManagedSession removes a managed session record.
func (s *Store) DeleteManagedSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM managed_sessions WHERE id = ?`, id)
	return err
}

// ListClaimedSessionIDs returns every claude session id already bound to a
// managed run. The loop engine seeds member discovery exclusions with this:
// a spawning member must never claim a transcript another run already owns.
func (s *Store) ListClaimedSessionIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT session_id FROM managed_sessions WHERE session_id <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		out[sid] = true
	}
	return out, rows.Err()
}

func scanManagedSession(sc interface {
	Scan(dest ...any) error
}) (*ManagedSession, error) {
	m := &ManagedSession{}
	var aen int
	err := sc.Scan(&m.ID, &m.SessionID, &m.Kind, &m.CWD, &m.Project, &m.Model, &m.Prompt,
		&m.ResumeFrom, &m.State, &m.PID, &m.StartedAt, &m.EndedAt, &m.ExitCode,
		&m.EventCount, &m.PermissionDenial, &m.TotalCostUSD, &m.TerminalReason,
		&m.LastError, &m.CreatedBy, &m.UpdatedAt, &aen,
		&m.Pgid, &m.ProcStartTicks, &m.Generation, &m.StopReason,
		&m.Engine, &m.ControlPort, &m.ControlBase, &m.Title)
	m.ApprovalsEnabled = aen == 1
	m.Engine = normaliseEngine(m.Engine)
	return m, err
}

// normaliseEngine maps empty / unknown engine ids to "claude" so every read
// path sees the same default.
func normaliseEngine(s string) string {
	switch s {
	case "opencode":
		return "opencode"
	default:
		return "claude"
	}
}

// SetManagedSessionError records why a run is not fully working, without
// touching anything else about it.
//
// An OpenCode run whose control API never bound is still a usable TERMINAL —
// the TUI is up and the engineer can type into it — so its state is not a
// lie and must not be rewritten to "crashed". What was missing is any trace
// of the failure: the row sat in "starting" with an empty session id and the
// only evidence was a log line nobody was reading. Now the row says so.
func (s *Store) SetManagedSessionError(ctx context.Context, id, lastErr string) error {
	if id == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE managed_sessions SET last_error = ?, updated_at = ? WHERE id = ?`,
		lastErr, time.Now().UnixMilli(), id)
	return err
}

// SetManagedStopReason records why a run that has already reached a terminal
// state was stopped. The pump writes the final row, and whoever asked for
// the stop adds the reason afterwards without rewriting anything else. A row
// that is somehow still live is left alone: a reason on a live row is a lie.
func (s *Store) SetManagedStopReason(ctx context.Context, id, reason string) error {
	if id == "" || reason == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE managed_sessions SET stop_reason = ?
		WHERE id = ? AND state IN ('finished', 'stopped', 'crashed')`, reason, id)
	return err
}

// SetManagedSessionID records a run's engine session id, for a run with no
// live process in this agentd to update in memory.
func (s *Store) SetManagedSessionID(ctx context.Context, id, sessionID string) error {
	if id == "" || sessionID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE managed_sessions SET session_id = ?, updated_at = ? WHERE id = ?`,
		sessionID, time.Now().UnixMilli(), id)
	return err
}

// SetManagedSessionModel writes ONLY the model.
//
// It exists because the alternative is an upsert of the whole row from an
// in-memory Session, and that Session does not carry Pgid, ProcStartTicks,
// Generation or StopReason: every such write silently zeroed the kill
// identity of a LIVE process, so a later stop could no longer prove it was
// killing the right one. A narrow UPDATE cannot do that by construction,
// which is a stronger guarantee than remembering four more fields.
func (s *Store) SetManagedSessionModel(ctx context.Context, id, model string) error {
	if id == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE managed_sessions SET model = ?, updated_at = ? WHERE id = ?`,
		model, time.Now().UnixMilli(), id)
	return err
}
