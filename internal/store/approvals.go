package store

import (
	"context"
	"database/sql"
	"time"
)

// Approval states. Rows are never deleted: the table is the audit
// trail; `state` moves pending → approved/denied/expired/cancelled.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
	ApprovalCollided = "collided" // collapsed into another pending approval for the same (run,tool)
)

// Approval is one PreToolUse approval request/audit row (migration 0006).
// ToolInput is a bounded preview + Hash per the blob policy (no secrets
// verbatim in the trail).
type Approval struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionId,omitempty"`
	RunID      string `json:"runId,omitempty"`
	MissionID  string `json:"missionId,omitempty"`
	ToolUseID  string `json:"toolUseId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	ToolInput  string `json:"toolInput,omitempty"`
	InputHash  string `json:"inputHash,omitempty"`
	Project    string `json:"project,omitempty"`
	CWD        string `json:"cwd,omitempty"`
	Routed     bool   `json:"routed,omitempty"`
	State      string `json:"state"`
	DecisionBy string `json:"decisionBy,omitempty"` // policy:<rule> | device:<id> | timeout | hook
	Collided   bool   `json:"collided,omitempty"`
	CreatedAt  int64  `json:"createdAt,omitempty"`
	ExpiresAt  int64  `json:"expiresAt,omitempty"`
	DecidedAt  int64  `json:"decidedAt,omitempty"`
}

// ApprovalPolicy is one standing rule.
type ApprovalPolicy struct {
	ID          int64  `json:"id"`
	Project     string `json:"project,omitempty"` // exact project or "" = all
	Tool        string `json:"tool,omitempty"`    // exact tool or "" = all
	Pattern     string `json:"pattern,omitempty"` // regex on tool input; "" = all
	Action      string `json:"action"`            // allow | ask | deny
	CooldownSec int    `json:"cooldownSec,omitempty"`
	Note        string `json:"note,omitempty"`
	CreatedAt   int64  `json:"createdAt,omitempty"`
}

// UpsertApproval inserts a new approval / audit row.
func (s *Store) UpsertApproval(ctx context.Context, a *Approval) error {
	routed := 0
	if a.Routed {
		routed = 1
	}
	collided := 0
	if a.Collided {
		collided = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals (
		id, session_id, run_id, mission_id, tool_use_id, tool_name, tool_input,
		input_hash, project, cwd, routed, state, decision_by, collided,
		created_at, expires_at, decided_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.SessionID, a.RunID, a.MissionID, a.ToolUseID, a.ToolName, a.ToolInput,
		a.InputHash, a.Project, a.CWD, routed, a.State, a.DecisionBy, collided,
		a.CreatedAt, a.ExpiresAt, a.DecidedAt)
	return err
}

// ResolveApproval finalizes a pending approval. A row that is no longer
// pending is left untouched, so a late human decision can never overwrite an
// expired row (or a timeout overwrite a human decision). Callers that need to
// know whether anything changed use ResolvePendingApproval.
func (s *Store) ResolveApproval(ctx context.Context, id, state, by string) error {
	_, err := s.ResolvePendingApproval(ctx, id, state, by)
	return err
}

// ResolvePendingApproval is ResolveApproval that also reports whether this
// call was the one that moved the row out of pending.
func (s *Store) ResolvePendingApproval(ctx context.Context, id, state, by string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET state = ?, decision_by = ?, decided_at = ?
		 WHERE id = ? AND state = 'pending'`,
		state, by, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetApproval returns one approval by id.
func (s *Store) GetApproval(ctx context.Context, id string) (*Approval, error) {
	row := s.db.QueryRowContext(ctx, approvalSelect+` WHERE id = ?`, id)
	a, err := scanApproval(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// PendingApprovalForRunTool returns the open pending approval for a
// (run, tool) pair — collapse target for the cooldown/anti-spam rule. A row
// whose deadline has passed is not open, even if nothing has marked it
// expired yet (for example after a restart mid-wait): collapsing onto it
// would park the new ask behind a request nobody can answer any more.
func (s *Store) PendingApprovalForRunTool(ctx context.Context, runID, tool string) (*Approval, error) {
	row := s.db.QueryRowContext(ctx, approvalSelect+
		` WHERE run_id = ? AND tool_name = ? AND state = 'pending'
		  AND (expires_at = 0 OR expires_at > ?)
		  ORDER BY created_at DESC LIMIT 1`,
		runID, tool, time.Now().UnixMilli())
	a, err := scanApproval(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// ListApprovals returns approvals filtered by state (+limit), newest first.
func (s *Store) ListApprovals(ctx context.Context, state, missionID string, limit int) ([]*Approval, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := approvalSelect
	args := []any{}
	conds := []string{}
	if state != "" {
		conds = append(conds, "state = ?")
		args = append(args, state)
	}
	if missionID != "" {
		conds = append(conds, "mission_id = ?")
		args = append(args, missionID)
	}
	if len(conds) > 0 {
		q += " WHERE " + joinConds(conds)
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PendingApprovalCount is the number of not-yet-decided approvals.
func (s *Store) PendingApprovalCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM approvals WHERE state = 'pending' AND expires_at > ?`,
		time.Now().UnixMilli()).Scan(&n)
	return n, err
}

// --- policies ---------------------------------------------------------------------

// UpsertPolicy inserts or replaces a standing policy.
func (s *Store) UpsertPolicy(ctx context.Context, p *ApprovalPolicy) error {
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().UnixMilli()
	}
	if p.ID == 0 {
		_, err := s.db.ExecContext(ctx, `INSERT INTO approval_policies
			(project, tool, pattern, action, cooldown_sec, note, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.Project, p.Tool, p.Pattern, p.Action, p.CooldownSec, p.Note, p.CreatedAt)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE approval_policies SET
		project = ?, tool = ?, pattern = ?, action = ?, cooldown_sec = ?, note = ?
		WHERE id = ?`,
		p.Project, p.Tool, p.Pattern, p.Action, p.CooldownSec, p.Note, p.ID)
	return err
}

// ListPolicies returns all standing policies.
func (s *Store) ListPolicies(ctx context.Context) ([]*ApprovalPolicy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project, tool, pattern, action, cooldown_sec, note, created_at
		 FROM approval_policies ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*ApprovalPolicy{}
	for rows.Next() {
		var p ApprovalPolicy
		if err := rows.Scan(&p.ID, &p.Project, &p.Tool, &p.Pattern, &p.Action,
			&p.CooldownSec, &p.Note, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// DeletePolicy removes a policy by id.
func (s *Store) DeletePolicy(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM approval_policies WHERE id = ?`, id)
	return err
}

const approvalSelect = `SELECT id, session_id, run_id, mission_id, tool_use_id, tool_name,
	tool_input, input_hash, project, cwd, routed, state, decision_by, collided,
	created_at, expires_at, decided_at FROM approvals`

func scanApproval(sc interface{ Scan(dest ...any) error }) (*Approval, error) {
	a := &Approval{}
	var routed, collided int
	err := sc.Scan(&a.ID, &a.SessionID, &a.RunID, &a.MissionID, &a.ToolUseID,
		&a.ToolName, &a.ToolInput, &a.InputHash, &a.Project, &a.CWD,
		&routed, &a.State, &a.DecisionBy, &collided, &a.CreatedAt, &a.ExpiresAt, &a.DecidedAt)
	a.Routed = routed == 1
	a.Collided = collided == 1
	return a, err
}

func joinConds(conds []string) string {
	out := ""
	for i, c := range conds {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out
}
