-- 0001_init.sql — the whole schema in one migration.
-- Consolidated at open-source inception; byte-identical to the
-- cumulative result of the project's earlier incremental migrations.

CREATE TABLE sessions (
  session_id TEXT PRIMARY KEY,
  project TEXT NOT NULL DEFAULT '',
  cwd TEXT NOT NULL DEFAULT '',
  is_subagent INTEGER NOT NULL DEFAULT 0,
  file_path TEXT NOT NULL DEFAULT '',
  started_ts INTEGER NOT NULL DEFAULT 0,
  ended_ts INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  first_prompt TEXT NOT NULL DEFAULT '',
  first_prompt_hash TEXT NOT NULL DEFAULT '',
  event_count INTEGER NOT NULL DEFAULT 0,
  user_message_count INTEGER NOT NULL DEFAULT 0,
  assistant_message_count INTEGER NOT NULL DEFAULT 0,
  tool_use_count INTEGER NOT NULL DEFAULT 0,
  tool_result_count INTEGER NOT NULL DEFAULT 0,
  session_state_count INTEGER NOT NULL DEFAULT 0,
  queue_op_count INTEGER NOT NULL DEFAULT 0,
  permission_denials INTEGER NOT NULL DEFAULT 0,
  total_cost_usd REAL NOT NULL DEFAULT 0,
  total_tokens_in INTEGER NOT NULL DEFAULT 0,
  total_tokens_out INTEGER NOT NULL DEFAULT 0,
  total_tokens_cache_read INTEGER NOT NULL DEFAULT 0,
  total_tokens_cache_wr INTEGER NOT NULL DEFAULT 0,
  model_mix_json TEXT NOT NULL DEFAULT '[]',
  primary_model TEXT NOT NULL DEFAULT '',
  terminal_reason TEXT NOT NULL DEFAULT '',
  compaction_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE events (
  id INTEGER PRIMARY KEY,
  dedupe_key TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL,
  origin_session_id TEXT NOT NULL DEFAULT '',
  event TEXT NOT NULL,
  subtype TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '',
  uuid TEXT NOT NULL DEFAULT '',
  parent_uuid TEXT NOT NULL DEFAULT '',
  seq INTEGER NOT NULL DEFAULT 0,
  ts INTEGER NOT NULL DEFAULT 0,
  tool_name TEXT NOT NULL DEFAULT '',
  tool_use_id TEXT NOT NULL DEFAULT '',
  tool_input_json TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  oversized INTEGER NOT NULL DEFAULT 0,
  is_sidechain INTEGER NOT NULL DEFAULT 0,
  is_meta INTEGER NOT NULL DEFAULT 0,
  retracted INTEGER NOT NULL DEFAULT 0,
  has_thinking INTEGER NOT NULL DEFAULT 0,
  thinking_signature TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  tokens_in INTEGER NOT NULL DEFAULT 0,
  tokens_out INTEGER NOT NULL DEFAULT 0,
  tokens_cache_read INTEGER NOT NULL DEFAULT 0,
  tokens_cache_write INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0,
  agent_id TEXT NOT NULL DEFAULT '',
  file_offset INTEGER NOT NULL DEFAULT 0,
  raw_version TEXT NOT NULL DEFAULT '',
  is_error INTEGER NOT NULL DEFAULT 0,
  terminal_reason TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  permission_denials_ct INTEGER NOT NULL DEFAULT 0,
  source TEXT NOT NULL DEFAULT ''
, thinking_content TEXT NOT NULL DEFAULT '');

CREATE TABLE projects (
  project TEXT PRIMARY KEY,
  cwd TEXT NOT NULL DEFAULT '',
  session_count INTEGER NOT NULL DEFAULT 0,
  event_count INTEGER NOT NULL DEFAULT 0,
  total_cost_usd REAL NOT NULL DEFAULT 0,
  total_tokens_in INTEGER NOT NULL DEFAULT 0,
  total_tokens_out INTEGER NOT NULL DEFAULT 0,
  first_seen INTEGER NOT NULL DEFAULT 0,
  last_seen INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE event_blobs (
  content_hash TEXT PRIMARY KEY,
  content TEXT NOT NULL,
  size INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE tailer_state (
  file_path TEXT PRIMARY KEY,
  offset INTEGER NOT NULL DEFAULT 0,
  seen_size INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE search_docs (
  id INTEGER PRIMARY KEY,
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL DEFAULT 0,
  ts INTEGER NOT NULL DEFAULT 0,
  event TEXT NOT NULL DEFAULT '',
  tool TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL DEFAULT ''
);

CREATE VIRTUAL TABLE search_idx USING fts5(
  content, event UNINDEXED, tool UNINDEXED, session_id UNINDEXED,
  seq UNINDEXED, ts UNINDEXED,
  content='search_docs', content_rowid='id', tokenize='unicode61'
);

CREATE TABLE managed_sessions (
  id TEXT PRIMARY KEY, -- agentd run key
  session_id TEXT NOT NULL DEFAULT '', -- claude session_id (system/init)
  kind TEXT NOT NULL DEFAULT 'chat', -- one_shot | chat
  cwd TEXT NOT NULL DEFAULT '',
  project TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  prompt TEXT NOT NULL DEFAULT '',
  resume_from TEXT NOT NULL DEFAULT '', -- --resume target when resuming
  state TEXT NOT NULL DEFAULT 'starting',
  pid INTEGER NOT NULL DEFAULT 0,
  started_at INTEGER NOT NULL DEFAULT 0,
  ended_at INTEGER NOT NULL DEFAULT 0,
  exit_code INTEGER NOT NULL DEFAULT 0,
  event_count INTEGER NOT NULL DEFAULT 0,
  permission_denials INTEGER NOT NULL DEFAULT 0,
  total_cost_usd REAL NOT NULL DEFAULT 0,
  terminal_reason TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT 'local', -- 'local' | device:<id> | remote
  updated_at INTEGER NOT NULL DEFAULT 0
, mission_id TEXT NOT NULL DEFAULT '', mission_role TEXT NOT NULL DEFAULT '', approvals_enabled INTEGER NOT NULL DEFAULT 0, pgid INTEGER NOT NULL DEFAULT 0, proc_start_ticks INTEGER NOT NULL DEFAULT 0, generation TEXT NOT NULL DEFAULT '', stop_reason TEXT NOT NULL DEFAULT '', engine TEXT NOT NULL DEFAULT 'claude', control_port INTEGER NOT NULL DEFAULT 0, control_base TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '');

CREATE TABLE devices (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  machine_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'device', -- device | pairing | agentd
  token_hash TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL DEFAULT 0, -- 0 = never (revocable active devices)
  status TEXT NOT NULL DEFAULT 'active', -- pending | active | revoked
  created_at INTEGER NOT NULL DEFAULT 0,
  last_seen INTEGER NOT NULL DEFAULT 0,
  revoked_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE missions (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  planner_run_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'created', -- created | running | blocked | completed | failed | over_budget
  budget_cents INTEGER NOT NULL DEFAULT 0, -- 0 = unlimited
  planner_prompt TEXT NOT NULL DEFAULT '',
  worker_template TEXT NOT NULL DEFAULT '', -- prompt template shared by workers
  worker_count INTEGER NOT NULL DEFAULT 0,
  created_by TEXT NOT NULL DEFAULT 'local',
  created_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  started_at INTEGER NOT NULL DEFAULT 0,
  ended_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  report_json TEXT NOT NULL DEFAULT '{}' -- end-of-mission report
);

CREATE TABLE missions_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  mission_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  level TEXT NOT NULL DEFAULT 'info', -- info | decision | error | budget
  message TEXT NOT NULL DEFAULT ''
);

CREATE TABLE approvals (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  mission_id TEXT NOT NULL DEFAULT '',
  tool_use_id TEXT NOT NULL DEFAULT '',
  tool_name TEXT NOT NULL DEFAULT '',
  tool_input TEXT NOT NULL DEFAULT '', -- preview (bounded)
  input_hash TEXT NOT NULL DEFAULT '',
  project TEXT NOT NULL DEFAULT '',
  cwd TEXT NOT NULL DEFAULT '',
  routed INTEGER NOT NULL DEFAULT 0, -- 1 = routed to approval (not auto-policy)
  state TEXT NOT NULL DEFAULT 'pending', -- pending | approved | denied | expired | cancelled
  decision_by TEXT NOT NULL DEFAULT '', -- policy:<rule> | device:<id> | timeout | hook
  collided INTEGER NOT NULL DEFAULT 0, -- collapsed into an existing pending approval
  created_at INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0,
  decided_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE approval_policies (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project TEXT NOT NULL DEFAULT '', -- exact project or '' = all
  tool TEXT NOT NULL DEFAULT '', -- exact tool or '' = all
  pattern TEXT NOT NULL DEFAULT '', -- regex on the tool input; '' = all inputs
  action TEXT NOT NULL DEFAULT 'ask',-- allow | ask | deny
  cooldown_sec INTEGER NOT NULL DEFAULT 0, -- per session+tool notification cooldown
  note TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE "loops" (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  task TEXT NOT NULL DEFAULT '',
  cwd TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active', -- active | complete | killed
  round INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT '',
  parent_loop_id TEXT NOT NULL DEFAULT '',
  poll_interval_seconds INTEGER NOT NULL DEFAULT 60,
  end_reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL DEFAULT 0
, play            TEXT    NOT NULL DEFAULT '', play_status     TEXT    NOT NULL DEFAULT '', step_id         TEXT    NOT NULL DEFAULT '', step_awaiting   TEXT    NOT NULL DEFAULT '', step_entered_at INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '', generation TEXT NOT NULL DEFAULT '', max_messages_per_round  INTEGER NOT NULL DEFAULT 0, max_wall_clock_seconds  INTEGER NOT NULL DEFAULT 0, max_members           INTEGER NOT NULL DEFAULT 0, started_at            INTEGER NOT NULL DEFAULT 0, play_ended_at INTEGER NOT NULL DEFAULT 0);

CREATE TABLE "loop_members" (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  loop_id TEXT NOT NULL,
  role TEXT NOT NULL,
  token_hash TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active', -- active | idle | dismissed
  poll_interval_seconds INTEGER NOT NULL DEFAULT 60,
  chars_in INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL DEFAULT 0,
  last_seen_at INTEGER NOT NULL DEFAULT 0, tool  TEXT NOT NULL DEFAULT 'claude', model TEXT NOT NULL DEFAULT '', run_id     TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '', stranded_since  INTEGER NOT NULL DEFAULT 0, last_posted_at  INTEGER NOT NULL DEFAULT 0, last_polled_at  INTEGER NOT NULL DEFAULT 0,
  UNIQUE (loop_id, role)
);

CREATE TABLE "loop_messages" (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  loop_id TEXT NOT NULL,
  sender_id TEXT NOT NULL DEFAULT '',
  sender_role TEXT NOT NULL DEFAULT '',
  recipient_id TEXT NOT NULL DEFAULT '',
  recipient_role TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending', -- pending | delivered | acked | cancelled
  delivery_count INTEGER NOT NULL DEFAULT 0,
  claim_id TEXT NOT NULL DEFAULT '',
  lease_seconds INTEGER NOT NULL DEFAULT 0,
  forwarded_from TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0,
  delivered_at INTEGER NOT NULL DEFAULT 0,
  acked_at INTEGER NOT NULL DEFAULT 0
, step_id TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL DEFAULT '', mirror_of TEXT NOT NULL DEFAULT '', idempotency_key TEXT    NOT NULL DEFAULT '', lease_expiries INTEGER NOT NULL DEFAULT 0);

CREATE TABLE "loop_notes" (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  loop_id TEXT NOT NULL,
  role TEXT NOT NULL,
  author_id TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE "loop_refusals" (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  id         TEXT    NOT NULL UNIQUE,
  loop_id    TEXT    NOT NULL,
  kind       TEXT    NOT NULL,
  from_role  TEXT    NOT NULL DEFAULT '',
  to_role    TEXT    NOT NULL DEFAULT '',
  play       TEXT    NOT NULL DEFAULT '',
  step_id    TEXT    NOT NULL DEFAULT '',
  content    TEXT    NOT NULL DEFAULT '',
  detail     TEXT    NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE prompt_overrides (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  id          TEXT    NOT NULL UNIQUE,
  kind        TEXT    NOT NULL,
  play        TEXT    NOT NULL DEFAULT '',
  step        TEXT    NOT NULL DEFAULT '',
  rule        TEXT    NOT NULL DEFAULT '',
  field       TEXT    NOT NULL,
  value       TEXT    NOT NULL,
  base_digest TEXT    NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL DEFAULT 0,
  UNIQUE (kind, play, step, rule, field)
);

CREATE TABLE attachments (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL,
    session_id  TEXT NOT NULL DEFAULT '',
    path        TEXT NOT NULL,
    mime        TEXT NOT NULL DEFAULT '',
    bytes       INTEGER NOT NULL DEFAULT 0,
    sha256      TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE agentflow_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE loop_events (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    id         TEXT    NOT NULL UNIQUE,
    loop_id    TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    content    TEXT    NOT NULL DEFAULT '',
    detail     TEXT    NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE opencode_backfill_state (
    session_id TEXT PRIMARY KEY,
    updated_at INTEGER NOT NULL DEFAULT 0,
    indexed_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_sessions_updated ON sessions (updated_at DESC);

CREATE INDEX idx_sessions_project ON sessions (project);

CREATE INDEX idx_sessions_model ON sessions (primary_model);

CREATE INDEX idx_sessions_started ON sessions (started_ts);

CREATE INDEX idx_events_session_seq ON events (session_id, seq);

CREATE INDEX idx_events_session_event ON events (session_id, event);

CREATE INDEX idx_events_session_tool ON events (session_id, tool_name);

CREATE INDEX idx_events_session_actor ON events (session_id, actor);

CREATE INDEX idx_events_ts ON events (ts);

CREATE INDEX idx_events_content_hash ON events (content_hash);

CREATE INDEX idx_events_tool_use_id ON events (tool_use_id);

CREATE INDEX idx_events_origin_session ON events (origin_session_id);

CREATE INDEX idx_events_event_model ON events (event, model);

CREATE INDEX idx_events_session_agent ON events (session_id, agent_id);

CREATE INDEX idx_managed_sessions_state ON managed_sessions (state);

CREATE INDEX idx_managed_sessions_updated ON managed_sessions (updated_at);

CREATE INDEX idx_managed_sessions_sid ON managed_sessions (session_id);

CREATE INDEX idx_devices_status ON devices (status);

CREATE INDEX idx_managed_sessions_mission ON managed_sessions (mission_id, mission_role);

CREATE INDEX idx_missions_status ON missions (status);

CREATE INDEX idx_missions_updated ON missions (updated_at);

CREATE INDEX idx_missions_log ON missions_log (mission_id, ts);

CREATE INDEX idx_approvals_state ON approvals (state);

CREATE INDEX idx_approvals_run ON approvals (run_id, state);

CREATE INDEX idx_approvals_session ON approvals (session_id, tool_name, state);

CREATE INDEX idx_loop_members_token    ON loop_members(token_hash);

CREATE INDEX idx_loop_members_loop     ON loop_members(loop_id, status);

CREATE INDEX idx_loop_messages_claim   ON loop_messages(claim_id);

CREATE INDEX idx_loop_messages_inbox   ON loop_messages(recipient_id, status, seq);

CREATE INDEX idx_loop_messages_loop    ON loop_messages(loop_id, seq);

CREATE INDEX idx_loop_messages_lease   ON loop_messages(status, delivered_at);

CREATE INDEX idx_loop_notes_role       ON loop_notes(loop_id, role, seq);

CREATE INDEX idx_loops_play            ON loops(play, play_status);

CREATE INDEX idx_loop_refusals_loop    ON loop_refusals(loop_id, seq);

CREATE INDEX idx_loop_members_run ON loop_members(run_id);

CREATE INDEX idx_prompt_overrides_kind ON prompt_overrides(kind, play, rule);

CREATE UNIQUE INDEX idx_loop_messages_idem
    ON loop_messages(sender_id, idempotency_key) WHERE idempotency_key <> '';

CREATE INDEX idx_attachments_run ON attachments(run_id);

CREATE INDEX idx_attachments_session ON attachments(session_id);

CREATE INDEX idx_attachments_created ON attachments(created_at);

CREATE INDEX idx_managed_sessions_session_id
    ON managed_sessions(session_id);

CREATE INDEX idx_loops_status_generation
    ON loops(status, generation);

CREATE INDEX idx_loop_events_loop
    ON loop_events(loop_id, seq);

CREATE INDEX idx_loop_events_kind
    ON loop_events(kind, created_at);

CREATE TRIGGER events_search_ai AFTER INSERT ON events BEGIN
  INSERT INTO search_docs (id, session_id, seq, ts, event, tool, content)
  SELECT new.id, new.session_id, new.seq, new.ts, new.event, new.tool_name,
    CASE
      WHEN new.event = 'tool_use'
        THEN substr(new.tool_name || ' ' || new.tool_input_json, 1, 16384)
      ELSE new.content
    END
  WHERE new.event IN ('user_message','assistant_message','tool_use',
                      'tool_result','queue_op')
    AND new.oversized = 0
    AND CASE WHEN new.event = 'tool_use'
        THEN new.tool_input_json != ''
        ELSE new.content != '' END;
END;

CREATE TRIGGER search_docs_ai AFTER INSERT ON search_docs BEGIN
  INSERT INTO search_idx (rowid, content, event, tool, session_id, seq, ts)
  VALUES (new.id, new.content, new.event, new.tool, new.session_id,
          new.seq, new.ts);
END;

CREATE TRIGGER search_docs_ad AFTER DELETE ON search_docs BEGIN
  INSERT INTO search_idx (search_idx, rowid, content, event, tool, session_id,
                          seq, ts)
  VALUES ('delete', old.id, old.content, old.event, old.tool, old.session_id,
          old.seq, old.ts);
END;

CREATE TRIGGER search_docs_au AFTER UPDATE ON search_docs BEGIN
  INSERT INTO search_idx (search_idx, rowid, content, event, tool, session_id,
                          seq, ts)
  VALUES ('delete', old.id, old.content, old.event, old.tool, old.session_id,
          old.seq, old.ts);
  INSERT INTO search_idx (rowid, content, event, tool, session_id, seq, ts)
  VALUES (new.id, new.content, new.event, new.tool, new.session_id,
          new.seq, new.ts);
END;

