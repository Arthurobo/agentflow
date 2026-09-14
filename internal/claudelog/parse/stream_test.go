package parse

import (
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
)

// TestStreamParserSequence models a two-turn persistent session exactly as
// observed in (init is repeatable; re-init between turns).
func TestStreamParserSequence(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","cwd":"/home/user/code/proj","session_id":"sess-1","model":"claude-fable-5"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning"},"uuid":"r1","session_id":"sess-1"}`,
		`{"type":"assistant","message":{"model":"claude-fable-5","content":[{"type":"text","text":"alpha"}]},"uuid":"a1","session_id":"sess-1"}`,
		`{"is_error":false,"num_turns":1,"total_cost_usd":0.05,"subtype":"success","result":"alpha","type":"result","uuid":"res1","session_id":"sess-1"}`,
		`{"type":"system","subtype":"init","cwd":"/home/user/code/proj","session_id":"sess-1","model":"claude-fable-5"}`, // re-init
		`{"type":"assistant","message":{"model":"claude-fable-5","content":[{"type":"text","text":"beta"}]},"uuid":"a2","session_id":"sess-1"}`,
		`{"type":"result","subtype":"success","result":"beta","uuid":"res2","session_id":"sess-1"}`,
	}, "\n") + "\n"

	sp := NewStreamParser(strings.NewReader(stream), Options{})
	var got []*eventmodel.Event
	for {
		p, err := sp.Next()
		if err != nil {
			break // io.EOF
		}
		got = append(got, p.Events...)
	}

	wantTypes := []eventmodel.EventType{
		eventmodel.EventSystemMeta, // init
		eventmodel.EventRateLimit,
		eventmodel.EventAssistantMessage,
		eventmodel.EventResult,
		eventmodel.EventSystemMeta, // re-init
		eventmodel.EventAssistantMessage,
		eventmodel.EventResult,
	}
	if len(got) != len(wantTypes) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(wantTypes), got)
	}
	for i, w := range wantTypes {
		if got[i].Type != w {
			t.Fatalf("event %d = %s, want %s", i, got[i].Type, w)
		}
	}
	if got[0].Subtype != "init" {
		t.Fatalf("init subtype = %q", got[0].Subtype)
	}
	if got[3].CostUSD != 0.05 || got[3].IsError {
		t.Fatalf("result ledger wrong: %+v", got[3])
	}
	if got[6].Type != eventmodel.EventResult {
		t.Fatalf("second result missing")
	}
	if got[5].CWD == "" {
		t.Fatal("stream parser must stamp cwd from init state")
	}
	if got[5].Project != "proj" {
		t.Fatalf("project = %q", got[5].Project)
	}
	if got[1].SessionID != "sess-1" {
		t.Fatalf("rate_limit session = %q", got[1].SessionID)
	}
	if got[0].Seq != 1 || got[2].Seq != 3 || got[6].Seq != 7 {
		t.Fatalf("sequence numbers wrong: got[0]=%d got[2]=%d got[6]=%d", got[0].Seq, got[2].Seq, got[6].Seq)
	}
}

// TestStreamParserToolRoundTrip models an assistant tool_use -> user
// tool_result round trip.
func TestStreamParserToolRoundTrip(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_01A","name":"Read","input":{"file_path":"/tmp/f.txt"}}]},"uuid":"au1","session_id":"s1","parent_tool_use_id":null}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01A","type":"tool_result","content":"hello slow test\n"}]},"uuid":"uu1","session_id":"s1","tool_use_result":{"type":"text","file":{"filePath":"/tmp/f.txt","content":"hello slow test\n"}}}`,
	}, "\n") + "\n"

	sp := NewStreamParser(strings.NewReader(stream), Options{})
	events := []*eventmodel.Event{}
	for {
		p, err := sp.Next()
		if err != nil {
			break
		}
		events = append(events, p.Events...)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (assistant_message + tool_use + tool_result)", len(events))
	}
	if events[1].Type != eventmodel.EventToolUse || events[1].ToolUseID != "toolu_01A" || events[1].ParentUUID != "au1" {
		t.Fatalf("tool_use event wrong: %+v", events[1])
	}
	if events[2].Type != eventmodel.EventToolResult || events[2].ToolUseID != "toolu_01A" || events[2].Content != "hello slow test\n" {
		t.Fatalf("tool_result event wrong: %+v", events[2])
	}
}

// TestStreamParserThinkingTokensAndTasks covers the system sub-events that
// only appear on the stream.
func TestStreamParserThinkingTokensAndTasks(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","cwd":"/p","session_id":"s1"}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":50,"estimated_tokens_delta":50,"uuid":"t1","session_id":"s1"}`,
		`{"type":"system","subtype":"task_started","task_id":"ab12","tool_use_id":"toolu_9","subagent_type":"Explore","uuid":"tk1","session_id":"s1"}`,
		`{"type":"system","subtype":"task_progress","task_id":"ab1","usage":{"total_tokens":9730,"tool_uses":1,"duration_ms":3880},"uuid":"tk2","session_id":"s1"}`,
		`{"type":"system","subtype":"task_updated","task_id":"ab1","uuid":"tk3","session_id":"s1"}`,
		`{"type":"system","subtype":"task_notification","task_id":"ab1","uuid":"tk4","session_id":"s1"}`,
	}, "\n") + "\n"

	sp := NewStreamParser(strings.NewReader(stream), Options{})
	want := []struct {
		typ eventmodel.EventType
		sub string
	}{
		{eventmodel.EventSystemMeta, "init"},
		{eventmodel.EventSystemMeta, "thinking_tokens"},
		{eventmodel.EventTask, "task_started"},
		{eventmodel.EventTask, "task_progress"},
		{eventmodel.EventTask, "task_updated"},
		{eventmodel.EventTask, "task_notification"},
	}
	i := 0
	for {
		p, err := sp.Next()
		if err != nil {
			break
		}
		for _, ev := range p.Events {
			if i >= len(want) {
				t.Fatalf("unexpected extra event: %+v", ev)
			}
			if ev.Type != want[i].typ || ev.Subtype != want[i].sub {
				t.Fatalf("event %d = %s/%s, want %s/%s", i, ev.Type, ev.Subtype, want[i].typ, want[i].sub)
			}
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("got %d events, want %d", i, len(want))
	}
}

// TestStreamParserPermissionDenied covers system/permission_denied mapping.
func TestStreamParserPermissionDenied(t *testing.T) {
	stream := `{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"toolu_01B","decision_reason_type":"rule","message":"Claude requested permissions to use Bash, but you haven't granted it yet.","uuid":"p1","session_id":"s2"}` + "\n"
	sp := NewStreamParser(strings.NewReader(stream), Options{})
	p, err := sp.Next()
	if err != nil {
		t.Fatal(err)
	}
	ev := p.Events[0]
	if ev.Type != eventmodel.EventPermission || ev.Subtype != "permission_denied" {
		t.Fatalf("event = %s/%s", ev.Type, ev.Subtype)
	}
	if ev.ToolName != "Bash" || ev.ToolUseID != "toolu_01B" {
		t.Fatalf("tool fields wrong: %+v", ev)
	}
}

// TestStreamCommandLifecycle: opaque until corpus presence.
func TestStreamCommandLifecycle(t *testing.T) {
	stream := `{"type":"command_lifecycle","message":"launching","session_id":"s3"}` + "\n"
	sp := NewStreamParser(strings.NewReader(stream), Options{})
	p, err := sp.Next()
	if err != nil {
		t.Fatal(err)
	}
	ev := p.Events[0]
	if ev.Type != eventmodel.EventMeta || ev.Subtype != "command_lifecycle" {
		t.Fatalf("event = %s/%s", ev.Type, ev.Subtype)
	}
}
