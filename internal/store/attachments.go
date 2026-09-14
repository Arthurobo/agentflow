package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Attachment is one file uploaded from a phone over the direct data channel
// and written to this machine's disk.
//
// The row is the only mapping from an id to a path. Callers hand ids around;
// nothing outside this package and the uploads writer ever chooses or
// receives a path, which is what keeps a prompt from naming a file it does
// not own.
type Attachment struct {
	ID        string `json:"id"`
	RunID     string `json:"runId"`
	SessionID string `json:"sessionId,omitempty"`
	Path      string `json:"path"`
	Mime      string `json:"mime,omitempty"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// InsertAttachment records a completed upload.
func (s *Store) InsertAttachment(ctx context.Context, a *Attachment) error {
	if a == nil || a.ID == "" || a.RunID == "" || a.Path == "" {
		return errors.New("store: attachment needs an id, a run and a path")
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().UnixMilli()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO attachments
		(id, run_id, session_id, path, mime, bytes, sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.RunID, a.SessionID, a.Path, a.Mime, a.Bytes, a.SHA256, a.CreatedAt)
	return err
}

// GetAttachment returns one attachment (nil when absent).
func (s *Store) GetAttachment(ctx context.Context, id string) (*Attachment, error) {
	var a Attachment
	err := s.db.QueryRowContext(ctx, `SELECT id, run_id, session_id, path, mime, bytes, sha256, created_at
		FROM attachments WHERE id = ?`, id).
		Scan(&a.ID, &a.RunID, &a.SessionID, &a.Path, &a.Mime, &a.Bytes, &a.SHA256, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// AttachmentsForRun resolves a set of ids, returning ONLY the ones that
// belong to runID.
//
// The filter is the authorization check and it lives here, in the query,
// rather than in a caller's loop: a prompt carrying someone else's
// attachment id must come back short, not come back with the file.
func (s *Store) AttachmentsForRun(ctx context.Context, runID string, ids []string) ([]*Attachment, error) {
	out := []*Attachment{}
	if runID == "" || len(ids) == 0 {
		return out, nil
	}
	const maxIDs = 32
	if len(ids) > maxIDs {
		ids = ids[:maxIDs]
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, runID)
	placeholders := ""
	for i, id := range ids {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}
	//nolint:gosec // the only interpolation is a generated "?,?,…" list
	q := `SELECT id, run_id, session_id, path, mime, bytes, sha256, created_at
		FROM attachments WHERE run_id = ? AND id IN (` + placeholders + `)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	found := map[string]*Attachment{}
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.RunID, &a.SessionID, &a.Path, &a.Mime,
			&a.Bytes, &a.SHA256, &a.CreatedAt); err != nil {
			return nil, err
		}
		found[a.ID] = &a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Caller order is the order the engineer attached them in, and the
	// prompt reads better for it.
	for _, id := range ids {
		if a := found[id]; a != nil {
			out = append(out, a)
		}
	}
	return out, nil
}

// SessionAttachmentUsage totals what a session has already stored, for the
// quota check.
func (s *Store) SessionAttachmentUsage(ctx context.Context, sessionID string) (files int64, bytes int64, err error) {
	if sessionID == "" {
		return 0, 0, nil
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(bytes), 0) FROM attachments WHERE session_id = ?`,
		sessionID).Scan(&files, &bytes)
	return files, bytes, err
}

// DeleteAttachment removes one row. The FILE is deleted by the caller first —
// losing the row while the file survives leaks disk that nothing can ever
// find again, so the row is the last thing to go.
func (s *Store) DeleteAttachment(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE id = ?`, id)
	return err
}

// AttachmentsOlderThan lists attachments created before cutoff (ms).
func (s *Store) AttachmentsOlderThan(ctx context.Context, cutoffMs int64, limit int) ([]*Attachment, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, session_id, path, mime, bytes, sha256, created_at
		 FROM attachments WHERE created_at < ? ORDER BY created_at ASC LIMIT ?`,
		cutoffMs, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanAttachments(rows)
}

// AttachmentsWithoutASession lists attachments whose run no longer has a
// managed row: the session was removed and nothing will ever reference the
// file again.
func (s *Store) AttachmentsWithoutASession(ctx context.Context, limit int) ([]*Attachment, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, a.run_id, a.session_id, a.path, a.mime, a.bytes, a.sha256, a.created_at
		 FROM attachments a
		 LEFT JOIN managed_sessions m ON m.id = a.run_id
		 WHERE m.id IS NULL
		 ORDER BY a.created_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanAttachments(rows)
}

func scanAttachments(rows *sql.Rows) ([]*Attachment, error) {
	out := []*Attachment{}
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.RunID, &a.SessionID, &a.Path, &a.Mime,
			&a.Bytes, &a.SHA256, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}
