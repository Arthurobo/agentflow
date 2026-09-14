// Package eventmodel defines the canonical normalized event shape for Agent
// Flow. It is the single output model for every
// capture path (JSONL tailer, stream-json, hooks) and must not depend on any
// other package in the module — it is the bottom of the layering
// eventmodel -> parse -> tailer.
package eventmodel

import (
	"crypto/sha256"
	"encoding/json"
	"strconv"
)

// Source identifies which capture path produced an event.
type Source string

const (
	// SourceTailer identifies events normalized from appended JSONL transcript
	// lines (live tailing of ~/.claude/projects/**/*.jsonl).
	SourceTailer Source = "tailer"
	// SourceHook identifies events normalized from hook payloads (stdin JSON).
	SourceHook Source = "hook"
	// SourceStream identifies events normalized from the stream-json stdout
	// protocol of `claude --output-format stream-json`.
	SourceStream Source = "stream-json"
	// SourceBackfill identifies events produced by a bulk corpus scan (
	// historical import; semantically identical to tailer minus liveness).
	SourceBackfill Source = "backfill"
	// SourceOpenCode identifies events normalized from the OpenCode TUI's
	// SSE event stream (GET /event) and from backfill walks of
	// the OpenCode storage tree.
	SourceOpenCode Source = "opencode"
)

// EventType is the canonical event discriminator.
//
// The complete vocabulary:
//
//	EventUserMessage — a human (or peer session) user message
//	EventAssistantMessage — a whole-message assistant reply (rule 3)
//	EventToolUse — a tool_use content block (rule 2)
//	EventToolResult — a tool_result content block (rule 2)
//	EventSessionState — snapshot records (mode, titles, history, ...)
//	EventRetraction — retraction surgery
//	EventPermission — permission_denied (system/permission_denied)
//	EventHook — a hook payload record
//	EventQueueOp — queue-operation background job events
//	EventMeta — opaque/unknown records (rule 11)
//	EventWorkflow — workflow records (reserved)
//	EventSystemMeta — system records (init, turn_duration, ...)
//	EventRateLimit — rate_limit_event (stream only)
//	EventTask — system/task_* progress events (stream only)
//	EventResult — result ledger (stream only)
type EventType string

// EventType values — see the EventType documentation above.
const (
	EventUserMessage      EventType = "user_message"
	EventAssistantMessage EventType = "assistant_message"
	EventToolUse          EventType = "tool_use"
	EventToolResult       EventType = "tool_result"
	EventSessionState     EventType = "session_state"
	EventRetraction       EventType = "retraction"
	EventPermission       EventType = "permission"
	EventHook             EventType = "hook"
	EventQueueOp          EventType = "queue_op"
	EventMeta             EventType = "meta"
	EventWorkflow         EventType = "workflow"
	EventSystemMeta       EventType = "system_meta"
	EventRateLimit        EventType = "rate_limit"
	EventTask             EventType = "task"
	EventResult           EventType = "result"
)

// Subtype constants shared across readers. The general rule: `subtype` echoes
// the raw `type` value (for session snapshot records) or the raw system
// `subtype` value (for system records), so new upstream values need no code
// changes — unknown ones keep their raw spelling verbatim.
const (
	SubtypeInit             = "init"
	SubtypeThinkingTokens   = "thinking_tokens"
	SubtypePermissionDenied = "permission_denied"
	// SubtypeRawPrefix is prepended to unknown/opaque raw top-level types:
	// event=meta, subtype="raw:<type>".
	SubtypeRawPrefix = "raw:"
)

// TokenUsage is the normalized model token/cost breakdown.
type TokenUsage struct {
	In         int64 `json:"in,omitempty"`
	Out        int64 `json:"out,omitempty"`
	CacheRead  int64 `json:"cacheRead,omitempty"`
	CacheWrite int64 `json:"cacheWrite,omitempty"`
}

// Event is the canonical normalized event.
//
// Raw retains the exact source record bytes verbatim so nothing is ever lost;
// it is not serialized (the `json:"-"` tag) because Raw is a debugging /
// round-trip aid, not part of the on-the-wire payload. ccdump prints Raw next
// to the normalized form.
type Event struct {
	MachineID         string         `json:"machineId,omitempty"`
	SessionID         string         `json:"sessionId,omitempty"`
	OriginSessionID   string         `json:"originSessionId,omitempty"`
	Project           string         `json:"project,omitempty"`
	CWD               string         `json:"cwd,omitempty"`
	Seq               int64          `json:"seq,omitempty"`
	TS                int64          `json:"ts,omitempty"`
	Type              EventType      `json:"event"`
	Subtype           string         `json:"subtype,omitempty"`
	Actor             string         `json:"actor,omitempty"`
	UUID              string         `json:"uuid,omitempty"`
	ParentUUID        string         `json:"parentUuid,omitempty"`
	IsSidechain       bool           `json:"isSidechain,omitempty"`
	IsMeta            bool           `json:"isMeta,omitempty"`
	ToolName          string         `json:"toolName,omitempty"`
	ToolInput         map[string]any `json:"toolInput,omitempty"`
	ToolUseID         string         `json:"toolUseId,omitempty"`
	Content           string         `json:"content,omitempty"`
	ContentHash       string         `json:"contentHash,omitempty"`
	Tokens            *TokenUsage    `json:"tokens,omitempty"`
	Model             string         `json:"model,omitempty"`
	CostUSD           float64        `json:"costUsd,omitempty"`
	RawSchemaVersion  int            `json:"rawSchemaVersion,omitempty"`
	RawVersion        string         `json:"rawVersion,omitempty"`
	Retracted         bool           `json:"retracted,omitempty"`
	RetractedUuids    []string       `json:"retractedUuids,omitempty"`
	SupersedesUuids   []string       `json:"supersedesUuids,omitempty"`
	HasThinking       bool           `json:"hasThinking,omitempty"`
	ThinkingSignature string         `json:"thinkingSignature,omitempty"`
	// ThinkingContent carries the reasoning-block text separately from the
	// answer (Content) so renderers can collapse it. Empty on sources that
	// redact thinking.
	ThinkingContent  string         `json:"thinkingContent,omitempty"`
	Origin           map[string]any `json:"origin,omitempty"`
	AgentID          string         `json:"agentId,omitempty"`
	FilePath         string         `json:"filePath,omitempty"`
	FileOffset       int64          `json:"fileOffset,omitempty"`
	Source           Source         `json:"source"`
	OversizedContent bool           `json:"oversizedContent,omitempty"`

	// Result-only telemetry ( rule 6: `result` events carry the
	// authoritative cost/perf/permission-denial ledger for a session).
	IsError           bool            `json:"isError,omitempty"`
	TerminalReason    string          `json:"terminalReason,omitempty"`
	TTFTMs            int64           `json:"ttftMs,omitempty"`
	DurationMs        int64           `json:"durationMs,omitempty"`
	PermissionDenials json.RawMessage `json:"permissionDenials,omitempty"`

	// Raw retains the exact source record bytes for round-tripping, debugging
	// and the tolerant-parse tenet (unknown fields never crash, never lost).
	Raw []byte `json:"-"`
}

// Copy returns a deep-enough copy for derived events: slices (retracted/
// supersedes lists, Raw) and maps (toolInput, origin) are copied so mutating
// the derived event never aliases the parent's data.
func (e *Event) Copy() *Event {
	c := *e
	if e.RetractedUuids != nil {
		c.RetractedUuids = append([]string(nil), e.RetractedUuids...)
	}
	if e.SupersedesUuids != nil {
		c.SupersedesUuids = append([]string(nil), e.SupersedesUuids...)
	}
	if e.ToolInput != nil {
		c.ToolInput = make(map[string]any, len(e.ToolInput))
		for k, v := range e.ToolInput {
			c.ToolInput[k] = v
		}
	}
	if e.Origin != nil {
		c.Origin = make(map[string]any, len(e.Origin))
		for k, v := range e.Origin {
			c.Origin[k] = v
		}
	}
	if e.PermissionDenials != nil {
		c.PermissionDenials = append(json.RawMessage(nil), e.PermissionDenials...)
	}
	return &c
}

// ContentHashPrefix is the prefix prepended to SHA-256 digests.
const ContentHashPrefix = "sha256:"

// ContentHashOfString computes the canonical content digest "sha256:<hex>".
func ContentHashOfString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return ContentHashPrefix + hexEncode(sum[:])
}

// DefaultOversizedThreshold is 64 KiB per rule 10.
const DefaultOversizedThreshold = 64 * 1024

// DefaultTruncatedPreview is how many bytes of oversized content are kept in
// `content` as a preview; the rest is addressed by ContentHash (blob store in
// ).
const DefaultTruncatedPreview = 4096

// SplitOversized applies rule 10: content larger than max stays
// addressable by hash while `content` carries a short preview plus an explicit
// truncation notice. It is safe to call on any content; when content is small
// it is returned verbatim with oversized=false.
func SplitOversized(content []byte, threshold int) (preview string, hash string, oversized bool) {
	return SplitOversizedString(string(content), threshold)
}

// SplitOversizedString is the string form of SplitOversized (single copy on
// input instead of up to two).
func SplitOversizedString(s string, threshold int) (preview string, hash string, oversized bool) {
	if len(s) == 0 {
		return "", "", false
	}
	hash = ContentHashOfString(s)
	if threshold <= 0 {
		threshold = DefaultOversizedThreshold
	}
	if len(s) <= threshold {
		return s, hash, false
	}
	keep := DefaultTruncatedPreview
	if keep >= len(s) {
		keep = len(s)
	}
	return s[:keep] + "\n…[truncated, full content is " + strconv.Itoa(len(s)) + " bytes keyed by " + hash + " — blob store arrives in ]", hash, true
}

// hexEncode is a tiny hex encoder so eventmodel needs no imports beyond crypto.
func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0x0f]
	}
	return string(out)
}
