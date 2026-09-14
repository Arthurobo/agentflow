package parse

import (
	"bytes"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
)

// realisticAssistant is a corpus-shaped assistant record: thinking block + text
// + tool_use block + usage telemetry (the hottest normalization path).
const realisticAssistant = `{"type":"assistant","uuid":"a-1","parentUuid":"u-1","timestamp":"2026-08-01T10:00:02.000Z","sessionId":"s-1","version":"2.1.235","cwd":"/home/user/code/proj","message":{"model":"claude-fable-5","content":[{"type":"thinking","thinking":"","signature":"SIG0001"},{"type":"text","text":"Implement the scheduler loop with backpressure, then wire it to the event bus."},{"type":"tool_use","id":"toolu_01ABC","name":"Bash","input":{"command":"go test ./..."}}],"usage":{"input_tokens":2438,"output_tokens":412,"cache_read_input_tokens":19102,"cache_creation_input_tokens":0}}}`

// corpusLike builds n corpus-shaped lines cycling the real record shapes.
func corpusLike(n int) []byte {
	var sb bytes.Buffer
	line := corpusLine(0)
	for i := 0; i < n; i++ {
		if i%3 == 0 { // vary shape a bit through the realistic templates
			line = corpusLine(i)
		}
		sb.Write(line)
		sb.WriteByte('\n')
	}
	return sb.Bytes()
}

// corpusLine emits one of the representative record shapes.
func corpusLine(seed int) []byte {
	templates := []string{
		`{"type":"user","message":{"role":"user","content":"fix the build please"}}`,
		realisticAssistant,
		`{"type":"system","subtype":"turn_duration","content":"2500"}`,
		`{"type":"queue-operation","operation":"enqueue","content":"<task-notification><summary>bg</summary></task-notification>"}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01ABC","type":"tool_result","content":"ok"}]}}`,
		`{"type":"mode","mode":"normal","sessionId":"s1"}`,
	}
	return []byte(templates[seed%len(templates)])
}

// BenchmarkNormalizeJSONLLine: single-line normalization (the per-record cost
// the corpus sweep pays 100k times).
func BenchmarkNormalizeJSONLLine(b *testing.B) {
	raw := []byte(realisticAssistant)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Normalize(raw, eventmodel.SourceBackfill); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONLLineRead measures reader + normalize throughput over a batch;
// ns/op is per-loop-of-2k-lines (report lines/sec in the phase report).
func BenchmarkJSONLLineRead(b *testing.B) {
	data := corpusLike(2000)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		jp := NewJSONLParser(bytes.NewReader(data), Options{})
		for {
			if _, err := jp.Next(); err != nil {
				break
			}
		}
	}
}

// BenchmarkNormalizeUserToolResult covers the block extraction + split path on
// user records with tool_result content.
func BenchmarkNormalizeUserToolResult(b *testing.B) {
	raw := []byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01ABC","type":"tool_result","content":"1\tok\n2\tdone\n"}]},"sessionId":"s1"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Normalize(raw, eventmodel.SourceBackfill); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNormalizeOversizedLine covers the 64KiB+ rule-10 path (with sha256).
func BenchmarkNormalizeOversizedLine(b *testing.B) {
	content := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	raw := append(append([]byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_9","type":"tool_result","content":"`), content...), []byte(`"}]},"sessionId":"s1"}`)...)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Normalize(raw, eventmodel.SourceBackfill); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStreamParse: incremental stream-json parsing including init-state
// propagation (stream events per ns).
func BenchmarkStreamParse(b *testing.B) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","cwd":"/home/user/code/proj","session_id":"sess-1","model":"claude-fable-5"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning"},"uuid":"r1","session_id":"sess-1"}`,
		realisticAssistant,
		`{"type":"result","subtype":"success","total_cost_usd":0.05,"result":"done","usage":{"input_tokens":2,"output_tokens":9,"cache_read_input_tokens":1,"cache_creation_input_tokens":0}}`,
	}, "\n") + "\n"
	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sp := NewStreamParser(strings.NewReader(stream), Options{})
		for {
			if _, err := sp.Next(); err != nil {
				break
			}
		}
	}
}
