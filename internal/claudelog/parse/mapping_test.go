package parse

import (
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
)

// typeCase is one entry of the per-type mapping table (every
// type has an assertion here; stream + hook shapes too).
type typeCase struct {
	name     string
	raw      string
	wantType eventmodel.EventType
	wantSub  string
	check    func(t *testing.T, ev *eventmodel.Event)
}

func runTable(t *testing.T, cases []typeCase) {
	t.Helper()
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			p, err := Normalize([]byte(c.raw), eventmodel.SourceTailer)
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if len(p.Events) == 0 {
				t.Fatal("no events produced for a parseable line")
			}
			// Records can expand into several events (tool_use/tool_result
			// blocks); find the requested event type.
			var ev *eventmodel.Event
			for _, e := range p.Events {
				if e.Type == c.wantType {
					ev = e
					break
				}
			}
			if ev == nil {
				ev = p.Events[0]
			}
			if ev.Type != c.wantType || ev.Subtype != c.wantSub {
				t.Fatalf("event = %s/%s, want %s/%s", ev.Type, ev.Subtype, c.wantType, c.wantSub)
			}
			if c.check != nil {
				c.check(t, ev)
			}
		})
	}
}

func TestMappingAllPhase0Types(t *testing.T) {
	session := `"sessionId":"s-1","timestamp":"2026-08-01T10:00:00.000Z","version":"2.1.220","cwd":"/home/user/code/proj"`
	user := `{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"},"isSidechain":false,` + `"origin":{"kind":"human"}` + `,"parentUuid":null,` + session + `}`
	_ = user
	cases := []typeCase{
		{
			name: "user-plain", raw: `{"type":"user","sessionId":"s1","message":{"role":"user","content":"hello there"}}`,
			wantType: eventmodel.EventUserMessage, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if ev.Actor != "user" || ev.Content != "hello there" {
					t.Fatalf("actor=%q content=%q", ev.Actor, ev.Content)
				}
				if ev.ContentHash == "" {
					t.Fatal("contentHash must be computed")
				}
			},
		},
		{
			name:     "user-tool-result-blocks",
			raw:      `{"type":"user","sessionId":"s","message":{"role":"user","content":[{"tool_use_id":"toolu_01AB","type":"tool_result","content":"file contents\nhere"}]}}`,
			wantType: eventmodel.EventToolResult, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if ev.ToolUseID != "toolu_01AB" || ev.Content != "file contents\nhere" {
					t.Fatalf("toolUseId=%q content=%q", ev.ToolUseID, ev.Content)
				}
			},
		},
		{
			name:     "user-peer-origin",
			raw:      `{"type":"user","sessionId":"s9","origin":{"kind":"peer","name":"captures-49"},"message":{"role":"user","content":"peer text"}}`,
			wantType: eventmodel.EventUserMessage, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if ev.Actor != "peer:captures-49" {
					t.Fatalf("actor=%q, want peer attribution", ev.Actor)
				}
				if ev.Origin == nil || ev.Origin["kind"] != "peer" {
					t.Fatal("origin must be retained")
				}
			},
		},
		{
			name:     "assistant-thinking",
			raw:      `{"type":"assistant","sessionId":"s2","version":"2.1.220","message":{"model":"claude-x-1","content":[{"type":"thinking","thinking":"","signature":"SIG="}],"usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":10,"cache_creation_input_tokens":12}}}`,
			wantType: eventmodel.EventAssistantMessage, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if !ev.HasThinking || ev.ThinkingSignature != "SIG=" {
					t.Fatalf("thinking flags wrong: %+v", ev)
				}
				if ev.Tokens == nil || ev.Tokens.In != 5 || ev.Tokens.Out != 7 ||
					ev.Tokens.CacheRead != 10 || ev.Tokens.CacheWrite != 12 {
					t.Fatalf("tokens = %+v", ev.Tokens)
				}
				if ev.Model != "claude-x-1" {
					t.Fatalf("model = %q", ev.Model)
				}
			},
		},
		{
			name:     "assistant-thinking-text-separated-from-answer",
			raw:      `{"type":"assistant","sessionId":"s9","message":{"content":[{"type":"thinking","thinking":"I should check the failing test first.","signature":"S1="},{"type":"text","text":"The fix is in insert.go."}]}}`,
			wantType: eventmodel.EventAssistantMessage, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if !ev.HasThinking {
					t.Fatal("hasThinking must be set")
				}
				if ev.ThinkingContent != "I should check the failing test first." {
					t.Fatalf("thinking content wrong: %q", ev.ThinkingContent)
				}
				if ev.Content != "The fix is in insert.go." {
					t.Fatalf("answer must not contain thinking text, got %q", ev.Content)
				}
			},
		},
		{
			name:     "assistant-tool-use-block",
			raw:      `{"type":"assistant","uuid":"au1","sessionId":"s1","message":{"content":[{"type":"tool_use","id":"toolu_9","name":"Bash","input":{"command":"ls -la"}}]}}`,
			wantType: eventmodel.EventToolUse, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if ev.ToolName != "Bash" || ev.ToolUseID != "toolu_9" || ev.ParentUUID != "au1" {
					t.Fatalf("tool_use mapping wrong: %+v", ev)
				}
				if ev.ToolInput["command"] != "ls -la" {
					t.Fatalf("toolInput = %v", ev.ToolInput)
				}
			},
		},
		{
			name:     "assistant-text-and-tool",
			raw:      `{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"thinking aloud"},{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/tmp/f"}}]}}`,
			wantType: eventmodel.EventAssistantMessage, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if ev.Content != "thinking aloud" {
					t.Fatalf("assistant_message must carry the text block, got %q", ev.Content)
				}
			},
		},

		// --- snapshot / session_state types ---
		{name: "mode", raw: `{"type":"mode","mode":"plan","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "mode", check: chkContent("plan")},
		{name: "permission-mode", raw: `{"type":"permission-mode","permissionMode":"acceptEdits","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "permission-mode", check: chkContent("acceptEdits")},
		{name: "custom-title", raw: `{"type":"custom-title","customTitle":"SUP-1 -> PLANNING","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "custom-title", check: chkContent("SUP-1 -> PLANNING")},
		{name: "agent-name", raw: `{"type":"agent-name","agentName":"worker-7","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "agent-name", check: chkContent("worker-7")},
		{name: "ai-title", raw: `{"type":"ai-title","aiTitle":"Review the dashboard","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "ai-title", check: chkContent("Review the dashboard")},
		{name: "last-prompt", raw: `{"type":"last-prompt","lastPrompt":"remember to commit","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "last-prompt", check: chkContent("remember to commit")},
		{name: "atis-latch", raw: `{"type":"atis-latch","atis":"","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "atis-latch"},
		{name: "pr-link", raw: `{"type":"pr-link","prUrl":"https://example.com/repo/pull/1","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "pr-link", check: chkContent("https://example.com/repo/pull/1")},
		{name: "file-history-snapshot", raw: `{"type":"file-history-snapshot","messageId":"m1","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "file-history-snapshot"},
		{name: "file-history-delta", raw: `{"type":"file-history-delta","trackingPath":"a/b/c.tf","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "file-history-delta"},
		{name: "bridge-session", raw: `{"type":"bridge-session","bridgeSessionId":"cse_x","sessionId":"s"}`, wantType: eventmodel.EventSessionState, wantSub: "bridge-session"},
		{
			name: "attachment", raw: `{"type":"attachment","attachment":{"type":"deferred_tools_delta"},"sessionId":"s"}`,
			wantType: eventmodel.EventMeta, wantSub: "attachment:deferred_tools_delta",
		},

		// --- queue-operation (all operations + content shapes) ---
		{name: "qop-enqueue-plaintext", raw: `{"type":"queue-operation","operation":"enqueue","content":"do the thing","sessionId":"s"}`, wantType: eventmodel.EventQueueOp, wantSub: "enqueue", check: chkContent("do the thing")},
		{name: "qop-enqueue-task-wrapper", raw: `{"type":"queue-operation","operation":"enqueue","content":"<task-notification>\n<task-id>t1</task-id>\n</task-notification>","sessionId":"s"}`, wantType: eventmodel.EventQueueOp, wantSub: "enqueue", check: func(t *testing.T, ev *eventmodel.Event) {
			if !strings.HasPrefix(ev.Content, "<task-notification>") {
				t.Fatalf("must retain the <task-...> wrapper verbatim, got %q", ev.Content)
			}
		}},
		{name: "qop-enqueue-cross-session", raw: `{"type":"queue-operation","operation":"enqueue","content":"<cross-session-message from=\"x\">hi</cross-session-message>","sessionId":"s"}`, wantType: eventmodel.EventQueueOp, wantSub: "enqueue"},
		{name: "qop-dequeue", raw: `{"type":"queue-operation","operation":"dequeue","sessionId":"s"}`, wantType: eventmodel.EventQueueOp, wantSub: "dequeue"},
		{name: "qop-remove", raw: `{"type":"queue-operation","operation":"remove","content":"<task-notification><status>failed</status></task-notification>","sessionId":"s"}`, wantType: eventmodel.EventQueueOp, wantSub: "remove"},

		// --- system records ---
		{name: "system-turn-duration", raw: `{"type":"system","subtype":"turn_duration","content":"2500","sessionId":"s"}`, wantType: eventmodel.EventSystemMeta, wantSub: "turn_duration", check: chkContent("2500")},
		{name: "system-away-summary", raw: `{"type":"system","subtype":"away_summary","content":"you were away","sessionId":"s"}`, wantType: eventmodel.EventSystemMeta, wantSub: "away_summary"},
		{name: "system-local-command", raw: `{"type":"system","subtype":"local_command","sessionId":"s"}`, wantType: eventmodel.EventSystemMeta, wantSub: "local_command"},
		{name: "system-stop-hook-summary", raw: `{"type":"system","subtype":"stop_hook_summary","sessionId":"s"}`, wantType: eventmodel.EventSystemMeta, wantSub: "stop_hook_summary"},
		{name: "system-scheduled-task", raw: `{"type":"system","subtype":"scheduled_task_fire","sessionId":"s"}`, wantType: eventmodel.EventSystemMeta, wantSub: "scheduled_task_fire"},
		{name: "system-bridge-status", raw: `{"type":"system","subtype":"bridge_status","sessionId":"s"}`, wantType: eventmodel.EventSystemMeta, wantSub: "bridge_status"},
		{name: "system-compact-boundary", raw: `{"type":"system","subtype":"compact_boundary","sessionId":"s"}`, wantType: eventmodel.EventSystemMeta, wantSub: "compact_boundary"},
		{name: "system-informational", raw: `{"type":"system","subtype":"informational","content":"note","sessionId":"s"}`, wantType: eventmodel.EventSystemMeta, wantSub: "informational"},
		{name: "system-agents-killed", raw: `{"type":"system","subtype":"agents_killed","sessionId":"s"}`, wantType: eventmodel.EventSystemMeta, wantSub: "agents_killed"},
		{name: "system-model-consent-fallback", raw: `{"type":"system","subtype":"model_consent_fallback","sessionId":"s"}`, wantType: eventmodel.EventSystemMeta, wantSub: "model_consent_fallback"},
		{
			name:     "system-model-refusal-fallback-retracted",
			raw:      `{"type":"system","subtype":"model_refusal_fallback","retractedMessageUuids":["a1","b2"],"content":"switched model","sessionId":"s","uuid":"uu1"}`,
			wantType: eventmodel.EventSystemMeta, wantSub: "model_refusal_fallback",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if len(ev.RetractedUuids) != 2 || ev.RetractedUuids[0] != "a1" {
					t.Fatalf("retractedUuids = %v", ev.RetractedUuids)
				}
			},
		},

		// --- sessionId/session_id dual spelling (rule 1) ---
		{
			name:     "dual-spelling-diff-forwarded",
			raw:      `{"type":"assistant","sessionId":"owner-1","session_id":"sub-2","uuid":"x1","message":{"content":[{"type":"text","text":"hi"}]}}`,
			wantType: eventmodel.EventAssistantMessage, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if ev.SessionID != "owner-1" || ev.OriginSessionID != "origin-2" && ev.OriginSessionID != "sub-2" {
					t.Fatalf("sessionId=%q originSessionId=%q", ev.SessionID, ev.OriginSessionID)
				}
			},
		},
		{
			name:     "dual-spelling-stream-only",
			raw:      `{"type":"assistant","session_id":"s-1","message":{"content":[{"type":"text","text":"hi"}]}}`,
			wantType: eventmodel.EventAssistantMessage, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if ev.SessionID != "s-1" || ev.OriginSessionID != "" {
					t.Fatalf("sessionId=%q originSessionId=%q", ev.SessionID, ev.OriginSessionID)
				}
			},
		},

		// --- isSidechain + agentId attribution ---
		{
			name:     "subagent-sidechain",
			raw:      `{"type":"assistant","isSidechain":true,"agentId":"hex123","sessionId":"sub-1","message":{"content":[{"type":"text","text":"sc"}]}}`,
			wantType: eventmodel.EventAssistantMessage, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if !ev.IsSidechain || ev.AgentID != "hex123" {
					t.Fatalf("sidechain fields wrong: %+v", ev)
				}
				if ev.Actor != "subagent:hex123" {
					t.Fatalf("actor = %q", ev.Actor)
				}
			},
		},

		// --- raw schema version ---
		{
			name:     "raw-version-parse",
			raw:      `{"type":"assistant","version":"2.1.235","sessionId":"s","message":{"content":[{"type":"text","text":"hi"}]}}`,
			wantType: eventmodel.EventAssistantMessage, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if ev.RawVersion != "2.1.235" || ev.RawSchemaVersion != 2 {
					t.Fatalf("rawVersion=%q rawSchemaVersion=%d", ev.RawVersion, ev.RawSchemaVersion)
				}
			},
		},
		{
			name:     "raw-version-absent",
			raw:      `{"type":"mode","mode":"normal","sessionId":"s"}`,
			wantType: eventmodel.EventSessionState, wantSub: "mode",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if ev.RawVersion != "" || ev.RawSchemaVersion != 0 {
					t.Fatalf("absent version must yield 0/\"\": %q/%d", ev.RawVersion, ev.RawSchemaVersion)
				}
			},
		},

		// --- tolerant parsing (rule 11) ---
		{
			name:     "unknown-type",
			raw:      `{"type":"mystery-type","anything":{"nested":true},"sessionId":"s"}`,
			wantType: eventmodel.EventMeta, wantSub: "raw:mystery-type",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if len(ev.Raw) == 0 {
					t.Fatal("Raw must be retained for unknown types")
				}
			},
		},
		{
			name:     "no-type-field",
			raw:      `{"hello":1,"sessionId":"s"}`,
			wantType: eventmodel.EventMeta, wantSub: "raw:",
		},
		{
			name:     "non-object-json",
			raw:      `["just","an","array"]`,
			wantType: eventmodel.EventMeta, wantSub: "raw:non-object",
		},
		{
			name:     "non-object-string",
			raw:      `"a plain string"`,
			wantType: eventmodel.EventMeta, wantSub: "raw:non-object",
		},
		{
			name:     "weird-typed-fields-dont-crash",
			raw:      `{"type":"user","isSidechain":"yes","uuid":42,"sessionId":["array"],"message":{"role":"user","content":"still works"}}`,
			wantType: eventmodel.EventUserMessage, wantSub: "",
			check: func(t *testing.T, ev *eventmodel.Event) {
				if ev.Content != "still works" {
					t.Fatalf("content=%q", ev.Content)
				}
			},
		},
		{
			name:     "garbage-block-types-dont-crash",
			raw:      `{"type":"assistant","message":{"content":[{"type":"mystery","x":1},{"type":"tool_use","id":5,"input":"not-a-map"}]}}`,
			wantType: eventmodel.EventAssistantMessage, wantSub: "",
		},
	}

	runTable(t, cases)
}

// weird-typed-fields case: type=user with garbage fields should still map to
// user_message — fix expectation.
func TestWeirdTypedFieldsMapToUser(t *testing.T) {
	p, err := Normalize([]byte(`{"type":"user","sessionId":"s","message":{"role":"user","content":"still works"}}`), eventmodel.SourceTailer)
	if err != nil {
		t.Fatal(err)
	}
	if p.Events[0].Type != eventmodel.EventUserMessage {
		t.Fatalf("want user_message, got %s", p.Events[0].Type)
	}
}

func chkContent(want string) func(*testing.T, *eventmodel.Event) {
	return func(t *testing.T, ev *eventmodel.Event) {
		if ev.Content != want {
			t.Fatalf("content = %q, want %q", ev.Content, want)
		}
	}
}

// TestSummaryLineIsSessionTitle pins the compact-list name: claude writes
// {"type":"summary","summary":"..."} records (what `claude -r` shows) and the
// sessions list surfaces them as the session's name.
func TestSummaryLineIsSessionTitle(t *testing.T) {
	p, err := Normalize([]byte(`{"type":"summary","summary":"Fix the failing store test","leafUuid":"u-1"}`), "corpus")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(p.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(p.Events))
	}
	ev := p.Events[0]
	if ev.Type != eventmodel.EventSessionState || ev.Subtype != "summary" {
		t.Fatalf("want session_state/summary, got %s/%s", ev.Type, ev.Subtype)
	}
	if ev.Content != "Fix the failing store test" {
		t.Fatalf("summary text lost: %q", ev.Content)
	}
}
