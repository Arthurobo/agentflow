package parse

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	gojson "github.com/goccy/go-json"
)

// rawRecord is a maximally tolerant view of a raw Claude Code record.
//
// Decode strategy (chosen for throughput — encoding/json was the recorded
// bottleneck on the 535 MB corpus; see ): a single-pass struct decode
// (strict) is attempted first, pulling in one scan exactly the fields the
// parsers read. Lines whose shapes deviate (a wrong-typed field, a string
// where an object is expected, ...) fall back to the tolerant map view with
// per-field coercion — that fallback is the safety net for tenet 7 (unknown
// fields never crash) and keeps behavior byte-identical to the map-only
// parser.
type rawRecord struct {
	strict *strictV1
	m      map[string]gojson.RawMessage // tolerant fallback view
	// nonObject is true for valid-JSON non-object lines (bare strings, arrays,
	// numbers) which map to an opaque meta event.
	nonObject bool
}

// strictV1 mirrors every field the parsers actually read off a raw record. The
// users of each field are noted; send anything exotic to the fallback instead.
type strictV1 struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// "message" is an OBJECT on user/assistant records but a STRING on
	// permission_denied-style records; a string payload fails this struct
	// decode and that line takes the tolerant path — deliberate.
	Message messageV1 `json:"message"`

	SessionID    string `json:"sessionId"`
	SessionSnake string `json:"session_id"`
	AgentIDSnake string `json:"agent_id"`
	UUID         string `json:"uuid"`
	ParentUUID   string `json:"parentUuid"`
	AgentID      string `json:"agentId"`
	Timestamp    string `json:"timestamp"`
	CWD          string `json:"cwd"`
	Version      string `json:"version"`
	Operation    string `json:"operation"`

	IsSidechain bool `json:"isSidechain"`
	IsMeta      bool `json:"isMeta"`

	Origin        gojson.RawMessage `json:"origin"`
	Content       gojson.RawMessage `json:"content"`
	ToolUseResult gojson.RawMessage `json:"tool_use_result"`
	Attachment    gojson.RawMessage `json:"attachment"`
	Usage         gojson.RawMessage `json:"usage"`
	ToolInput     gojson.RawMessage `json:"tool_input"`
	ToolResponse  gojson.RawMessage `json:"tool_response"`
	Prompt        gojson.RawMessage `json:"prompt"`
	Question      gojson.RawMessage `json:"question"`
	LastAssistant gojson.RawMessage `json:"last_assistant_message"`
	Result        gojson.RawMessage `json:"result"`
	TTFTMs        gojson.RawMessage `json:"ttft_ms"`
	DurationMs    gojson.RawMessage `json:"duration_ms"`
	PermDenials   gojson.RawMessage `json:"permission_denials"`

	ToolName            string `json:"tool_name"`
	ToolUseID           string `json:"tool_use_id"`
	HookEventName       string `json:"hook_event_name"`
	TranscriptPath      string `json:"transcript_path"`
	AgentTranscriptPath string `json:"agent_transcript_path"`

	Retracted  []string `json:"retractedMessageUuids"`
	Supersedes []string `json:"supersedesUuids"`

	CostUSD        float64 `json:"total_cost_usd"`
	IsError        bool    `json:"is_error"`
	TerminalReason string  `json:"terminal_reason"`

	// --- session snapshot scalars (rule 4) ---
	Mode           string `json:"mode"`
	PermissionMode string `json:"permissionMode"`
	CustomTitle    string `json:"customTitle"`
	AgentName      string `json:"agentName"`
	AITitle        string `json:"aiTitle"`
	Summary        string `json:"summary"`
	LastPrompt     string `json:"lastPrompt"`
	ATIS           string `json:"atis"`
	PrURL          string `json:"prUrl"`
}

// messageV1 is the typed view of `message` for user/assistant records.
type messageV1 struct {
	Role    string            `json:"role"`
	Model   string            `json:"model"`
	Content gojson.RawMessage `json:"content"`
	Usage   gojson.RawMessage `json:"usage"`
}

// decodeRaw parses a raw line. ErrCorruptLine is returned only for input that
// is not valid JSON at all (or not an object and not representable).
func decodeRaw(raw []byte) (rawRecord, error) {
	var s strictV1
	if err := gojson.Unmarshal(raw, &s); err == nil {
		return rawRecord{strict: &s}, nil
	}
	// Tolerant fallback: any shape the strict struct cannot hold.
	var m map[string]gojson.RawMessage
	if err := gojson.Unmarshal(raw, &m); err != nil {
		var v any
		if jerr := gojson.Unmarshal(raw, &v); jerr != nil {
			return rawRecord{}, &LineError{Err: ErrCorruptLine}
		}
		return rawRecord{nonObject: true}, nil
	}
	return rawRecord{m: m}, nil
}

func (r rawRecord) getString(field string) string {
	if r.strict != nil {
		s := r.strict
		switch field {
		case "type":
			return s.Type
		case "subtype":
			return s.Subtype
		case "sessionId":
			return s.SessionID
		case "session_id":
			return s.SessionSnake
		case "uuid":
			return s.UUID
		case "parentUuid":
			return s.ParentUUID
		case "agentId":
			return s.AgentID
		case "agent_id":
			return s.AgentIDSnake
		case "timestamp":
			return s.Timestamp
		case "cwd":
			return s.CWD
		case "version":
			return s.Version
		case "operation":
			return s.Operation
		case "tool_name":
			return s.ToolName
		case "tool_use_id":
			return s.ToolUseID
		case "hook_event_name":
			return s.HookEventName
		case "transcript_path":
			return s.TranscriptPath
		case "agent_transcript_path":
			return s.AgentTranscriptPath
		case "mode":
			return s.Mode
		case "permissionMode":
			return s.PermissionMode
		case "customTitle":
			return s.CustomTitle
		case "agentName":
			return s.AgentName
		case "aiTitle":
			return s.AITitle
		case "summary":
			return s.Summary
		case "lastPrompt":
			return s.LastPrompt
		case "atis":
			return s.ATIS
		case "prUrl":
			return s.PrURL
		case "terminal_reason":
			return s.TerminalReason
		case "content":
			return rawStr(s.Content)
		case "prompt":
			return rawStr(s.Prompt)
		case "question":
			return rawStr(s.Question)
		case "result":
			return rawStr(s.Result)
		case "last_assistant_message":
			return rawStr(s.LastAssistant)
		case "origin":
			return rawStr(s.Origin)
		case "attachment":
			return rawStr(s.Attachment)
		case "usage":
			return rawStr(s.Usage)
		case "tool_input":
			return rawStr(s.ToolInput)
		case "tool_response":
			return rawStr(s.ToolResponse)
		case "tool_use_result":
			return rawStr(s.ToolUseResult)
		case "message":
			// On strict records `message` is always an object; the string-
			// shaped `message` (permission_denied-style) fails strict decode
			// and is served by the map path.
			return ""
		}
		return ""
	}
	return rawString(r.m[field])
}

// rawStr decodes a keep-with-TolerantMessage into the string it holds ("" when
// absent or not a string).
func rawStr(raw gojson.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := gojson.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func (r rawRecord) getBool(field string) bool {
	if r.strict != nil {
		switch field {
		case "isSidechain":
			return r.strict.IsSidechain
		case "isMeta":
			return r.strict.IsMeta
		case "is_error":
			return r.strict.IsError
		}
		return false
	}
	var b bool
	_ = gojson.Unmarshal(r.m[field], &b)
	return b
}

func (r rawRecord) getFloat(field string) float64 {
	if r.strict != nil {
		if field == "total_cost_usd" {
			return r.strict.CostUSD
		}
		return 0
	}
	var f float64
	_ = gojson.Unmarshal(r.m[field], &f)
	return f
}

func (r rawRecord) getStringSlice(field string) []string {
	if r.strict != nil {
		switch field {
		case "retractedMessageUuids":
			return r.strict.Retracted
		case "supersedesUuids":
			return r.strict.Supersedes
		}
		return nil
	}
	var out []string
	_ = gojson.Unmarshal(r.m[field], &out)
	return out
}

func (r rawRecord) getMap(field string) map[string]any {
	raw := r.getRaw(field)
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := gojson.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// getRaw returns the raw JSON of a member. json.RawMessage (stdlib type) is
// used at the API boundary; gojson.RawMessage fields convert cheaply.
func (r rawRecord) getRaw(field string) json.RawMessage {
	if r.strict != nil {
		s := r.strict
		switch field {
		case "origin":
			return s.Origin
		case "content":
			return s.Content
		case "tool_use_result":
			return s.ToolUseResult
		case "attachment":
			return s.Attachment
		case "usage":
			return s.Usage
		case "tool_input":
			return s.ToolInput
		case "tool_response":
			return s.ToolResponse
		case "prompt":
			return s.Prompt
		case "question":
			return s.Question
		case "last_assistant_message":
			return s.LastAssistant
		case "result":
			return s.Result
		case "ttft_ms":
			return s.TTFTMs
		case "duration_ms":
			return s.DurationMs
		case "permission_denials":
			return s.PermDenials
		}
		return nil
	}
	return json.RawMessage(r.m[field])
}

// messageContent returns the raw `message.content` member (nil if absent) and
// whether the record carries a `message` member at all.
func (r rawRecord) messageContent() (json.RawMessage, bool) {
	if r.strict != nil {
		c := r.strict.Message.Content
		return c, c != nil
	}
	raw := r.m["message"]
	if len(raw) == 0 {
		return nil, false
	}
	var v messageV1
	if err := gojson.Unmarshal(raw, &v); err != nil {
		return nil, true
	}
	return v.Content, true
}

// messageModel extracts the model off the message object ("" if absent).
func (r rawRecord) messageModel() string {
	if r.strict != nil {
		return r.strict.Message.Model
	}
	var v messageV1
	_ = gojson.Unmarshal(r.m["message"], &v)
	return v.Model
}

// messageUsage extracts the usage object off the message object.
func (r rawRecord) messageUsage() *usageTokenValues {
	if r.strict != nil {
		if u := r.strict.Message.Usage; len(u) > 0 {
			return usageFromRaw(u)
		}
		return nil
	}
	var v messageV1
	_ = gojson.Unmarshal(r.m["message"], &v)
	if len(v.Usage) == 0 {
		return nil
	}
	return usageFromRaw(v.Usage)
}

// getIntCoerce accepts integer or numeric-string values.
func (r rawRecord) getIntCoerce(field string) int64 {
	raw := r.getRaw(field)
	if len(raw) == 0 {
		return 0
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := gojson.Unmarshal(trimmed, &s); err != nil {
			return 0
		}
		if i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return int64(math.Round(f))
		}
		return 0
	}
	var f float64
	if err := gojson.Unmarshal(trimmed, &f); err != nil {
		return 0
	}
	return int64(math.Round(f))
}

// --- shared fragment helpers ------------------------------------------------

// rawString decodes a RawMessage into a string tolerantly ("" on failure).
func rawString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// usage is the raw usage sub-object shared by assistant messages and result
// events. Keep ONLY integer leaf fields: a mismatched type inside this object
// fails the strict decode and pushes the line to the tolerant fallback (never
// a crash).
type usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

// usageTokenValues is the normalized token tuple extracted from a raw usage
// object (filled into eventmodel.TokenUsage by the caller).
type usageTokenValues struct {
	In         int64
	Out        int64
	CacheRead  int64
	CacheWrite int64
}

// usageFromRaw parses a usage object tolerantly (all values optional).
func usageFromRaw(raw json.RawMessage) *usageTokenValues {
	var u usage
	_ = gojson.Unmarshal(raw, &u)
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 {
		return nil
	}
	return &usageTokenValues{
		In:         u.Input,
		Out:        u.Output,
		CacheRead:  u.CacheRead,
		CacheWrite: u.CacheWrite,
	}
}
