package store

import (
	"context"
	"database/sql"
	"time"
)

// OpenCodeBackfillWatermark is the session "updated" time the OpenCode
// backfill last indexed a session at, 0 when it never has.
func (s *Store) OpenCodeBackfillWatermark(ctx context.Context, sessionID string) (int64, error) {
	var at int64
	err := s.db.QueryRowContext(ctx,
		`SELECT updated_at FROM opencode_backfill_state WHERE session_id = ?`, sessionID).Scan(&at)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return at, err
}

// SetOpenCodeBackfillWatermark records that a session has been indexed up to
// its storage's updatedAt.
func (s *Store) SetOpenCodeBackfillWatermark(ctx context.Context, sessionID string, updatedAt int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO opencode_backfill_state (session_id, updated_at, indexed_at)
		VALUES (?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET updated_at = excluded.updated_at, indexed_at = excluded.indexed_at`,
		sessionID, updatedAt, time.Now().UnixMilli())
	return err
}

// SetSessionFirstPromptIfEmpty gives an indexed session a first prompt when it
// has none, for engines whose sessions have a title but no prompt text.
func (s *Store) SetSessionFirstPromptIfEmpty(ctx context.Context, sessionID, prompt string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET first_prompt = ?
		WHERE session_id = ? AND first_prompt = ''`, prompt, sessionID)
	return err
}
