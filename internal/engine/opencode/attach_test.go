package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/engine"
)

// OpenCode takes files natively. The shape below was read off the installed
// 1.18.29's own SDK types (FilePartInput: {id?, type:"file", mime,
// filename?, url, source?}), not guessed — and the url is a base64 data URL
// because the binary's image pipeline raises ImageInvalidDataUrlError
// ("Image URL must be a base64 data URL") for anything else on an image
// part, at a call site that does not catch it.
func TestPromptWithFilesSendsFilePartsBeforeTheText(t *testing.T) {
	dir := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3, 4}
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, png, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var body map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewControl(srv.URL)
	c.BindSession("ses_1")
	if err := c.PromptWithFiles(context.Background(), "why is this misaligned?",
		[]engine.Attachment{{ID: "a1", Path: path, Mime: "image/png", Name: "shot.png", Bytes: int64(len(png))}}); err != nil {
		t.Fatalf("PromptWithFiles: %v", err)
	}

	if gotPath != "/session/ses_1/message" {
		t.Fatalf("posted to %q", gotPath)
	}
	parts, _ := body["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("want a file part and a text part, got %v", body)
	}

	file, _ := parts[0].(map[string]any)
	if file["type"] != "file" {
		t.Fatalf("the FILE part must come first so the model reads the image as context: %v", parts)
	}
	if file["mime"] != "image/png" {
		t.Fatalf("mime = %v", file["mime"])
	}
	if file["filename"] != "shot.png" {
		t.Fatalf("filename = %v", file["filename"])
	}
	url, _ := file["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("url must be a base64 data URL, got %.40q", url)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("url payload is not base64: %v", err)
	}
	if string(decoded) != string(png) {
		t.Fatal("the data URL does not carry the file's bytes")
	}

	text, _ := parts[1].(map[string]any)
	if text["type"] != "text" || text["text"] != "why is this misaligned?" {
		t.Fatalf("text part = %v", text)
	}
}

// An attachment with no message still says what to do with it.
func TestPromptWithFilesSuppliesTextWhenTheMessageIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.png")
	if err := os.WriteFile(path, []byte{0x89, 'P', 'N', 'G'}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}))
	defer srv.Close()

	c := NewControl(srv.URL)
	c.BindSession("ses_1")
	if err := c.PromptWithFiles(context.Background(), "   ",
		[]engine.Attachment{{ID: "a", Path: path, Mime: "image/png"}}); err != nil {
		t.Fatalf("PromptWithFiles: %v", err)
	}
	parts, _ := body["parts"].([]any)
	text, _ := parts[len(parts)-1].(map[string]any)
	if text["text"] != "Please look at the attached image(s)." {
		t.Fatalf("empty message should still instruct, got %v", text["text"])
	}
}

// Caps must say it natively, or the host will build a path-in-text turn for
// an engine that could have had the image itself.
func TestOpenCodeDeclaresNativeAttachments(t *testing.T) {
	if !NewControl("http://127.0.0.1:1").Capabilities().AttachFiles {
		t.Fatal("opencode takes file parts; caps.attachFiles must be true")
	}
}

// A control with no session bound cannot post anywhere, and says so rather
// than half-sending.
func TestPromptWithFilesNeedsASession(t *testing.T) {
	c := NewControl("http://127.0.0.1:1")
	err := c.PromptWithFiles(context.Background(), "hi", []engine.Attachment{{ID: "a", Path: "/nope"}})
	if err != engine.ErrUnsupported {
		t.Fatalf("unbound control: %v, want ErrUnsupported", err)
	}
}

// A file that has vanished between upload and send is an error the caller
// can act on, not a silently text-only turn.
func TestPromptWithFilesFailsLoudlyOnAMissingFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewControl(srv.URL)
	c.BindSession("ses_1")
	err := c.PromptWithFiles(context.Background(), "hi",
		[]engine.Attachment{{ID: "a", Path: filepath.Join(t.TempDir(), "gone.png")}})
	if err == nil || !strings.Contains(err.Error(), "reading attachment") {
		t.Fatalf("want a read error naming the attachment, got %v", err)
	}
}
