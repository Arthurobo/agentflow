package store

import (
	"context"
	"path/filepath"
)

// SessionHead is the part of a session row a list needs to order and page
// through sessions before it spends anything on titles.
type SessionHead struct {
	ID        string
	Cwd       string
	UpdatedAt int64
}

// ListOpenCodeSessionHeads returns every indexed OpenCode session. OpenCode
// rows are the ones whose file_path is under the "opencode" storage prefix
// the OpenCode ingest writes; Claude transcripts are listed from disk
// instead, so they're left out here.
func (s *Store) ListOpenCodeSessionHeads(ctx context.Context) ([]SessionHead, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, cwd, updated_at FROM sessions
		WHERE file_path LIKE ? AND is_subagent = 0`, "opencode"+string(filepath.Separator)+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []SessionHead{}
	for rows.Next() {
		var h SessionHead
		if err := rows.Scan(&h.ID, &h.Cwd, &h.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
