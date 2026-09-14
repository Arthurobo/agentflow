package parse

import (
	"encoding/json"
	"strings"
	"time"

	gojson "github.com/goccy/go-json"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
)

// Parsed is the result of normalizing a single raw record: the raw bytes plus
// the one-or-more canonical events it expands into (an assistant record with
// tool_use blocks yields one assistant_message event plus one tool_use event
// per block — rules 2 & 3).
type Parsed struct {
	// Raw is the exact source record bytes (retained for rule 10 round-trip).
	Raw []byte
	// RawLen is the length of the raw line (always set, even when Raw is
	// dropped by RetainRaw=false).
	RawLen int
	// RawType echoes the raw top-level `type` (e.g. "assistant",
	// "queue-operation") or "" for non-object lines.
	RawType string
	// RawSubtype echoes the raw system subtype / operation where present.
	RawSubtype string
	// Events holds the normalized events for this record (never empty).
	Events []*eventmodel.Event
}

// Normalize maps one raw river-format record (JSONL line, stream-json line, or
// hook payload) to canonical events. It is shared by every reader so the three
// capture paths cannot drift apart. It never panics and never returns an empty
// Events slice for a parseable record.
func Normalize(raw []byte, src eventmodel.Source) (*Parsed, error) {
	rec, err := decodeRaw(raw)
	if err != nil {
		return nil, err
	}
	if rec.nonObject {
		ev := &eventmodel.Event{Source: src, Type: eventmodel.EventMeta, Subtype: "raw:non-object"}
		return &Parsed{Raw: raw, Events: []*eventmodel.Event{ev}}, nil
	}

	typ := rec.getString("type")
	p := &Parsed{Raw: raw, RawType: typ}

	switch typ {
	case "user":
		return p, p.parseUser(rec, src)
	case "assistant":
		return p, p.parseAssistant(rec, src)
	case "system":
		return p, p.parseSystem(rec, src)
	case "queue-operation":
		return p, p.parseQueueOp(rec, src)
	case "rate_limit_event":
		return p, p.parseRateLimit(rec, src)
	case "result":
		return p, p.parseResult(rec, src)
	case "command_lifecycle":
		return p, p.parseCommandLifecycle(rec, src)
	case "mode", "permission-mode", "custom-title", "agent-name", "ai-title",
		"last-prompt", "atis-latch", "pr-link", "file-history-snapshot",
		"file-history-delta", "bridge-session", "summary":
		return p, p.parseSnapshot(rec, src, typ)
	case "attachment":
		return p, p.parseAttachment(rec, src)
	default:
		// rule 11: unknown top-level type → meta, subtype "raw:<type>".
		ev := p.base(rec, src)
		ev.Type = eventmodel.EventMeta
		ev.Subtype = eventmodel.SubtypeRawPrefix + typ
		ev.Content = rec.getString("content")
		p.Events = append(p.Events, ev)
		return p, nil
	}
}

// base fills every field that is common to all record kinds regardless of type.
func (p *Parsed) base(rec rawRecord, src eventmodel.Source) *eventmodel.Event {
	e := &eventmodel.Event{Source: src, Raw: p.Raw}

	// rule 1 — dual spelling. `sessionId` (JSONL, file owner) is the
	// canonical value; `session_id` (stream/subagent origin) is used only as a
	// fallback for the canonical field, or captured as originSessionId when the
	// two differ (forwarded subagent content, 20,134 such events in corpus).
	e.SessionID = rec.getString("sessionId")
	if sid := rec.getString("session_id"); sid != "" {
		if e.SessionID == "" {
			e.SessionID = sid
		} else if e.SessionID != sid {
			e.OriginSessionID = sid
		}
	}

	e.UUID = rec.getString("uuid")
	e.ParentUUID = rec.getString("parentUuid")
	e.IsSidechain = rec.getBool("isSidechain")
	e.IsMeta = rec.getBool("isMeta")
	e.AgentID = rec.getString("agentId")
	e.Origin = rec.getMap("origin")
	e.TS = tsFromTimestamp(rec.getString("timestamp"))
	e.CWD = rec.getString("cwd")
	e.Project = eventmodel.ProjectFromCWD(e.CWD)
	e.SupersedesUuids = rec.getStringSlice("supersedesUuids")
	e.RawVersion = rec.getString("version")
	e.RawSchemaVersion = schemaVersionFrom(e.RawVersion)
	return e
}

// parseUser handles `user` records: plain prompts, tool_result content blocks,
// and cross-session peer messages.
func (p *Parsed) parseUser(rec rawRecord, src eventmodel.Source) error {
	e := p.base(rec, src)
	e.Type = eventmodel.EventUserMessage
	e.Actor = "user"

	if o := e.Origin; o != nil {
		if kind, _ := o["kind"].(string); kind == "peer" {
			name, _ := o["name"].(string)
			if name == "" {
				name, _ = o["from"].(string)
			}
			e.Actor = "peer:" + name
		}
	}

	var blocks []contentBlock
	if content, ok := rec.messageContent(); ok {
		blocks = decodeBlocks(content)
	}
	if len(blocks) == 0 {
		// stream-json writes tool results in a top-level tool_use_result object.
		if tur := rec.getRaw("tool_use_result"); tur != nil {
			if blk := toolResultFromObject(tur); blk != nil {
				blocks = append(blocks, *blk)
			}
		}
	}
	if len(blocks) == 0 {
		p.Events = append(p.Events, e) // plain (possibly empty) user message
		return nil
	}
	// Emit the user_message only when the record carries readable content
	// (text/image blocks); a user record holding nothing but tool results maps
	// to the tool_result events alone — an empty shell user_message would only
	// clutter the timeline.
	hasText := false
	for i := range blocks {
		if blocks[i].kind == "text" || blocks[i].kind == "image" {
			hasText = true
			break
		}
	}
	if hasText {
		p.Events = append(p.Events, e)
	}
	for i := range blocks {
		b := &blocks[i]
		switch b.kind {
		case "text", "image":
			if !hasText {
				continue
			}
			// user_message for human-typed text; images are represented as a
			// marker (base64 in raw only, never in content).
			if b.kind == "image" {
				e.Content, e.ContentHash, _ = eventmodel.SplitOversized([]byte("[image block — base64 retained in raw]"), 0)
			} else {
				e.Content, e.ContentHash, e.OversizedContent = splitContent(b.text)
			}
		case "tool_result":
			te := e.Copy()
			te.Type = eventmodel.EventToolResult
			te.Actor = "system" // tool feedback, not a conversation participant
			te.ToolUseID = b.toolUseID
			te.ToolName = b.toolName
			te.Content, te.ContentHash, te.OversizedContent = splitContent(b.text)
			te.ToolInput = nil
			p.Events = append(p.Events, te)
		}
	}
	return nil
}

// parseAssistant handles `assistant` records: whole-message text + thinking
// blocks plus any tool_use blocks (each became a separate tool_use event).
func (p *Parsed) parseAssistant(rec rawRecord, src eventmodel.Source) error {
	e := p.base(rec, src)
	e.Type = eventmodel.EventAssistantMessage
	e.Actor = "assistant"
	if e.IsSidechain {
		if e.AgentID != "" {
			e.Actor = "subagent:" + e.AgentID
		}
	}

	if content, ok := rec.messageContent(); ok {
		_ = content
	}
	e.Model = rec.messageModel()
	if usage := rec.messageUsage(); usage != nil {
		e.Tokens = &eventmodel.TokenUsage{
			In:         usage.In,
			Out:        usage.Out,
			CacheRead:  usage.CacheRead,
			CacheWrite: usage.CacheWrite,
		}
	}

	var blocks []contentBlock
	if content, ok := rec.messageContent(); ok {
		blocks = decodeBlocks(content)
	}

	// Split blocks by kind: text → Content (the answer), thinking →
	// ThinkingContent (collapsible reasoning). Thinking is never merged into
	// the answer — the old K4 assumption (thinking arrives empty) no longer
	// holds on current models.
	var text, thinking strings.Builder
	for i := range blocks {
		b := &blocks[i]
		switch b.kind {
		case "text":
			if b.text != "" {
				text.WriteString(b.text)
				text.WriteString("\n")
			}
		case "thinking":
			if b.text != "" {
				thinking.WriteString(b.text)
				thinking.WriteString("\n")
			}
			e.HasThinking = true
			e.ThinkingSignature = b.thinkingSignature
		}
	}
	e.Content, e.ContentHash, e.OversizedContent = splitContent(strings.TrimSuffix(text.String(), "\n"))
	e.ThinkingContent = strings.TrimSuffix(thinking.String(), "\n")

	p.Events = append(p.Events, e)

	for i := range blocks {
		b := &blocks[i]
		if b.kind != "tool_use" {
			continue
		}
		te := *e
		te.Type = eventmodel.EventToolUse
		te.ToolName = b.toolName
		te.ToolInput = b.toolInput
		te.ToolUseID = b.toolUseID
		te.Content = ""
		te.ContentHash = ""
		te.OversizedContent = false
		te.ParentUUID = e.UUID
		p.Events = append(p.Events, &te)
	}
	return nil
}

func (p *Parsed) parseSystem(rec rawRecord, src eventmodel.Source) error {
	e := p.base(rec, src)
	e.Type = eventmodel.EventSystemMeta
	e.Actor = "system"
	sub := rec.getString("subtype")
	e.Subtype = sub
	switch sub {
	case "permission_denied":
		e.Type = eventmodel.EventPermission
		e.ToolName = rec.getString("tool_name")
		e.ToolUseID = rec.getString("tool_use_id")
		e.Content = rec.getString("message")
	case "task_started", "task_progress", "task_updated", "task_notification":
		e.Type = eventmodel.EventTask
	case "init":
		e.Subtype = eventmodel.SubtypeInit
	default:
		e.Content = rec.getString("content")
	}
	if uuids := rec.getStringSlice("retractedMessageUuids"); len(uuids) > 0 {
		e.RetractedUuids = uuids
	}
	p.Events = append(p.Events, e)
	return nil
}

func (p *Parsed) parseQueueOp(rec rawRecord, src eventmodel.Source) error {
	e := p.base(rec, src)
	e.Type = eventmodel.EventQueueOp
	e.Actor = "system"
	e.Subtype = rec.getString("operation")
	e.Content = rec.getString("content")
	p.Events = append(p.Events, e)
	return nil
}

func (p *Parsed) parseRateLimit(rec rawRecord, src eventmodel.Source) error {
	e := p.base(rec, src)
	e.Type = eventmodel.EventRateLimit
	e.Actor = "system"
	p.Events = append(p.Events, e)
	return nil
}

func (p *Parsed) parseResult(rec rawRecord, src eventmodel.Source) error {
	e := p.base(rec, src)
	e.Type = eventmodel.EventResult
	e.Actor = "system"
	e.Subtype = rec.getString("subtype")
	e.CostUSD = rec.getFloat("total_cost_usd")
	e.IsError = rec.getBool("is_error")
	e.TerminalReason = rec.getString("terminal_reason")
	e.TTFTMs = rec.getIntCoerce("ttft_ms")
	e.DurationMs = rec.getIntCoerce("duration_ms")
	e.PermissionDenials = rec.getRaw("permission_denials")
	e.Content, e.ContentHash, e.OversizedContent = splitContent(rec.getString("result"))
	if usageRaw := rec.getRaw("usage"); usageRaw != nil {
		usage := tokensFromRaw(usageRaw)
		if usage != nil {
			e.Tokens = usage
		}
	}
	p.Events = append(p.Events, e)
	return nil
}

func (p *Parsed) parseCommandLifecycle(rec rawRecord, src eventmodel.Source) error {
	// Observed live but not in corpus; treated as opaque meta until
	// corpus presence matures the schema.
	e := p.base(rec, src)
	e.Type = eventmodel.EventMeta
	e.Actor = "system"
	e.Subtype = "command_lifecycle"
	e.Content = rec.getString("message")
	p.Events = append(p.Events, e)
	return nil
}

// parseSnapshot maps the small session-snapshot records (rule 4): event
// session_state with subtype = raw type, plus a readable content value when the
// record carries one.
func (p *Parsed) parseSnapshot(rec rawRecord, src eventmodel.Source, typ string) error {
	e := p.base(rec, src)
	e.Type = eventmodel.EventSessionState
	e.Actor = "system"
	e.Subtype = typ
	switch typ {
	case "mode":
		e.Content = rec.getString("mode")
	case "permission-mode":
		e.Content = rec.getString("permissionMode")
	case "custom-title":
		e.Content = rec.getString("customTitle")
	case "agent-name":
		e.Content = rec.getString("agentName")
	case "ai-title":
		e.Content = rec.getString("aiTitle")
	case "summary":
		// the compact-list session name claude writes for `claude -r` (also
		// used as the first line of follow-up session files) — this IS the
		// session's name.
		e.Content = rec.getString("summary")
	case "last-prompt":
		e.Content = rec.getString("lastPrompt")
	case "pr-link":
		e.Content = rec.getString("prUrl")
	default:
		e.Content = rec.getString("content")
	}
	p.Events = append(p.Events, e)
	return nil
}

// parseAttachment maps top-level `attachment` records. Per (deferred
// note) attachments are meta events with subtype "attachment:<type>".
func (p *Parsed) parseAttachment(rec rawRecord, src eventmodel.Source) error {
	e := p.base(rec, src)
	e.Type = eventmodel.EventMeta
	e.Actor = "system"
	sub := "attachment"
	if attRaw := rec.getRaw("attachment"); attRaw != nil {
		var att struct {
			Type string `json:"type"`
		}
		if err := gojson.Unmarshal(attRaw, &att); err == nil && att.Type != "" {
			sub = "attachment:" + att.Type
		}
	}
	e.Subtype = sub
	p.Events = append(p.Events, e)
	return nil
}

// --- shared helpers ----------------------------------------------------------

// splitContent applies the rule 10 oversized-content policy and always
// computes the content hash so the dedupe key (rule 9) is available.
func splitContent(s string) (content string, hash string, oversized bool) {
	return eventmodel.SplitOversized([]byte(s), eventmodel.DefaultOversizedThreshold)
}

// tsFromTimestamp parses RFC3339(Nano) timestamps ("2026-07-28T14:35:34.705Z")
// into epoch milliseconds. Unparseable/absent timestamps yield 0.
func tsFromTimestamp(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// schemaVersionFrom derives the integer rawSchemaVersion from a raw version
// string — the major segment of "2.1.220" → 2. Records without a version field
// (all snapshot types in the corpus) yield 0; rawVersion retains the literal
// string either way.
func schemaVersionFrom(version string) int {
	if version == "" {
		return 0
	}
	major := strings.SplitN(version, ".", 2)[0]
	var n int
	for _, c := range major {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func tokensFromRaw(raw json.RawMessage) *eventmodel.TokenUsage {
	v := usageFromRaw(raw)
	if v == nil {
		return nil
	}
	return &eventmodel.TokenUsage{
		In:         v.In,
		Out:        v.Out,
		CacheRead:  v.CacheRead,
		CacheWrite: v.CacheWrite,
	}
}
