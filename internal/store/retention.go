package store

import (
	"context"
	"database/sql"
	"time"
)

// DefaultRetentionDays is how long the session index keeps a session nobody
// has touched, when AF_RETENTION_DAYS does not say otherwise.
const DefaultRetentionDays = 90

// retentionBatch is how many sessions one prune transaction removes. Every
// deleted event also deletes its search document through a trigger, so a
// small batch keeps each transaction's hold on the write lock short.
const retentionBatch = 50

// PruneReport is what one retention pass removed.
type PruneReport struct {
	Sessions int64
	Events   int64
	Blobs    int64
}

// PruneSessionsNotUpdatedSince deletes the indexed history (events, their
// search documents, oversized-content blobs nothing references any more, and
// the session row) of every session whose last event is older than cutoff.
//
// A session with a live managed run is kept however old its last event: a
// run resumed today on a session last touched in spring is not history.
// Loops, runs and devices are not touched; this is the transcript index,
// which grows with every tool call and thinking block.
func (s *Store) PruneSessionsNotUpdatedSince(ctx context.Context, cutoff time.Time) (PruneReport, error) {
	var rep PruneReport
	cut := cutoff.UnixMilli()
	for {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		var n int64
		err := s.tx(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `SELECT session_id, project FROM sessions
				WHERE updated_at > 0 AND updated_at < ?
				  AND session_id NOT IN (SELECT session_id FROM managed_sessions
				      WHERE session_id != '' AND state NOT IN ('finished', 'stopped', 'crashed'))
				LIMIT ?`, cut, retentionBatch)
			if err != nil {
				return err
			}
			var ids []any
			projects := map[string]bool{}
			for rows.Next() {
				var id, project string
				if err := rows.Scan(&id, &project); err != nil {
					_ = rows.Close()
					return err
				}
				ids = append(ids, id)
				projects[project] = true
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			_ = rows.Close()
			if len(ids) == 0 {
				return nil
			}
			in := placeholders(len(ids))
			//nolint:gosec // G202: placeholders() emits only "?" separators
			if _, err := tx.ExecContext(ctx, `DELETE FROM search_docs WHERE id IN
				(SELECT id FROM events WHERE session_id IN (`+in+`))`, ids...); err != nil {
				return err
			}
			//nolint:gosec // G202: same generated placeholder list
			res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE session_id IN (`+in+`)`, ids...)
			if err != nil {
				return err
			}
			events, err := res.RowsAffected()
			if err != nil {
				return err
			}
			//nolint:gosec // G202: same generated placeholder list
			if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE session_id IN (`+in+`)`, ids...); err != nil {
				return err
			}
			for project := range projects {
				if err := s.refreshProjectTx(ctx, tx, project); err != nil {
					return err
				}
			}
			n = int64(len(ids))
			rep.Sessions += n
			rep.Events += events
			return nil
		})
		if err != nil {
			return rep, err
		}
		if n < retentionBatch {
			break
		}
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM event_blobs WHERE content_hash NOT IN
		(SELECT content_hash FROM events WHERE content_hash != '')`)
	if err != nil {
		return rep, err
	}
	if rep.Blobs, err = res.RowsAffected(); err != nil {
		return rep, err
	}
	// Give the freed pages back. It only does anything on a database created
	// with auto_vacuum=INCREMENTAL, which every new install is.
	if rep.Sessions > 0 || rep.Blobs > 0 {
		if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return rep, err
		}
	}
	return rep, nil
}
