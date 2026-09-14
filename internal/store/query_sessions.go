package store

import (
	"context"
	"database/sql"
	"html"
	"regexp"
	"strings"
)

// Engine ids, matching engine.IDClaude / engine.IDOpenCode. Duplicated as
// bare strings rather than imported: the store must not depend on the engine
// package, and these two values are in the database's own vocabulary now.
const (
	EngineClaude   = "claude"
	EngineOpenCode = "opencode"
)

var namedSessionRe = regexp.MustCompile(`(?s)The user named this session "([^"]+)"`)

// managedListID is the id a managed run is known by: its session id once it
// has one, otherwise its run id (an unbound run keyed by an empty session id
// would collide with every other unbound run).
const managedListID = `CASE WHEN m.session_id != '' THEN m.session_id ELSE m.id END`

// isManagedLive reports whether a managed state means the process is still
// there. Mirrors spawner.State.Terminal(), inverted.
func isManagedLive(state string) bool {
	switch strings.TrimSpace(state) {
	case "starting", "running", "awaiting":
		return true
	}
	return false
}

// managedFallbackTitle names a managed run that has neither a spawn title nor
// a prompt to derive one from, which is every TTY run until someone names it.
func managedFallbackTitle(project, id string) string {
	if p := strings.TrimSpace(project); p != "" {
		return p
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// GetSession returns one session row (nil when missing).
func (s *Store) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	q := `SELECT session_id, project, cwd, is_subagent, file_path, started_ts,
		ended_ts, updated_at, first_prompt,
		COALESCE((SELECT e.content FROM events e WHERE e.session_id = sessions.session_id
			AND e.event = 'session_state'
			AND e.subtype IN ('ai-title', 'custom-title', 'summary')
			AND e.content != ''
			ORDER BY CASE e.subtype WHEN 'custom-title' THEN 0 ELSE 1 END, e.seq DESC LIMIT 1), '') AS state_title,
		COALESCE((SELECT content FROM events e WHERE e.session_id = sessions.session_id
			AND e.event = 'user_message'
			AND e.content LIKE '%The user named this session "%'
			ORDER BY e.seq ASC LIMIT 1), '') AS title_hint,
		-- The name a MANAGED run was given at spawn. It outranks everything
		-- derived from the transcript because it is what a person actually
		-- typed, it is available immediately, and it is the only name an
		-- OpenCode run has at all until that engine invents a summary of
		-- its own.
		COALESCE((SELECT m.title FROM managed_sessions m
			WHERE m.session_id = sessions.session_id AND m.title != ''
			ORDER BY m.updated_at DESC LIMIT 1), '') AS managed_title,
		event_count, user_message_count,
		assistant_message_count, tool_use_count, tool_result_count,
		session_state_count, queue_op_count, permission_denials, total_cost_usd,
		total_tokens_in, total_tokens_out, total_tokens_cache_read,
		total_tokens_cache_wr, model_mix_json, primary_model, terminal_reason,
		compaction_count
		FROM sessions WHERE session_id = ?`
	var it Session
	var mixRaw string
	var titleHint, stateTitle, managedTitle string
	var isAgent int64
	err := s.db.QueryRowContext(ctx, q, sessionID).Scan(&it.ID, &it.Project,
		&it.Cwd, &isAgent, &it.FilePath, &it.StartedAt, &it.EndedAt,
		&it.UpdatedAt, &it.FirstPrompt, &stateTitle, &titleHint, &managedTitle,
		&it.EventCount, &it.UserMessageCount,
		&it.AssistantCount, &it.ToolUseCount, &it.ToolResultCount,
		&it.SessionStateCount, &it.QueueOpCount, &it.PermissionDenials,
		&it.TotalCostUSD, &it.TokensIn, &it.TokensOut, &it.TokCacheRead,
		&it.TokCacheWrite, &mixRaw, &it.PrimaryModel, &it.TerminalReason,
		&it.CompactionCount)
	if err == sql.ErrNoRows {
		// No transcript under that id. It may still be a managed run the
		// corpus has never seen — every live OpenCode run is one — and
		// tapping a row the list just showed must not 404.
		return s.getManagedSessionAsSession(ctx, sessionID)
	}
	if err != nil {
		return nil, err
	}
	it.IsSubagent = isAgent == 1
	it.ModelMix = UnmarshalModelMix(mixRaw)
	it.Title = ManagedOrDerivedTitle(managedTitle, it.FirstPrompt, stateTitle, titleHint)
	return &it, nil
}

// getManagedSessionAsSession resolves a managed run that has no corpus row,
// projected onto the same shape the list emits for it.
//
// The id may be either the run's session id or its run key, because that is
// exactly what the list emitted: a bound run lists under its session id, an
// unbound one under its run id. Looking up only one of the two would 404 half
// the rows the list just rendered.
func (s *Store) getManagedSessionAsSession(ctx context.Context, id string) (*Session, error) {
	var it Session
	var managedTitle, managedState string
	err := s.db.QueryRowContext(ctx,
		`SELECT `+managedListID+`, m.project, m.cwd, m.started_at, m.ended_at,
			m.updated_at, m.prompt, m.title, m.event_count, m.permission_denials,
			m.total_cost_usd, m.model, m.terminal_reason, m.engine, m.state
		 FROM managed_sessions m
		 WHERE (m.session_id = ? AND m.session_id != '') OR m.id = ?
		 ORDER BY m.updated_at DESC LIMIT 1`, id, id).
		Scan(&it.ID, &it.Project, &it.Cwd, &it.StartedAt, &it.EndedAt,
			&it.UpdatedAt, &it.FirstPrompt, &managedTitle, &it.EventCount,
			&it.PermissionDenials, &it.TotalCostUSD, &it.PrimaryModel,
			&it.TerminalReason, &it.Engine, &managedState)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	it.Managed = true
	it.ManagedState = managedState
	it.IsActive = isManagedLive(managedState)
	it.Title = ManagedOrDerivedTitle(managedTitle, it.FirstPrompt, "", "")
	if it.Title == "" {
		it.Title = managedFallbackTitle(it.Project, it.ID)
	}
	return &it, nil
}

// SessionTitle returns the UI-facing session label without changing
// first_prompt, which remains the raw indexed prompt for compatibility.
// Priority: claude's own session name (ai-title / custom-title / summary
// snapshot records — what `claude -r` shows), then a user-assigned name,
// then the first prompt.
// ManagedOrDerivedTitle is what a session is called, when it might be a
// managed run.
//
// The spawn title wins outright. It is what a person typed, it exists from
// the first millisecond, and for an OpenCode run it is the ONLY name until
// that engine writes a summary — which is why such runs read "Untitled".
// Everything after it is the transcript's own account of itself.
func ManagedOrDerivedTitle(managedTitle, firstPrompt, stateTitle, titleHint string) string {
	if t := strings.TrimSpace(managedTitle); t != "" {
		return t
	}
	return SessionTitle(firstPrompt, stateTitle, titleHint)
}

func SessionTitle(firstPrompt, stateTitle, titleHint string) string {
	if t := strings.TrimSpace(stateTitle); t != "" {
		return t
	}
	if m := namedSessionRe.FindStringSubmatch(titleHint); len(m) == 2 {
		if title := strings.TrimSpace(html.UnescapeString(m[1])); title != "" {
			return title
		}
	}
	clean := strings.TrimSpace(firstPrompt)
	if strings.Contains(clean, "<command-name>") ||
		strings.Contains(clean, "<local-command-") ||
		strings.Contains(clean, "<command-message>") {
		return ""
	}
	return clean
}
