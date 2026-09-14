// Package opencode indexes OpenCode sessions. It owns two paths:
//
// 1. An event-stream consumer on GET /event of a running OpenCode, one per
// run. Each event is normalized into eventmodel and handed to the shared
// ingest writer, so the index keeps a single writer.
//
// 2. A backfill walk over the on-disk storage tree at
// ~/.local/share/opencode/storage/{session,message,part}/, for sessions
// started outside agentflow, which are adopted read-only.
//
// Both paths produce the same normalized events, keyed by OpenCode's own part
// and message ids, so a part seen by both is stored once.
package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
	"github.com/arthurobo/agentflow/internal/store"
)

// SessionMeta derives a session metadata block from the OpenCode session
// payload. The source field uses the storage path under
// ~/.local/share/opencode/storage — a stable identifier that survives
// restart and is unique per session.
func SessionMetaFromSession(s *SessionDoc, projectID string) *store.SessionMeta {
	return &store.SessionMeta{
		SessionID:  s.ID,
		CWD:        s.Directory,
		Project:    s.Directory,
		FilePath:   storageSessionPath(projectID, s.ID),
		IsSubagent: false,
	}
}

// storageSessionPath is the canonical path the writer will see on the
// managed_sessions row's file_path column. The OpenCode storage layout is
//
//	~/.local/share/opencode/storage/session/<projectID>/<sessionID>.json
//
// We persist the same shape so backfill and live SSE produce the same
// meta for the same session — the dedup key on (engine, sessionID, file
// path) keeps them from double-writing.
func storageSessionPath(projectID, sessionID string) string {
	return filepath.Join("opencode", "session", projectID, sessionID+".json")
}

// StorageSessionPathForTest exposes storageSessionPath to the package's
// tests without widening the public API.
func StorageSessionPathForTest(projectID, sessionID string) string {
	return storageSessionPath(projectID, sessionID)
}

// Writer is the single-writer sink the ingest service exposes. The
// internal/ingest.Service satisfies it; tests pass a recorder. Keeping the
// interface local avoids an import cycle with internal/ingest.
type Writer interface {
	// EnqueueLive mirrors ingest.Service.EnqueueLive: events go through the
	// single SQLite writer.
	EnqueueLive(ctx context.Context, evs []store.Incoming, meta *store.SessionMeta) error
}

// flusher is a Writer that can wait for everything handed to it to commit.
// The backfill uses it before recording that a session is indexed, so a
// crash mid-backfill never records a session whose events never landed.
type flusher interface {
	Flush(ctx context.Context) error
}

// Ingest is the OpenCode ingest façade. One instance per agentd process;
// the spawner's post-start hook calls StartSSE and the boot path calls
// BackfillAll for adoption.
type Ingest struct {
	st     *store.Store
	writer Writer
	clock  func() int64

	mu sync.Mutex
	// consumers is keyed by run. Keying by control URL let a second run that
	// was handed a port a finished run had used find the old entry and
	// never get a consumer of its own.
	consumers map[string]*sseConsumer
}

// New builds an Ingest over the shared store + writer.
func New(st *store.Store, w Writer) *Ingest {
	return &Ingest{
		st:        st,
		writer:    w,
		clock:     func() int64 { return time.Now().UnixMilli() },
		consumers: map[string]*sseConsumer{},
	}
}

// SessionDoc is the on-disk session projection (mirrors the SSE event
// payload for "session.updated" / "session.created").
type SessionDoc struct {
	ID        string `json:"id"`
	Slug      string `json:"slug,omitempty"`
	ProjectID string `json:"projectID,omitempty"`
	Directory string `json:"directory,omitempty"`
	Title     string `json:"title,omitempty"`
	Version   string `json:"version,omitempty"`
	// OpenCode stores session timestamps under the nested "time" object
	// (`{"time":{"created": <ms>, "updated": <ms>}}`) — NOT the camelCase
	// fields at the root. json tags below accept both shapes so live SSE
	// payloads (camelCase) and storage JSON files (nested) decode correctly.
	CreatedAt int64 `json:"createdAt,omitempty"`
	UpdatedAt int64 `json:"updatedAt,omitempty"`
	// raw captures the nested time object verbatim for unmarshalling into
	// the flat CreatedAt / UpdatedAt fields below.
	Raw map[string]any `json:"time,omitempty"`
}

// MessageDoc is the on-disk message projection.
type MessageDoc struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID"`
	Role      string         `json:"role"`
	Agent     string         `json:"agent,omitempty"`
	Model     *ModelRef      `json:"model,omitempty"`
	Time      map[string]any `json:"time,omitempty"`
	Summary   map[string]any `json:"summary,omitempty"`
}

// ModelRef is the model descriptor on a message.
type ModelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// PartDoc is the on-disk part projection.
type PartDoc struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// reasoning
	Reasoning string `json:"reasoning,omitempty"`
	// file
	Mime     string `json:"mime,omitempty"`
	Filename string `json:"filename,omitempty"`
	URL      string `json:"url,omitempty"`
	// tool
	CallID string     `json:"callID,omitempty"`
	Tool   string     `json:"tool,omitempty"`
	State  *ToolState `json:"state,omitempty"`
	// step-finish
	Reason string      `json:"reason,omitempty"`
	Cost   float64     `json:"cost,omitempty"`
	Tokens *TokenUsage `json:"tokens,omitempty"`
	// Time is the part's own {start, end} in unix ms; end is absent while a
	// text or reasoning part is still streaming.
	Time map[string]any `json:"time,omitempty"`
}

// timestamp is when the part happened by its own clock: its start, its end,
// or its tool state's, whichever is known first. 0 when none is.
func (p *PartDoc) timestamp() int64 {
	for _, t := range []map[string]any{p.Time, stateTime(p.State)} {
		if v := toInt64(t["start"]); v > 0 {
			return v
		}
		if v := toInt64(t["end"]); v > 0 {
			return v
		}
	}
	return 0
}

// streaming reports a text or reasoning part OpenCode is still writing. Its
// id is its identity in the index, so storing it now would keep the first
// few words and ignore the finished text.
func (p *PartDoc) streaming() bool {
	if p.Type != "text" && p.Type != "reasoning" || p.Time == nil {
		return false
	}
	_, ended := p.Time["end"]
	return !ended
}

func stateTime(st *ToolState) map[string]any {
	if st == nil {
		return nil
	}
	return st.Time
}

// ToolState is the tool call state machine.
type ToolState struct {
	Status   string         `json:"status"`
	Input    map[string]any `json:"input,omitempty"`
	Output   string         `json:"output,omitempty"`
	Title    string         `json:"title,omitempty"`
	Time     map[string]any `json:"time,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// TokenUsage is the per-step token breakdown.
type TokenUsage struct {
	Total     int64       `json:"total,omitempty"`
	Input     int64       `json:"input,omitempty"`
	Output    int64       `json:"output,omitempty"`
	Reasoning int64       `json:"reasoning,omitempty"`
	Cache     *TokenCache `json:"cache,omitempty"`
}

// TokenCache is the cache read/write breakdown.
type TokenCache struct {
	Read  int64 `json:"read,omitempty"`
	Write int64 `json:"write,omitempty"`
}

// NormalizeEvent converts a raw OpenCode part payload into the canonical
// eventmodel representation:
//
//	part.text (assistant) -> assistant_message
//	part.reasoning -> assistant_message (ThinkingContent)
//	part.tool (status=running) -> tool_use
//	part.tool (completed/error) -> tool_result
//	part.step-finish -> result (cost + tokens)
//	part.file -> tool_use (attachment flavor)
//	EventPermissionAsked V2 -> permission
//	EventSession* -> session_state
//	anything unknown -> meta
func NormalizePart(p *PartDoc, session *SessionDoc) []*eventmodel.Event {
	if p == nil {
		return nil
	}
	evs := normalizePart(p, session)
	for _, ev := range evs {
		// The part id is the event's identity, so the live stream and the
		// backfill store one row for one part however often either sees
		// it. Without it every part of a type shared one fallback key and
		// all but the first were silently dropped.
		ev.UUID = p.ID
	}
	return evs
}

func normalizePart(p *PartDoc, session *SessionDoc) []*eventmodel.Event {
	now := p.timestamp()
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	meta := &store.SessionMeta{
		FilePath:   storageSessionPath(session.ProjectID, session.ID),
		IsSubagent: false,
	}
	project := session.Directory
	switch p.Type {
	case "text":
		return []*eventmodel.Event{newEvent(eventmodel.EventAssistantMessage, p.SessionID, project, now, map[string]any{
			"content": p.Text,
		}, meta, "")}
	case "reasoning":
		return []*eventmodel.Event{newEvent(eventmodel.EventAssistantMessage, p.SessionID, project, now, map[string]any{
			"content":         "",
			"thinkingContent": p.Reasoning,
			"hasThinking":     true,
		}, meta, "thinking")}
	case "tool":
		return normalizeToolPart(p, session, meta, now)
	case "step-finish":
		return []*eventmodel.Event{newEvent(eventmodel.EventResult, p.SessionID, project, now, map[string]any{
			"reason":  p.Reason,
			"costUsd": p.Cost,
			"tokens":  tokenUsageToMap(p.Tokens),
		}, meta, "step-finish")}
	case "file":
		return []*eventmodel.Event{newEvent(eventmodel.EventToolUse, p.SessionID, project, now, map[string]any{
			"toolName": "file",
			"toolInput": map[string]any{
				"mime":     p.Mime,
				"filename": p.Filename,
				"url":      p.URL,
			},
		}, meta, "attachment")}
	case "step-start":
		// a step-start carries no data anything reads; emit as meta
		return []*eventmodel.Event{newEvent(eventmodel.EventMeta, p.SessionID, project, now, map[string]any{
			"step": "start",
		}, meta, "raw:step-start")}
	default:
		return []*eventmodel.Event{newEvent(eventmodel.EventMeta, p.SessionID, project, now, map[string]any{
			"type": p.Type,
		}, meta, "raw:"+p.Type)}
	}
}

func normalizeToolPart(p *PartDoc, session *SessionDoc, meta *store.SessionMeta, now int64) []*eventmodel.Event {
	if p.State == nil {
		return []*eventmodel.Event{newEvent(eventmodel.EventMeta, p.SessionID, session.Directory, now, map[string]any{
			"part": "tool-no-state",
		}, meta, "raw:tool-no-state")}
	}
	project := session.Directory
	status := p.State.Status
	switch status {
	case "running", "pending":
		return []*eventmodel.Event{newEvent(eventmodel.EventToolUse, p.SessionID, project, now, map[string]any{
			"toolName":  p.Tool,
			"toolUseId": p.CallID,
			"toolInput": p.State.Input,
		}, meta, "")}
	case "completed", "error":
		// a single tool_result carrying both output and the original input
		// snapshot (so a viewer can render "what did the tool see")
		return []*eventmodel.Event{newEvent(eventmodel.EventToolResult, p.SessionID, project, now, map[string]any{
			"toolName":  p.Tool,
			"toolUseId": p.CallID,
			"toolInput": p.State.Input,
			"content":   p.State.Output,
			"isError":   status == "error",
		}, meta, "")}
	default:
		return []*eventmodel.Event{newEvent(eventmodel.EventMeta, p.SessionID, project, now, map[string]any{
			"status": status,
		}, meta, "raw:tool-status")}
	}
}

func newEvent(t eventmodel.EventType, sessionID, project string, ts int64, attrs map[string]any, meta *store.SessionMeta, subtype string) *eventmodel.Event {
	ev := &eventmodel.Event{
		SessionID: sessionID,
		Project:   project,
		TS:        ts,
		Type:      t,
		Subtype:   subtype,
		Source:    eventmodel.SourceOpenCode,
	}
	for k, v := range attrs {
		switch k {
		case "toolName":
			ev.ToolName, _ = v.(string)
		case "toolUseId":
			ev.ToolUseID, _ = v.(string)
		case "toolInput":
			if m, ok := v.(map[string]any); ok {
				ev.ToolInput = m
			}
		case "content":
			ev.Content, _ = v.(string)
		case "title":
			// session_state events carry the user's conversation title
			// in a "title" attr (mirrors the on-disk JSON field). Stash
			// it in Content too so the corpus indexer's sessionTitle
			// sees it as first_prompt fallback when no custom-title
			// event has fired yet.
			if t, _ := v.(string); t != "" {
				ev.Content = t
			}
		case "thinkingContent":
			ev.ThinkingContent, _ = v.(string)
		case "hasThinking":
			ev.HasThinking, _ = v.(bool)
		case "costUsd":
			if f, ok := v.(float64); ok {
				ev.CostUSD = f
			}
		case "tokens":
			if m, ok := v.(map[string]any); ok {
				ev.Tokens = tokenMapToEventmodel(m)
			} else if t, ok := v.(*TokenUsage); ok && t != nil {
				ev.Tokens = tokenUsageToEventmodel(t)
			}
		case "isError":
			ev.IsError, _ = v.(bool)
		case "reason":
			ev.TerminalReason, _ = v.(string)
		}
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	return ev
}

func tokenUsageToMap(t *TokenUsage) map[string]any {
	if t == nil {
		return nil
	}
	m := map[string]any{
		"total":     t.Total,
		"input":     t.Input,
		"output":    t.Output,
		"reasoning": t.Reasoning,
	}
	if t.Cache != nil {
		m["cacheRead"] = t.Cache.Read
		m["cacheWrite"] = t.Cache.Write
	}
	return m
}

func tokenUsageToEventmodel(t *TokenUsage) *eventmodel.TokenUsage {
	if t == nil {
		return nil
	}
	out := &eventmodel.TokenUsage{
		In:  t.Input,
		Out: t.Output,
	}
	if t.Cache != nil {
		out.CacheRead = t.Cache.Read
		out.CacheWrite = t.Cache.Write
	}
	return out
}

// tokenMapToEventmodel unpacks a generic map (the shape produced by
// tokenUsageToMap and by the JSON decoder) into an eventmodel.TokenUsage.
// Field names follow eventmodel's convention; unknown keys are ignored so
// future OpenCode schema additions round-trip safely.
func tokenMapToEventmodel(m map[string]any) *eventmodel.TokenUsage {
	if m == nil {
		return nil
	}
	out := &eventmodel.TokenUsage{}
	if v := toInt64(m["in"]); v != 0 {
		out.In = v
	} else if v := toInt64(m["input"]); v != 0 {
		out.In = v
	}
	if v := toInt64(m["out"]); v != 0 {
		out.Out = v
	} else if v := toInt64(m["output"]); v != 0 {
		out.Out = v
	}
	if v := toInt64(m["cacheRead"]); v != 0 {
		out.CacheRead = v
	} else if c, ok := m["cache"].(map[string]any); ok {
		if v := toInt64(c["read"]); v != 0 {
			out.CacheRead = v
		}
		if v := toInt64(c["write"]); v != 0 {
			out.CacheWrite = v
		}
	} else if v := toInt64(m["cacheWrite"]); v != 0 {
		out.CacheWrite = v
	}
	return out
}

// toInt64 is the polymorphic int64 helper used across the normalization
// paths: JSON decoding yields float64, Go assignments yield int64, and
// the OpenCode storage can yield either depending on the surface.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	}
	return 0
}

// ToIncoming converts a normalized event into the store.Incoming shape.
// SessionID / CWD / Project are filled by the ingest writer from meta.
func ToIncoming(ev *eventmodel.Event) store.Incoming {
	preview, hash, oversized := eventmodel.SplitOversizedString(ev.Content, 0)
	var toolInput []byte
	if ev.ToolInput != nil {
		toolInput, _ = json.Marshal(ev.ToolInput)
	}
	in := store.Incoming{
		UUID:            ev.UUID,
		ParentUUID:      ev.ParentUUID,
		SessionID:       ev.SessionID,
		Event:           string(ev.Type),
		Subtype:         ev.Subtype,
		Actor:           ev.Actor,
		ToolName:        ev.ToolName,
		ToolUseID:       ev.ToolUseID,
		ToolInput:       toolInput,
		Content:         preview,
		ContentHash:     hash,
		Oversized:       oversized,
		Model:           ev.Model,
		CostUSD:         ev.CostUSD,
		HasThinking:     ev.HasThinking,
		ThinkingContent: ev.ThinkingContent,
		IsError:         ev.IsError,
		TermReason:      ev.TerminalReason,
		DurationMs:      ev.DurationMs,
		Source:          string(ev.Source),
		TS:              ev.TS,
	}
	if ev.Tokens != nil {
		in.TokensIn = ev.Tokens.In
		in.TokensOut = ev.Tokens.Out
		in.TokCacheRead = ev.Tokens.CacheRead
		in.TokCacheWr = ev.Tokens.CacheWrite
	}
	return in
}

// --- SSE consumer -----------------------------------------------------------

type sseConsumer struct {
	baseURL string
	stop    context.CancelFunc
	done    chan struct{}
}

// sseMaxLine bounds one event on the stream. A tool result carrying a large
// file is one line, and a scanner that gives up on it ends the stream.
const sseMaxLine = 16 << 20

// StartSSE opens the event-stream consumer for one run. It runs until ctx is
// cancelled (the run's own context ends with the run), StopSSE is called, or
// the process is gone; the entry is removed when it ends. Reconnect is
// automatic with exponential backoff.
//
// The function returns immediately.
func (i *Ingest) StartSSE(ctx context.Context, runID, baseURL, sessionID string) error {
	if baseURL == "" {
		return errors.New("opencode: StartSSE: empty baseURL")
	}
	if runID == "" {
		return errors.New("opencode: StartSSE: empty run id")
	}
	i.mu.Lock()
	if _, ok := i.consumers[runID]; ok {
		i.mu.Unlock()
		return nil // already running
	}
	cctx, cancel := context.WithCancel(ctx)
	cons := &sseConsumer{
		baseURL: baseURL,
		stop:    cancel,
		done:    make(chan struct{}),
	}
	i.consumers[runID] = cons
	i.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			i.mu.Lock()
			if i.consumers[runID] == cons {
				delete(i.consumers, runID)
			}
			i.mu.Unlock()
		}()
		i.runSSE(cctx, cons, sessionID)
	}()
	return nil
}

// StopSSE ends a run's consumer and waits for it to finish.
func (i *Ingest) StopSSE(runID string) {
	i.mu.Lock()
	cons, ok := i.consumers[runID]
	i.mu.Unlock()
	if !ok {
		return
	}
	cons.stop()
	<-cons.done
}

// consumerCount is how many consumers are running.
func (i *Ingest) consumerCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.consumers)
}

func (i *Ingest) runSSE(ctx context.Context, cons *sseConsumer, sessionID string) {
	defer close(cons.done)
	backoff := 250 * time.Millisecond
	const maxBackoff = 8 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := i.consumeOnce(ctx, cons.baseURL, sessionID)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = 250 * time.Millisecond
			continue
		}
		// exponential backoff with cap
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// consumeOnce opens the stream, reads events until error / EOF / ctx done,
// normalizes them, and enqueues through the ingest writer.
func (i *Ingest) consumeOnce(ctx context.Context, baseURL, sessionID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	hc := &http.Client{Timeout: 0}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("opencode: SSE %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), sseMaxLine)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			continue
		}
		i.handleSSEEvent(ctx, raw, sessionID)
	}
	return scanner.Err()
}

// handleSSEEvent normalizes one SSE event into eventmodel.Events. The
// OpenCode SSE payload shape is {type, properties}; we keep the mapping
// local so the test fixtures can exercise each branch independently.
func (i *Ingest) handleSSEEvent(ctx context.Context, raw map[string]any, defaultSessionID string) {
	t, _ := raw["type"].(string)
	props, _ := raw["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	part, _ := props["part"].(map[string]any)
	sid, _ := props["sessionID"].(string)
	if sid == "" && part != nil {
		sid, _ = part["sessionID"].(string)
	}
	if sid == "" {
		sid = defaultSessionID
	}
	if sid == "" {
		// An event for no session would be indexed under an empty session
		// id: one fake session collecting every such event forever.
		return
	}
	project, _ := props["directory"].(string)
	sess := &SessionDoc{ID: sid, ProjectID: projectFromProjectString(project), Directory: project}
	meta := &store.SessionMeta{
		SessionID:  sid,
		CWD:        project,
		Project:    eventmodel.ProjectFromCWD(project),
		FilePath:   storageSessionPath(sess.ProjectID, sid),
		IsSubagent: false,
	}
	now := i.clock()
	switch t {
	case "message.updated", "message.part.updated":
		if part != nil {
			p := partFromMap(part)
			p.SessionID = sid
			if p.streaming() {
				return // stored once it is finished, under the same id
			}
			i.enqueue(ctx, NormalizePart(p, sess), meta)
		}
	case "session.updated", "session.created":
		ev := newEvent(eventmodel.EventSessionState, sid, project, now,
			map[string]any{"title": props["title"]}, meta, "session")
		i.enqueue(ctx, []*eventmodel.Event{ev}, meta)
	case "session.compacted":
		ev := newEvent(eventmodel.EventMeta, sid, project, now,
			map[string]any{"compacted": true}, meta, "compaction")
		i.enqueue(ctx, []*eventmodel.Event{ev}, meta)
	case "session.error":
		ev := newEvent(eventmodel.EventMeta, sid, project, now,
			map[string]any{"error": props["error"]}, meta, "raw:session-error")
		i.enqueue(ctx, []*eventmodel.Event{ev}, meta)
	case "permission.asked", "permission.asked.v2":
		// surface as eventmodel.EventPermission so the approval
		// flow treats it like the Claude permission_denied event
		toolName, _ := props["tool"].(string)
		input, _ := props["input"].(map[string]any)
		permissionID, _ := props["id"].(string)
		ev := newEvent(eventmodel.EventPermission, sid, project, now, map[string]any{
			"toolName":     toolName,
			"toolInput":    input,
			"permissionId": permissionID,
		}, meta, "permission_asked")
		i.enqueue(ctx, []*eventmodel.Event{ev}, meta)
	case "question.asked", "question.asked.v2":
		ev := newEvent(eventmodel.EventMeta, sid, project, now,
			map[string]any{"questions": props["questions"]}, meta, "raw:question-asked")
		i.enqueue(ctx, []*eventmodel.Event{ev}, meta)
	default:
		// rule 11: keep the raw spelling so future schema additions don't
		// silently lose events
		ev := newEvent(eventmodel.EventMeta, sid, project, now, raw, meta, "raw:"+t)
		i.enqueue(ctx, []*eventmodel.Event{ev}, meta)
	}
}

// partFromMap is the SSE / JSON part decoder; the OpenCode storage layout
// is identical to the SSE payload for "part" fields.
func partFromMap(m map[string]any) *PartDoc {
	p := &PartDoc{}
	if s, ok := m["id"].(string); ok {
		p.ID = s
	}
	if s, ok := m["messageID"].(string); ok {
		p.MessageID = s
	}
	if s, ok := m["type"].(string); ok {
		p.Type = s
	}
	if t, ok := m["time"].(map[string]any); ok {
		p.Time = t
	}
	if s, ok := m["text"].(string); ok {
		p.Text = s
	}
	if s, ok := m["reasoning"].(string); ok {
		p.Reasoning = s
	}
	if s, ok := m["callID"].(string); ok {
		p.CallID = s
	}
	if s, ok := m["tool"].(string); ok {
		p.Tool = s
	}
	if s, ok := m["mime"].(string); ok {
		p.Mime = s
	}
	if s, ok := m["filename"].(string); ok {
		p.Filename = s
	}
	if s, ok := m["url"].(string); ok {
		p.URL = s
	}
	if s, ok := m["reason"].(string); ok {
		p.Reason = s
	}
	if f, ok := m["cost"].(float64); ok {
		p.Cost = f
	}
	if t, ok := m["tokens"].(map[string]any); ok {
		p.Tokens = tokensFromMap(t)
	}
	if st, ok := m["state"].(map[string]any); ok {
		p.State = stateFromMap(st)
	}
	return p
}

func tokensFromMap(m map[string]any) *TokenUsage {
	out := &TokenUsage{}
	if v, ok := m["total"].(float64); ok {
		out.Total = int64(v)
	}
	if v, ok := m["input"].(float64); ok {
		out.Input = int64(v)
	}
	if v, ok := m["output"].(float64); ok {
		out.Output = int64(v)
	}
	if v, ok := m["reasoning"].(float64); ok {
		out.Reasoning = int64(v)
	}
	if c, ok := m["cache"].(map[string]any); ok {
		out.Cache = &TokenCache{}
		if v, ok := c["read"].(float64); ok {
			out.Cache.Read = int64(v)
		}
		if v, ok := c["write"].(float64); ok {
			out.Cache.Write = int64(v)
		}
	}
	return out
}

func stateFromMap(m map[string]any) *ToolState {
	out := &ToolState{}
	if s, ok := m["status"].(string); ok {
		out.Status = s
	}
	if s, ok := m["output"].(string); ok {
		out.Output = s
	}
	if s, ok := m["title"].(string); ok {
		out.Title = s
	}
	if in, ok := m["input"].(map[string]any); ok {
		out.Input = in
	}
	if t, ok := m["time"].(map[string]any); ok {
		out.Time = t
	}
	if md, ok := m["metadata"].(map[string]any); ok {
		out.Metadata = md
	}
	return out
}

func projectFromProjectString(s string) string {
	if s == "" {
		return ""
	}
	sum := sha1OfString(s)
	return sum[:12]
}

// enqueue converts the normalized events to store.Incoming and hands them
// to the ingest writer. Returns nil when no writer is wired (the boot path
// sets the writer once the ingest.Service is built).
func (i *Ingest) enqueue(ctx context.Context, evs []*eventmodel.Event, meta *store.SessionMeta) {
	if i.writer == nil || len(evs) == 0 {
		return
	}
	in := make([]store.Incoming, 0, len(evs))
	for _, ev := range evs {
		in = append(in, ToIncoming(ev))
	}
	_ = i.writer.EnqueueLive(ctx, in, meta)
}

// --- backfill ----------------------------------------------------------------

// BackfillAll walks ~/.local/share/opencode/storage and indexes every session
// whose storage changed since it was last indexed. Returns what it did.
//
// root defaults to ~/.local/share/opencode/storage; tests pass a temp dir.
//
// Events go through the shared writer like the live stream's, and carry the
// part's and message's own timestamps. Each indexed session gets a watermark
// (its own "updated" time), recorded only after the writer has committed its
// events, so the next boot skips it until OpenCode writes to it again.
func (i *Ingest) BackfillAll(ctx context.Context, root string) (BackfillReport, error) {
	rep := BackfillReport{}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return rep, err
		}
		root = filepath.Join(home, ".local", "share", "opencode", "storage")
	}
	if _, err := os.Stat(root); err != nil {
		return rep, nil // storage may not exist yet; not an error
	}

	// session/<projectID>/<sessionID>.json
	sessRoot := filepath.Join(root, "session")
	walkErr := filepath.WalkDir(sessRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw, rerr := os.ReadFile(path) //nolint:gosec // a file under the OpenCode storage root
		if rerr != nil {
			rep.Skipped++
			return nil
		}
		var s SessionDoc
		if jerr := json.Unmarshal(raw, &s); jerr != nil || s.ID == "" {
			rep.Skipped++
			return nil
		}
		// OpenCode storage writes timestamps nested under time.created /
		// time.updated; the flat fields stay zero without this.
		if s.CreatedAt == 0 {
			s.CreatedAt = toInt64(s.Raw["created"])
		}
		if s.UpdatedAt == 0 {
			s.UpdatedAt = toInt64(s.Raw["updated"])
		}
		projectID := filepath.Base(filepath.Dir(path))
		if s.ProjectID == "" {
			s.ProjectID = projectID
		}
		if i.st != nil && s.UpdatedAt > 0 {
			if mark, err := i.st.OpenCodeBackfillWatermark(ctx, s.ID); err == nil && mark >= s.UpdatedAt {
				rep.Unchanged++
				return nil
			}
		}
		meta := SessionMetaFromSession(&s, projectID)
		// Adopt the session into managed_sessions so the runs list shows
		// it. The row is read-only until the user spawns a TUI for it:
		// control_port=0 greys out the control sheet, and the session id
		// lets a new run resume it.
		i.adoptManagedSession(ctx, &s, projectID)
		ev := newEvent(eventmodel.EventSessionState, s.ID, s.Directory, s.CreatedAt, map[string]any{
			"title":   s.Title,
			"slug":    s.Slug,
			"version": s.Version,
		}, meta, "session")
		i.enqueue(ctx, []*eventmodel.Event{ev}, meta)
		rep.Sessions++
		i.backfillSession(ctx, root, &s, meta, &rep)
		if err := i.markIndexed(ctx, &s); err != nil {
			return err
		}
		return nil
	})
	if walkErr != nil && ctx.Err() == nil {
		return rep, walkErr
	}
	return rep, ctx.Err()
}

// markIndexed records a session's watermark once its events have committed.
func (i *Ingest) markIndexed(ctx context.Context, s *SessionDoc) error {
	if i.st == nil {
		return nil
	}
	if f, ok := i.writer.(flusher); ok {
		if err := f.Flush(ctx); err != nil {
			return err
		}
	}
	if s.Title != "" {
		// OpenCode user messages carry no text in the index, so the session
		// would have no first prompt to show; its title stands in. Only now:
		// every committed batch recomputes the first prompt from events.
		if err := i.st.SetSessionFirstPromptIfEmpty(ctx, s.ID, s.Title); err != nil {
			return err
		}
	}
	if s.UpdatedAt <= 0 {
		return nil // nothing to compare against next time
	}
	return i.st.SetOpenCodeBackfillWatermark(ctx, s.ID, s.UpdatedAt)
}

// adoptManagedSession upserts a managed_sessions row for an on-disk
// OpenCode session so the runs list shows it. Idempotent: a re-run refreshes
// the title and times.
func (i *Ingest) adoptManagedSession(ctx context.Context, s *SessionDoc, projectID string) {
	if i.st == nil {
		return
	}
	// The run id is derived from the OpenCode session id, so it is stable
	// across boots. A run spawned for the session has its own id.
	row := &store.ManagedSession{
		ID:             "opencode-adopted-" + s.ID,
		SessionID:      s.ID,
		Kind:           "tty",
		CWD:            s.Directory,
		Project:        eventmodel.ProjectFromCWD(s.Directory),
		Prompt:         s.Title, // the runs list shows the prompt as the label of a run with no title
		State:          "stopped",
		StartedAt:      s.CreatedAt,
		UpdatedAt:      i.clock(),
		EndedAt:        s.UpdatedAt,
		CreatedBy:      adoptMarker,
		Engine:         "opencode",
		TerminalReason: "adopted at agentd boot from on-disk storage",
	}
	if err := i.st.UpsertManagedSession(ctx, row); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-backfill: adopt %s: %v\n", s.ID, err)
	}
}

// adoptMarker is exported so tests can assert that a row came from the
// adoption path (vs being actively spawned).
const adoptMarker = "adopted:opencode"

// AdoptMarkerOf returns the CreatedBy value the adoption path stamps on
// every adopted OpenCode row. Tests use it to confirm the row origin.
func AdoptMarkerOf() string { return adoptMarker }

// backfillSession walks message/<sessionID>/<messageID>.json and the matching
// part/<messageID>/<partID>.json files and hands their events to the writer.
func (i *Ingest) backfillSession(ctx context.Context, root string, sess *SessionDoc, meta *store.SessionMeta, rep *BackfillReport) {
	msgDir := filepath.Join(root, "message", sess.ID)
	entries, err := os.ReadDir(msgDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(msgDir, e.Name())) //nolint:gosec // under the storage root
		if rerr != nil {
			continue
		}
		var m MessageDoc
		if jerr := json.Unmarshal(raw, &m); jerr != nil {
			continue
		}
		created := extractCreated(m.Time)
		if m.Role == "user" {
			ev := newEvent(eventmodel.EventUserMessage, sess.ID, sess.Directory, created, map[string]any{
				"actor": m.Agent,
			}, meta, "")
			ev.UUID = m.ID
			i.enqueue(ctx, []*eventmodel.Event{ev}, meta)
		}
		// parts in file-name order (the ids embed their creation time)
		partDir := filepath.Join(root, "part", m.ID)
		partEntries, perr := os.ReadDir(partDir)
		if perr != nil {
			continue
		}
		var evs []*eventmodel.Event
		for _, pe := range partEntries {
			if pe.IsDir() || !strings.HasSuffix(pe.Name(), ".json") {
				continue
			}
			praw, err := os.ReadFile(filepath.Join(partDir, pe.Name())) //nolint:gosec // under the storage root
			if err != nil {
				continue
			}
			var p PartDoc
			if jerr := json.Unmarshal(praw, &p); jerr != nil {
				continue
			}
			if p.timestamp() == 0 && created > 0 {
				// A part without a clock of its own happened with its message.
				p.Time = map[string]any{"start": created}
			}
			evs = append(evs, NormalizePart(&p, sess)...)
		}
		i.enqueue(ctx, evs, meta)
		rep.Parts += len(evs)
	}
}

// BackfillReport summarises one BackfillAll pass.
type BackfillReport struct {
	// Sessions were indexed, Unchanged skipped by their watermark, Skipped
	// unreadable.
	Sessions  int
	Unchanged int
	Skipped   int
	Parts     int
}

func extractCreated(t map[string]any) int64 {
	return toInt64(t["created"])
}

// --- helpers ----------------------------------------------------------------

// sha1OfString fingerprints a directory into a stable project id.
func sha1OfString(s string) string {
	return eventmodel.ContentHashOfString(s)
}
