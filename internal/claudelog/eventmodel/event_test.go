package eventmodel

import (
	"strings"
	"testing"
)

func TestContentHashOfString(t *testing.T) {
	got := ContentHashOfString("hello")
	if !strings.HasPrefix(got, ContentHashPrefix) || len(got) != len(ContentHashPrefix)+64 {
		t.Fatalf("bad hash shape: %q", got)
	}
	if got2 := ContentHashOfString("hello"); got2 != got {
		t.Fatalf("hash not deterministic: %q vs %q", got, got2)
	}
	if got3 := ContentHashOfString("hello!"); got3 == got {
		t.Fatalf("different content should hash differently")
	}
}

func TestSplitOversized(t *testing.T) {
	small := []byte("small content")
	preview, hash, oversized := SplitOversized(small, 64*1024)
	if oversized || preview != "small content" || hash == "" {
		t.Fatalf("small content: oversized=%v preview=%q hash=%q", oversized, preview, hash)
	}

	big := []byte(strings.Repeat("x", 70*1024))
	preview, hash, oversized = SplitOversized(big, 64*1024)
	if !oversized {
		t.Fatal("big content must be flagged oversized")
	}
	if hash != ContentHashOfString(string(big)) {
		t.Fatalf("hash must address the FULL content, got %q", hash)
	}
	if len(preview) > 64*1024 {
		t.Fatalf("preview must be small, got %d bytes", len(preview))
	}
	if !strings.Contains(preview, "truncated") {
		t.Fatalf("preview must carry an explicit truncation notice")
	}

	edge := make([]byte, 64*1024)
	_, _, oversized = SplitOversized(edge, 64*1024)
	if oversized {
		t.Fatal("exactly-threshold content is not oversized")
	}
}

func TestSplitOversizedEmpty(t *testing.T) {
	preview, hash, oversized := SplitOversized(nil, 0)
	if oversized || preview != "" || hash != "" {
		t.Fatalf("empty content: oversized=%v preview=%q hash=%q", oversized, preview, hash)
	}
}

func TestProjectFromCWD(t *testing.T) {
	cases := map[string]string{
		"/home/u/Desktop/code/webapp":   "webapp",
		"/home/user/Desktop/code/myapp": "myapp",
		"/":                             "",
		"":                              "",
		".":                             "",
		"/tmp":                          "tmp",
	}
	for in, want := range cases {
		if got := ProjectFromCWD(in); got != want {
			t.Errorf("ProjectFromCWD(%q) = %q, want %q", in, got, want)
		}
	}
}
