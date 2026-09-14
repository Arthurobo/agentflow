package parse

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
)

// TestFuzzLiteNeverPanics drives the normalizer with truncated, corrupt, and
// byte-random inputs (an offline mini-fuzzer; keeps CI deterministic).
func TestFuzzLiteNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // deterministic test seed, not security-sensitive
	crops := []string{
		`{"type":"user","message":{"role":"user","content":`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123",`,
		`{"type":"queue-operation","operation":"enqueue","content":"<task-`,
		`{"origin":{"kind":"peer",`,
		`{"retractedMessageUuids":[`,
	}
	for i := 0; i < 5000; i++ {
		r := rng.Intn(4)
		var raw string
		switch r {
		case 0:
			raw = strings.Repeat("\x00\xff", rng.Intn(30))
		case 1:
			// random bytes, valid utf-8 sometimes
			var sb strings.Builder
			for j := 0; j < rng.Intn(120); j++ {
				if rng.Intn(2) == 0 {
					sb.WriteRune(rune(rng.Intn(utf8.MaxRune)))
				} else {
					sb.WriteByte(byte(rng.Intn(256)))
				}
			}
			raw = sb.String()
		case 2:
			// a crop of a real shape (truncated at a random point)
			base := crops[rng.Intn(len(crops))]
			raw = base[:rng.Intn(len(base)+1)] + strings.Repeat("}", rng.Intn(5))
		default:
			// valid JSON with hostile values
			raw = `{"type":"` + strings.Repeat("x", rng.Intn(40)) + `","sessionId":` +
				strings.Repeat(`"`, rng.Intn(3)) + `,"content":{"a":[1,{"b":null}],"c":"` +
				strings.Repeat("y", rng.Intn(60)) + `"},"uuid":` + strings.Repeat("z", rng.Intn(20)) + `}`
		}
		p, err := Normalize([]byte(raw), eventmodel.SourceBackfill) //nolint:staticcheck // panic is the failure mode
		if err == nil && (len(p.Events) == 0 || p.Events[0].Type == "") {
			t.Fatalf("parseable input must produce a typed event: %.80q", raw)
		}
	}
}

// FuzzNormalize is the real fuzz entry point (run with go test -fuzz); the
// invariant is simply "never panic, always return either an error or events".
func FuzzNormalize(f *testing.F) {
	f.Add([]byte(`{"type":"user","message":{"content":"hi"}}`))
	f.Add([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}}`))
	f.Add([]byte(`garbage{`))
	f.Add([]byte{0xff, 0xfe, 0x00, 0x01})
	for _, raw := range corpusSeeds() {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if !utf8.Valid(raw) {
			return
		}
		p, err := Normalize(raw, eventmodel.SourceBackfill)
		if err == nil && len(p.Events) == 0 {
			t.Fatalf("no error but no events for %q", raw)
		}
	})
}

// corpusSeeds provides real corpus-shaped records for the fuzz seed pool.
func corpusSeeds() [][]byte {
	return [][]byte{
		[]byte(`{"type":"mode","mode":"normal","sessionId":"s"}`),
		[]byte(`{"type":"queue-operation","operation":"enqueue","content":"<task-a>"}`),
		[]byte(`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"t"}`),
		[]byte(`{"type":"result","total_cost_usd":0.05,"subtype":"success"}`),
	}
}
