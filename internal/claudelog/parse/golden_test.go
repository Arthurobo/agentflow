package parse

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// TestGoldenNormalize pins the exact normalized output for a small mixed
// transcript: any mapping change that alters emitted JSON fails the diff.
// Refresh with: go test ./internal/claudelog/parse -run Golden -update
func TestGoldenNormalize(t *testing.T) {
	fixture := filepath.Join(testdataDir, "golden", "mixed.jsonl")
	golden := fixture + ".golden"

	jp := NewJSONLParser(mustOpen(t, fixture), Options{})
	var lines []string
	for {
		p, err := jp.Next()
		if err != nil {
			break // io.EOF
		}
		b, merr := json.Marshal(p.Events)
		if merr != nil {
			t.Fatalf("marshal: %v", merr)
		}
		lines = append(lines, string(b))
	}
	got := strings.Join(lines, "\n") + "\n"

	if *updateGolden {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update)", err)
	}
	if got != string(want) {
		t.Fatalf("normalized output diverged from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
