// rewait_test.go — the report IS the re-entry.
//
// Waiting again was a separate command issued at the exact moment a model
// feels finished, which is the moment it stops following instructions. Both
// live investigators reported and then stopped polling; the orchestrator's
// follow-up brief has never been read by anything.
package mailapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

func (h *harness) post(token, path string, body any, headers map[string]string) (*http.Response, map[string]any) {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, h.http.URL+Prefix+path, bytes.NewReader(raw))
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	req.Header.Set("X-Agent-Token", token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("post %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	body2, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body2, &out)
	return resp, out
}

// note puts something on the record, because reporting with no notes is
// refused and that refusal is correct: a worker's session can be retired the
// moment it delivers, and the notes are all its role keeps.
func (h *harness) note(token, body string) {
	h.t.Helper()
	resp, out := h.post(token, "/notes", map[string]any{"body": body}, nil)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("note: %d %v", resp.StatusCode, out)
	}
}

// The reply to a report is the next brief, delivered in the same call.
func TestPostWithWaitReturnsTheReplyThatLandsDuringTheWait(t *testing.T) {
	h := newHarness(t, []string{store.RoleOrchestrator, "INVESTIGATION"}, Config{MaxWait: 10 * time.Second})
	inv := h.tokens["INVESTIGATION"]
	h.note(inv, "the export query is unbounded above 500 rows")

	// The orchestrator answers a moment after the report is posted.
	go func() {
		time.Sleep(150 * time.Millisecond)
		h.post(h.tokens[store.RoleOrchestrator], "/messages",
			map[string]any{"to": "INVESTIGATION", "body": "now check the second table"}, nil)
	}()

	resp, out := h.post(inv, "/messages?wait=5",
		map[string]any{"to": store.RoleOrchestrator, "body": "the count is 8,468"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post: %d %v", resp.StatusCode, out)
	}
	posted, _ := out["posted"].(map[string]any)
	if posted == nil || posted["id"] == nil {
		t.Fatalf("the receipt must survive the wait: %v", out)
	}
	inbox, _ := out["inbox"].(map[string]any)
	if inbox == nil {
		t.Fatalf("a waiting post must carry an inbox: %v", out)
	}
	msgs, _ := inbox["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("the reply that landed during the wait must be returned, got %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["body"] != "now check the second table" {
		t.Fatalf("wrong message came back: %v", first)
	}
}

// A quiet queue returns an empty inbox at the deadline, rather than hanging or
// erroring. The receipt is still there: the post is committed before the wait.
func TestPostWithWaitReturnsAnEmptyInboxAtTheDeadline(t *testing.T) {
	h := newHarness(t, []string{store.RoleOrchestrator, "INVESTIGATION"}, Config{MaxWait: 10 * time.Second})

	h.note(h.tokens["INVESTIGATION"], "the export query is unbounded above 500 rows")
	start := time.Now()
	resp, out := h.post(h.tokens["INVESTIGATION"], "/messages?wait=1",
		map[string]any{"to": store.RoleOrchestrator, "body": "the count is 8,468"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post: %d %v", resp.StatusCode, out)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("returned in %s: it cannot have waited", elapsed)
	}
	posted, _ := out["posted"].(map[string]any)
	if posted == nil || posted["id"] == nil {
		t.Fatalf("the report must never be lost to a quiet wait: %v", out)
	}
	inbox, _ := out["inbox"].(map[string]any)
	if inbox == nil {
		t.Fatalf("a waiting post must carry an inbox even when empty: %v", out)
	}
	if msgs, _ := inbox["messages"].([]any); len(msgs) != 0 {
		t.Fatalf("nothing was sent, so nothing must come back, got %d", len(msgs))
	}
}

// Without wait it is the old shape exactly, so nothing that does not ask for
// the new behaviour is changed by it.
func TestPostWithoutWaitIsUnchanged(t *testing.T) {
	h := newHarness(t, []string{store.RoleOrchestrator, "INVESTIGATION"}, Config{})
	h.note(h.tokens["INVESTIGATION"], "the export query is unbounded above 500 rows")
	resp, out := h.post(h.tokens["INVESTIGATION"], "/messages",
		map[string]any{"to": store.RoleOrchestrator, "body": "the count is 8,468"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post: %d %v", resp.StatusCode, out)
	}
	if out["id"] == nil {
		t.Fatalf("a plain post answers with the receipt at the top level: %v", out)
	}
	if _, wrapped := out["posted"]; wrapped {
		t.Fatalf("a post that did not ask to wait must not be wrapped: %v", out)
	}
}

// A retry after a dead connection must not write the report twice. The post is
// committed before the wait begins, so "died during the wait" and "died before
// the write" look identical to the client; the key is how it tells us which.
func TestARetryWithTheSameIdempotencyKeyReturnsTheOriginal(t *testing.T) {
	h := newHarness(t, []string{store.RoleOrchestrator, "INVESTIGATION"}, Config{})
	inv := h.tokens["INVESTIGATION"]
	h.note(inv, "the export query is unbounded above 500 rows")
	headers := map[string]string{"Idempotency-Key": "report-1"}

	_, first := h.post(inv, "/messages", map[string]any{
		"to": store.RoleOrchestrator, "body": "the count is 8,468",
	}, headers)
	_, second := h.post(inv, "/messages", map[string]any{
		"to": store.RoleOrchestrator, "body": "the count is 8,468",
	}, headers)

	if first["id"] == nil || first["id"] != second["id"] {
		t.Fatalf("a retry must return the original id: %v then %v", first["id"], second["id"])
	}

	// And exactly one row exists. An id match alone would also pass if the
	// handler returned the same id while writing a second row.
	rows, err := h.st.ListLoopMessages(h.t.Context(), h.loop.ID, 50)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	count := 0
	for _, m := range rows {
		if m.Body == "the count is 8,468" && m.MirrorOf == "" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the retry wrote a second report: %d rows", count)
	}

	// A different key is a different message, or a worker could never send
	// the same sentence twice.
	_, third := h.post(inv, "/messages", map[string]any{
		"to": store.RoleOrchestrator, "body": "the count is 8,468",
	}, map[string]string{"Idempotency-Key": "report-2"})
	if third["id"] == first["id"] {
		t.Fatalf("a different key must write a new message, got %v", third["id"])
	}
}

// No key means no deduplication: two identical posts are two messages.
func TestWithoutAKeyIdenticalPostsAreTwoMessages(t *testing.T) {
	h := newHarness(t, []string{store.RoleOrchestrator, "INVESTIGATION"}, Config{})
	inv := h.tokens["INVESTIGATION"]
	h.note(inv, "the export query is unbounded above 500 rows")
	_, first := h.post(inv, "/messages", map[string]any{
		"to": store.RoleOrchestrator, "body": "same words",
	}, nil)
	_, second := h.post(inv, "/messages", map[string]any{
		"to": store.RoleOrchestrator, "body": "same words",
	}, nil)
	if first["id"] == second["id"] {
		t.Fatalf("without a key nothing is deduplicated, got %v twice", first["id"])
	}
}
