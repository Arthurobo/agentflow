package parse

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
)

// testdataDir holds the committed fixture corpus. The fixtures keep the exact
// record shapes of real Claude Code transcripts but every piece of prose, path
// and identifier in them is synthetic; see testdata/fixtures/README.md.
const testdataDir = "../testdata"

// TestFixtureCorpusParsesClean asserts the invariant on the committed
// fixtures: every line of every fixture file normalizes to ≥1 event with zero
// errors — the corpus-shaped equivalent of the full-corpus sweep.
func TestFixtureCorpusParsesClean(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join(testdataDir, "fixtures", "*.jsonl"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	var lines, events int
	for _, f := range fixtures {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, raw := range strings.Split(string(data), "\n") {
				if strings.TrimSpace(raw) == "" {
					continue
				}
				p, err := Normalize([]byte(raw), eventmodel.SourceBackfill)
				if err != nil {
					t.Fatalf("fixture failed normalization: %v\nline: %.200s", err, raw)
				}
				if len(p.Events) == 0 {
					t.Fatalf("zero events for a parseable line: %.200s", raw)
				}
				lines++
				events += len(p.Events)
			}
		})
	}
	t.Logf("fixture corpus: %d lines -> %d normalized events", lines, events)
	if lines == 0 {
		t.Fatal("fixture corpus is empty")
	}
}

// TestFixtureBigLine: the synthetic ~2 MB tool_result line must parse and be
// flagged oversized with a content hash addressing the full content.
func TestFixtureBigLine(t *testing.T) {
	path := filepath.Join(testdataDir, "fixtures", "special-big-line.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("big-line fixture: %v", err)
	}
	if len(data) < 2*1024*1024 {
		t.Fatalf("big-line fixture must be ~2MB, got %d bytes", len(data))
	}
	p, err := Normalize(data, eventmodel.SourceBackfill)
	if err != nil {
		t.Fatal(err)
	}
	var toolResult *eventmodel.Event
	for _, ev := range p.Events {
		if ev.Type == eventmodel.EventToolResult {
			toolResult = ev
		}
	}
	if toolResult == nil {
		t.Fatal("expected a tool_result event from the big line")
	}
	if !toolResult.OversizedContent {
		t.Fatal("big content must be flagged oversized")
	}
	if !strings.HasPrefix(toolResult.ContentHash, "sha256:") || !strings.Contains(toolResult.Content, "truncated") {
		t.Fatalf("contentHash=%q content=%.80q", toolResult.ContentHash, toolResult.Content)
	}
}

// TestFixtureForwardedSubagent asserts forwarded-content detection: a record
// whose sessionId and session_id differ carries originSessionId.
func TestFixtureForwardedSubagent(t *testing.T) {
	path := filepath.Join(testdataDir, "fixtures", "special-forwarded.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("forwarded fixture: %v", err)
	}
	checked := 0
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		p, err := Normalize([]byte(raw), eventmodel.SourceBackfill)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range p.Events {
			if !strings.Contains(raw, `"sessionId"`) || !strings.Contains(raw, `"session_id"`) {
				continue
			}
			checked++
			if ev.OriginSessionID == "" {
				t.Fatalf("forwarded event must carry originSessionId: %+v", ev)
			}
		}
	}
	if checked == 0 {
		t.Fatal("forwarded fixture contained no dual-spelling records")
	}
}

// TestFixturePeerUser asserts peer attribution and origin retention on
// cross-session messages.
func TestFixturePeerUser(t *testing.T) {
	path := filepath.Join(testdataDir, "fixtures", "special-peer-user.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("peer fixture: %v", err)
	}
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		p, err := Normalize([]byte(raw), eventmodel.SourceBackfill)
		if err != nil {
			t.Fatal(err)
		}
		ev := p.Events[0]
		if !strings.HasPrefix(ev.Actor, "peer:") {
			t.Fatalf("peer origin must yield peer actor, got %q", ev.Actor)
		}
		if ev.Origin == nil || ev.Origin["kind"] != "peer" {
			t.Fatalf("origin object must be retained: %+v", ev.Origin)
		}
	}
}

// TestFixtureRetracted asserts retractedMessageUuids survive normalization.
func TestFixtureRetracted(t *testing.T) {
	path := filepath.Join(testdataDir, "fixtures", "special-retracted.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("retracted fixture: %v", err)
	}
	seenUUIDs := 0
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(raw) == "" || !strings.Contains(raw, "retractedMessageUuids") {
			continue
		}
		p, err := Normalize([]byte(raw), eventmodel.SourceBackfill)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range p.Events {
			if len(ev.RetractedUuids) > 0 {
				seenUUIDs += len(ev.RetractedUuids)
			}
		}
	}
	if seenUUIDs == 0 {
		t.Fatal("retracted fixture yielded no retractedUuids")
	}
}

// TestFixtureSidechain asserts isSidechain+agentId normalization on subagent
// records.
func TestFixtureSidechain(t *testing.T) {
	path := filepath.Join(testdataDir, "fixtures", "special-subagent.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("subagent fixture: %v", err)
	}
	checked := 0
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		p, err := Normalize([]byte(raw), eventmodel.SourceBackfill)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range p.Events {
			if !ev.IsSidechain {
				continue
			}
			checked++
			if ev.AgentID == "" {
				t.Fatalf("sidechain event must carry agentId: %+v", ev)
			}
		}
	}
	if checked == 0 {
		t.Fatal("subagent fixture yielded no sidechain events")
	}
}

// TestFixtureVersions asserts rawVersion stamping across the version series.
func TestFixtureVersions(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(testdataDir, "fixtures", "version-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 10 {
		t.Fatalf("expected ~19 version fixtures, got %d", len(files))
	}
	versions := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			p, err := Normalize([]byte(raw), eventmodel.SourceBackfill)
			if err != nil {
				t.Fatal(err)
			}
			for _, ev := range p.Events {
				if ev.RawVersion != "" {
					versions[ev.RawVersion] = true
				}
			}
		}
	}
	if len(versions) < 10 {
		t.Fatalf("expected ~20 distinct corpus versions stamped, got %d: %v", len(versions), versions)
	}
}

var (
	syntheticCWD  = regexp.MustCompile(`^/home/user/code/project-[a-z]$`)
	syntheticUUID = regexp.MustCompile(`^[0-9a-f]{8}-0000-4000-8000-[0-9a-f]{12}$`)
	homeDir       = regexp.MustCompile(`/home/([^/"\s]+)`)
	emailAddress  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+\.[A-Za-z]{2,}`)
	urlHost       = regexp.MustCompile(`https?://([^/\s"'<>)\]]+)`)
)

// TestFixturesAreSynthetic guards the corpus against a raw transcript capture
// being committed without going through testdata/sanitize.py: working
// directories and record ids must carry the sanitizer's synthetic shapes, and
// no string may name a real home directory, an email address or a URL host
// outside the placeholder set.
func TestFixturesAreSynthetic(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join(testdataDir, "fixtures", "*.jsonl"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	allowedHosts := map[string]bool{"example.com": true, "claude.ai": true, "github.com": true}
	for _, f := range fixtures {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, raw := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			var rec any
			if err := json.Unmarshal([]byte(raw), &rec); err != nil {
				t.Fatalf("%s:%d: %v", filepath.Base(f), i+1, err)
			}
			walkStrings(rec, "", func(key, val string) {
				where := fmt.Sprintf("%s:%d %s", filepath.Base(f), i+1, key)
				switch key {
				case "cwd":
					if !syntheticCWD.MatchString(val) {
						t.Errorf("%s: cwd %q is not a synthetic project path", where, val)
					}
				case "uuid", "parentUuid", "sessionId", "session_id":
					if !syntheticUUID.MatchString(val) {
						t.Errorf("%s: %q is not a renumbered id", where, val)
					}
				}
				for _, m := range homeDir.FindAllStringSubmatch(val, -1) {
					if m[1] != "user" {
						t.Errorf("%s: names home directory %q", where, m[0])
					}
				}
				if m := emailAddress.FindString(val); m != "" {
					t.Errorf("%s: contains an email address %q", where, m)
				}
				for _, m := range urlHost.FindAllStringSubmatch(val, -1) {
					if !allowedHosts[strings.ToLower(m[1])] {
						t.Errorf("%s: URL host %q is not a placeholder", where, m[1])
					}
				}
			})
		}
	}
}

// walkStrings calls fn for every string value in a decoded JSON document with
// the nearest object key (list items report their list's key).
func walkStrings(v any, key string, fn func(key, val string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			walkStrings(child, k, fn)
		}
	case []any:
		for _, child := range x {
			walkStrings(child, key, fn)
		}
	case string:
		fn(key, x)
	}
}
