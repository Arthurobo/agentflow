package opencode_test

import (
	"path/filepath"
	"testing"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
	opencodeingest "github.com/arthurobo/agentflow/internal/ingest/opencode"
)

// TestNormalizePart_Text pins the user_text → assistant_message mapping.
func TestNormalizePart_Text(t *testing.T) {
	sess := &opencodeingest.SessionDoc{ID: "ses_text", ProjectID: "p", Directory: "/tmp/x"}
	p := &opencodeingest.PartDoc{SessionID: "ses_text", Type: "text", Text: "hello"}
	evs := opencodeingest.NormalizePart(p, sess)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Type != eventmodel.EventAssistantMessage {
		t.Fatalf("type = %s, want assistant_message", ev.Type)
	}
	if ev.Content != "hello" {
		t.Fatalf("content = %q, want hello", ev.Content)
	}
	if ev.Source != eventmodel.SourceOpenCode {
		t.Fatalf("source = %s, want opencode", ev.Source)
	}
}

// TestNormalizePart_Reasoning asserts thinking content flows into
// ThinkingContent, not Content (separate channels per eventmodel).
func TestNormalizePart_Reasoning(t *testing.T) {
	sess := &opencodeingest.SessionDoc{ID: "ses_r", ProjectID: "p"}
	p := &opencodeingest.PartDoc{SessionID: "ses_r", Type: "reasoning", Reasoning: "thinking…"}
	evs := opencodeingest.NormalizePart(p, sess)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if !ev.HasThinking {
		t.Fatalf("HasThinking should be true")
	}
	if ev.ThinkingContent != "thinking…" {
		t.Fatalf("ThinkingContent = %q, want thinking…", ev.ThinkingContent)
	}
}

// TestNormalizePart_ToolRunning asserts a running tool → tool_use.
func TestNormalizePart_ToolRunning(t *testing.T) {
	sess := &opencodeingest.SessionDoc{ID: "ses_t", ProjectID: "p"}
	p := &opencodeingest.PartDoc{
		SessionID: "ses_t", Type: "tool", CallID: "call_1", Tool: "read",
		State: &opencodeingest.ToolState{Status: "running", Input: map[string]any{"path": "x"}},
	}
	evs := opencodeingest.NormalizePart(p, sess)
	if len(evs) != 1 || evs[0].Type != eventmodel.EventToolUse {
		t.Fatalf("want tool_use, got %+v", evs)
	}
	if evs[0].ToolUseID != "call_1" || evs[0].ToolName != "read" {
		t.Fatalf("tool ids: %+v", evs[0])
	}
}

// TestNormalizePart_ToolCompleted asserts a completed tool → tool_result
// carrying both output AND the original input snapshot (so a viewer
// can render "what the tool saw").
func TestNormalizePart_ToolCompleted(t *testing.T) {
	sess := &opencodeingest.SessionDoc{ID: "ses_t2", ProjectID: "p"}
	p := &opencodeingest.PartDoc{
		SessionID: "ses_t2", Type: "tool", CallID: "call_2", Tool: "bash",
		State: &opencodeingest.ToolState{
			Status: "completed",
			Input:  map[string]any{"cmd": "ls"},
			Output: "file.txt",
		},
	}
	evs := opencodeingest.NormalizePart(p, sess)
	if len(evs) != 1 || evs[0].Type != eventmodel.EventToolResult {
		t.Fatalf("want tool_result, got %+v", evs)
	}
	if evs[0].Content != "file.txt" {
		t.Fatalf("output: %q", evs[0].Content)
	}
	if evs[0].ToolInput["cmd"] != "ls" {
		t.Fatalf("input snapshot missing: %+v", evs[0].ToolInput)
	}
	if evs[0].IsError {
		t.Fatalf("completed tool should not be IsError")
	}
}

// TestNormalizePart_StepFinish asserts step-finish carries cost + tokens
// through to eventmodel (the only source of real cost for OpenCode).
func TestNormalizePart_StepFinish(t *testing.T) {
	sess := &opencodeingest.SessionDoc{ID: "ses_sf", ProjectID: "p"}
	p := &opencodeingest.PartDoc{
		SessionID: "ses_sf", Type: "step-finish", Reason: "stop",
		Cost: 0.0125,
		Tokens: &opencodeingest.TokenUsage{
			Total: 100, Input: 40, Output: 50, Reasoning: 10,
			Cache: &opencodeingest.TokenCache{Read: 1000, Write: 0},
		},
	}
	evs := opencodeingest.NormalizePart(p, sess)
	if len(evs) != 1 || evs[0].Type != eventmodel.EventResult {
		t.Fatalf("want result, got %+v", evs)
	}
	if evs[0].CostUSD != 0.0125 {
		t.Fatalf("cost: %v", evs[0].CostUSD)
	}
	if evs[0].Tokens == nil ||
		evs[0].Tokens.In != 40 || evs[0].Tokens.Out != 50 ||
		evs[0].Tokens.CacheRead != 1000 {
		t.Fatalf("tokens: %+v", evs[0].Tokens)
	}
}

// TestNormalizePart_File asserts attachments round-trip into a tool_use with
// a "file" tool name (file parts are attachments).
func TestNormalizePart_File(t *testing.T) {
	sess := &opencodeingest.SessionDoc{ID: "ses_f", ProjectID: "p"}
	p := &opencodeingest.PartDoc{
		SessionID: "ses_f", Type: "file", Mime: "image/png",
		Filename: "shot.png", URL: "file://shot.png",
	}
	evs := opencodeingest.NormalizePart(p, sess)
	if len(evs) != 1 || evs[0].Type != eventmodel.EventToolUse {
		t.Fatalf("want tool_use, got %+v", evs)
	}
	if evs[0].ToolName != "file" || evs[0].Subtype != "attachment" {
		t.Fatalf("file part: %+v", evs[0])
	}
}

// TestNormalizePart_UnknownType asserts unknown types round-trip as meta
// with the raw spelling preserved.
func TestNormalizePart_UnknownType(t *testing.T) {
	sess := &opencodeingest.SessionDoc{ID: "ses_u", ProjectID: "p"}
	p := &opencodeingest.PartDoc{SessionID: "ses_u", Type: "compaction"}
	evs := opencodeingest.NormalizePart(p, sess)
	if len(evs) != 1 || evs[0].Type != eventmodel.EventMeta {
		t.Fatalf("want meta, got %+v", evs)
	}
	if evs[0].Subtype != "raw:compaction" {
		t.Fatalf("subtype: %q", evs[0].Subtype)
	}
}

// TestToIncoming_PreservesCostAndTokens pins the store.Incoming mapping:
// the writer depends on TokensIn/TokensOut/TokCacheRead/TokCacheWr/CostUSD
// all being set when present on the event.
func TestToIncoming_PreservesCostAndTokens(t *testing.T) {
	ev := &eventmodel.Event{
		SessionID: "ses_x", Type: eventmodel.EventResult, Subtype: "step-finish",
		CostUSD: 0.0125,
		Tokens:  &eventmodel.TokenUsage{In: 40, Out: 50, CacheRead: 1000},
		Content: "done",
		Source:  eventmodel.SourceOpenCode,
	}
	in := opencodeingest.ToIncoming(ev)
	if in.CostUSD != 0.0125 {
		t.Fatalf("CostUSD: %v", in.CostUSD)
	}
	if in.TokensIn != 40 || in.TokensOut != 50 || in.TokCacheRead != 1000 {
		t.Fatalf("token fields: %+v", in)
	}
	if in.Event != "result" {
		t.Fatalf("Event: %s", in.Event)
	}
	if in.Source != "opencode" {
		t.Fatalf("Source: %s", in.Source)
	}
}

// TestStorageSessionPath is the canonical session-path shape the writer
// uses for dedupe (it must match across live SSE + backfill walks).
func TestStorageSessionPath(t *testing.T) {
	got := opencodeingest.StorageSessionPathForTest("p1", "ses_abc")
	want := filepath.Join("opencode", "session", "p1", "ses_abc.json")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
