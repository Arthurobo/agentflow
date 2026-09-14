package store

import (
	"context"
	"database/sql"
	"time"
)

// insertEventSQL is the single INSERT OR IGNORE used by every ingest path.
// Idempotency is the UNIQUE dedupe_key.
const insertEventSQL = `INSERT OR IGNORE INTO events (
	id, dedupe_key, session_id, origin_session_id, event, subtype, actor,
	uuid, parent_uuid, seq, ts, tool_name, tool_use_id, tool_input_json,
	content, thinking_content, content_hash, oversized, is_sidechain, is_meta,
	retracted, has_thinking, thinking_signature, model, tokens_in, tokens_out,
	tokens_cache_read, tokens_cache_write, cost_usd, agent_id, file_offset,
	raw_version, is_error, terminal_reason, duration_ms, permission_denials_ct,
	source)
VALUES (NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const upsertSessionSQL = `INSERT INTO sessions (
	session_id, project, cwd, is_subagent, file_path)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (session_id) DO UPDATE SET
	project = excluded.project,
	cwd = CASE WHEN sessions.cwd = '' THEN excluded.cwd ELSE sessions.cwd END`

const upsertBlobSQL = `INSERT OR IGNORE INTO event_blobs (content_hash, content, size, created_at)
VALUES (?, ?, ?, ?)`

// Batch is a set of events (plus optional blobs) to be committed atomically.
// All events should share a session (ingest groups per-file batches).
type Batch struct {
	Events  []Incoming
	Blobs   []Blob
	Session *SessionMeta // session row to upsert before the aggregate refresh
}

// Blob is one oversized-content payload.
type Blob struct {
	Hash    string
	Content string
}

// SessionMeta is the session-level metadata upserted with a batch.
type SessionMeta struct {
	SessionID  string
	CWD        string
	Project    string
	FilePath   string
	IsSubagent bool
}

// InsertBatch commits a batch in one transaction: events (OR IGNORE), blobs
// (OR IGNORE), session row upsert, then refresh of that session's aggregates
// and its project row. The AFTER INSERT trigger on events maintains the
// search docs automatically (single-writer design: callers serialize).
func (s *Store) InsertBatch(ctx context.Context, b *Batch) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return s.insertBatchTx(ctx, tx, b)
	})
}

func (s *Store) insertBatchTx(ctx context.Context, tx *sql.Tx, b *Batch) error {
	stmt, err := tx.PrepareContext(ctx, insertEventSQL)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for i := range b.Events {
		ev := &b.Events[i]
		if _, err := stmt.ExecContext(ctx,
			ev.Key(), ev.SessionID, ev.OriginSessionID, ev.Event, ev.Subtype,
			ev.Actor, ev.UUID, ev.ParentUUID, ev.Seq, ev.TS, ev.ToolName,
			ev.ToolUseID, string(ev.ToolInput), ev.Content,
			ev.ThinkingContent, ev.ContentHash,
			ev.Oversized, ev.IsSidechain, ev.IsMeta, ev.Retracted,
			ev.HasThinking, ev.ThinkingSig, ev.Model, ev.TokensIn,
			ev.TokensOut, ev.TokCacheRead, ev.TokCacheWr, ev.CostUSD,
			ev.AgentID, ev.FileOffset, ev.RawVersion, ev.IsError,
			ev.TermReason, ev.DurationMs, ev.PermDenials, ev.Source,
		); err != nil {
			return err
		}
	}
	return s.finishBatchTx(ctx, tx, b)
}

// finishBatchTx writes the batch's blobs and refreshes the session and project
// aggregates.
func (s *Store) finishBatchTx(ctx context.Context, tx *sql.Tx, b *Batch) error {
	if len(b.Blobs) > 0 {
		bstmt, err := tx.PrepareContext(ctx, upsertBlobSQL)
		if err != nil {
			return err
		}
		defer func() { _ = bstmt.Close() }()
		for i := range b.Blobs {
			bl := &b.Blobs[i]
			if _, err := bstmt.ExecContext(ctx, bl.Hash, bl.Content,
				int64(len(bl.Content)), time.Now().UnixMilli()); err != nil {
				return err
			}
		}
	}
	if b.Session != nil {
		if _, err := tx.ExecContext(ctx, upsertSessionSQL,
			b.Session.SessionID, b.Session.Project, b.Session.CWD,
			b.Session.IsSubagent, b.Session.FilePath); err != nil {
			return err
		}
		if err := s.refreshSessionTx(ctx, tx, b.Session.SessionID); err != nil {
			return err
		}
		if err := s.refreshProjectTx(ctx, tx, b.Session.Project); err != nil {
			return err
		}
	}
	return nil
}

// refreshSessionTx recomputes all sessions columns from events.
func (s *Store) refreshSessionTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET
		started_ts = COALESCE((SELECT MIN(ts) FROM events WHERE session_id = ?1), 0),
		ended_ts = COALESCE((SELECT MAX(ts) FROM events WHERE session_id = ?1), 0),
		updated_at = COALESCE((SELECT MAX(ts) FROM events WHERE session_id = ?1), 0),
		first_prompt = COALESCE((SELECT content FROM events WHERE session_id = ?1
			AND event = 'user_message' AND content != '' AND is_meta = 0
			ORDER BY seq ASC LIMIT 1), ''),
		event_count = (SELECT COUNT(*) FROM events WHERE session_id = ?1),
		user_message_count = (SELECT COUNT(*) FROM events WHERE session_id = ?1 AND event = 'user_message'),
		assistant_message_count = (SELECT COUNT(*) FROM events WHERE session_id = ?1 AND event = 'assistant_message'),
		tool_use_count = (SELECT COUNT(*) FROM events WHERE session_id = ?1 AND event = 'tool_use'),
		tool_result_count = (SELECT COUNT(*) FROM events WHERE session_id = ?1 AND event = 'tool_result'),
		session_state_count = (SELECT COUNT(*) FROM events WHERE session_id = ?1 AND event = 'session_state'),
		queue_op_count = (SELECT COUNT(*) FROM events WHERE session_id = ?1 AND event = 'queue_op'),
		total_cost_usd = COALESCE((SELECT SUM(cost_usd) FROM events WHERE session_id = ?1 AND event = 'result'), 0),
		total_tokens_in = COALESCE((SELECT SUM(tokens_in) FROM events WHERE session_id = ?1 AND event IN ('assistant_message','result')), 0),
		total_tokens_out = COALESCE((SELECT SUM(tokens_out) FROM events WHERE session_id = ?1 AND event IN ('assistant_message','result')), 0),
		total_tokens_cache_read = COALESCE((SELECT SUM(tokens_cache_read) FROM events WHERE session_id = ?1 AND event IN ('assistant_message','result')), 0),
		total_tokens_cache_wr = COALESCE((SELECT SUM(tokens_cache_write) FROM events WHERE session_id = ?1 AND event IN ('assistant_message','result')), 0),
		permission_denials = COALESCE((SELECT COUNT(*) FROM events WHERE session_id = ?1 AND event = 'permission'), 0)
			+ COALESCE((SELECT SUM(permission_denials_ct) FROM events WHERE session_id = ?1 AND event = 'result'), 0),
		terminal_reason = COALESCE((SELECT terminal_reason FROM events WHERE session_id = ?1
			AND event = 'result' ORDER BY ts DESC, seq DESC LIMIT 1), ''),
		compaction_count = (SELECT COUNT(*) FROM events WHERE session_id = ?1
			AND event = 'system_meta' AND subtype = 'compact_boundary'),
		model_mix_json = COALESCE((SELECT json_group_array(json_object(
			'model', model,
			'messages', cnt,
			'tokensIn', tin,
			'tokensOut', tout,
			'cacheRead', cr,
			'cacheWrite', cw))
			FROM (SELECT model, COUNT(*) AS cnt,
				SUM(tokens_in) AS tin, SUM(tokens_out) AS tout,
				SUM(tokens_cache_read) AS cr, SUM(tokens_cache_write) AS cw
				FROM events WHERE session_id = ?1 AND event = 'assistant_message'
				AND model != '' GROUP BY model) WHERE model != ''), '[]'),
		primary_model = COALESCE((SELECT model FROM events
			WHERE session_id = ?1 AND event = 'assistant_message' AND model != ''
			GROUP BY model ORDER BY SUM(tokens_in) + SUM(tokens_out) DESC LIMIT 1), '')
		WHERE session_id = ?1`, sessionID)
	return err
}

// refreshProjectTx recomputes a project row from its sessions.
func (s *Store) refreshProjectTx(ctx context.Context, tx *sql.Tx, project string) error {
	if project == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO projects (project, cwd, session_count,
			event_count, total_cost_usd, total_tokens_in, total_tokens_out, first_seen, last_seen)
		SELECT project, MAX(cwd), COUNT(*), SUM(event_count), SUM(total_cost_usd),
			SUM(total_tokens_in), SUM(total_tokens_out), MIN(started_ts), MAX(updated_at)
		FROM sessions WHERE project = ? GROUP BY project
		ON CONFLICT (project) DO UPDATE SET
			cwd = excluded.cwd,
			session_count = excluded.session_count,
			event_count = excluded.event_count,
			total_cost_usd = excluded.total_cost_usd,
			total_tokens_in = excluded.total_tokens_in,
			total_tokens_out = excluded.total_tokens_out,
			first_seen = excluded.first_seen,
			last_seen = excluded.last_seen`, project)
	return err
}

// RefreshSession recomputes one session's aggregates outside a batch, for
// rows written before an aggregate existed.
func (s *Store) RefreshSession(ctx context.Context, sessionID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return s.refreshSessionTx(ctx, tx, sessionID)
	})
}

// RefreshAll recomputes every session + project aggregate (boot-time repair,
// cheap: 366 sessions).
func (s *Store) RefreshAll(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT session_id FROM sessions`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		for _, id := range ids {
			if err := s.refreshSessionTx(ctx, tx, id); err != nil {
				return err
			}
			var project string
			if err := tx.QueryRowContext(ctx,
				`SELECT project FROM sessions WHERE session_id = ?`, id).Scan(&project); err != nil {
				return err
			}
			if err := s.refreshProjectTx(ctx, tx, project); err != nil {
				return err
			}
		}
		return nil
	})
}

// EventCount returns the number of events for a session.
func (s *Store) EventCount(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}
