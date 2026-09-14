// mailapi_test.go — the agent surface. The first test is the one that matters:
// the pool has four connections and a poll is held for minutes, so a handler
// that waits with a connection in hand deadlocks the application on the fourth
// agent. Everything else here is the contract an external agent joins on.
package mailapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

type harness struct {
	t      *testing.T
	st     *store.Store
	srv    *Server
	http   *httptest.Server
	base   string
	loop   *store.Loop
	tokens map[string]string
}

func newHarness(t *testing.T, roles []string, cfg Config) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Above the default member cap: the long-poll test below creates 14 roles.
	loop := &store.Loop{Task: "Fix the flaky guest export", CWD: "/tmp/target", MaxMembers: 64}
	tokens, err := st.CreateLoop(context.Background(), loop, roles)
	if err != nil {
		t.Fatalf("create loop: %v", err)
	}
	if cfg.MaxWait == 0 {
		cfg.MaxWait = 10 * time.Second
	}
	if cfg.PollTick == 0 {
		cfg.PollTick = 2 * time.Second
	}
	if cfg.RetrySeconds == 0 {
		cfg.RetrySeconds = 1
	}
	srv := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{t: t, st: st, srv: srv, http: ts, base: ts.URL, loop: loop, tokens: tokens}
}

func (h *harness) do(token, method, path string, body any) (*http.Response, map[string]any) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.http.URL+Prefix+path, rdr)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("X-Agent-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func errorOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no error object: %v", body)
	}
	return e
}

// THE hazard. More concurrent long polls than the pool has connections, and
// unrelated queries must keep answering throughout. A handler that waits with
// a connection or a transaction held deadlocks everything on the fourth agent,
// dashboard included.
func TestLongPollHoldsNoDatabaseConnection(t *testing.T) {
	roles := []string{store.RoleOrchestrator, store.RoleEngineer}
	for i := 1; i <= 12; i++ {
		roles = append(roles, "INVESTIGATION#"+strconv.Itoa(i))
	}
	h := newHarness(t, roles, Config{})
	const pollers = 12 // the pool is 4

	var wg sync.WaitGroup
	for i := 1; i <= pollers; i++ {
		token := h.tokens["INVESTIGATION#"+strconv.Itoa(i)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := h.do(token, "GET", "/inbox?wait=6", nil)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("poll returned %d", resp.StatusCode)
			}
		}()
	}

	// The pollers must actually be parked, or this test proves nothing: a
	// poll that returned immediately holds no connection either.
	parked := 0
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if n := h.st.WaiterCount(); n > parked {
			parked = n
		}
		if parked >= pollers {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if parked < pollers {
		t.Fatalf("only %d of %d pollers ever parked; the concurrency this test needs never happened", parked, pollers)
	}

	// With every poller parked, unrelated work must still get through.
	for i := 0; i < 60; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, err := h.st.Counts(ctx); err != nil {
			cancel()
			t.Fatalf("unrelated query %d failed while %d agents were polling: %v", i, pollers, err)
		}
		if _, err := h.st.GetLoop(ctx, h.loop.ID); err != nil {
			cancel()
			t.Fatalf("unrelated loop read %d failed while %d agents were polling: %v", i, pollers, err)
		}
		cancel()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("pollers never returned")
	}
}

// A parked poll returns the moment mail lands, not at its deadline.
func TestLongPollReturnsTheMomentMailLands(t *testing.T) {
	h := newHarness(t, nil, Config{})
	orch, inv := h.tokens[store.RoleOrchestrator], h.tokens["INVESTIGATION"]

	started := time.Now()
	type result struct {
		body map[string]any
		took time.Duration
	}
	got := make(chan result, 1)
	go func() {
		_, body := h.do(inv, "GET", "/inbox?wait=10", nil)
		got <- result{body, time.Since(started)}
	}()

	for h.st.WaiterCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if _, body := h.do(orch, "POST", "/messages", map[string]any{
		"to": "INVESTIGATION", "body": "your brief",
	}); body["id"] == nil {
		t.Fatalf("post failed: %v", body)
	}

	select {
	case r := <-got:
		msgs := r.body["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("poll returned %d messages", len(msgs))
		}
		// The safety tick is 2s, so anything at or above that proves only
		// that the tick fired. The wake-up must be what returned this.
		if r.took > time.Second {
			t.Fatalf("poll took %s; it waited for the safety tick instead of the wake-up channel", r.took)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("the poll never returned")
	}
}

// Member tokens are their own class. A device token is not a member, an
// unknown token is nobody, and the engineer's empty token_hash never
// authenticates.
func TestMemberTokenIsItsOwnClass(t *testing.T) {
	h := newHarness(t, nil, Config{})
	ctx := context.Background()

	deviceToken := "device-token-not-a-member"
	if err := h.st.UpsertDevice(ctx, &store.Device{
		ID: "dev1", Name: "phone", Kind: "device", TokenHash: store.HashToken(deviceToken),
	}, 0); err != nil {
		t.Fatalf("device: %v", err)
	}
	if resp, _ := h.do(deviceToken, "GET", "/whoami", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a device token must not act as a member, got %d", resp.StatusCode)
	}

	// And the other direction: a member token is not a device, so it cannot
	// reach the device surface's gate.
	memberToken := h.tokens[store.RoleOrchestrator]
	d, err := h.st.VerifyDevice(ctx, memberToken)
	if err != nil || d != nil {
		t.Fatalf("a member token must not verify as a device: %v %v", d, err)
	}

	// The two refusals are deliberately distinguishable, because they come
	// from different layers. An empty token is stopped at the HTTP layer
	// BEFORE the store is consulted; the store's own empty-token guard is
	// the second line, not the only one.
	resp, body := h.do("", "GET", "/whoami", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an empty token must be refused, got %d", resp.StatusCode)
	}
	if msg := errorOf(t, body)["message"]; msg != "missing X-Agent-Token" {
		t.Fatalf("an empty token must be refused by the HTTP layer, not fall through to a lookup: %q", msg)
	}
	resp, body = h.do("nonsense", "GET", "/whoami", nil)
	if resp.StatusCode != http.StatusUnauthorized || errorOf(t, body)["message"] != "unknown agent token" {
		t.Fatalf("an unknown token must be refused by the lookup: %d %v", resp.StatusCode, body)
	}
	// The engineer holds no token at all, and hashing the empty string must
	// not become one.
	if _, ok := h.tokens[store.RoleEngineer]; ok {
		t.Fatal("the engineer must be issued no token")
	}
	if m, _, _ := h.st.LoopMemberByToken(ctx, ""); m != nil {
		t.Fatal("an empty token must never authenticate")
	}
}

// bodies=false omits the body and reports its size. It never cuts one.
func TestInboxOmitsBodiesRatherThanCutting(t *testing.T) {
	h := newHarness(t, nil, Config{})
	orch, inv := h.tokens[store.RoleOrchestrator], h.tokens["INVESTIGATION"]
	body := strings.Repeat("é", 50_000)
	h.do(orch, "POST", "/messages", map[string]any{"to": "INVESTIGATION", "body": body})

	_, peek := h.do(inv, "GET", "/inbox?wait=0&bodies=false", nil)
	msgs := peek["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if _, present := m["body"]; present {
		t.Fatal("bodies=false must OMIT the body key, not blank or cut it")
	}
	if m["bodyChars"].(float64) != 50_000 {
		t.Fatalf("bodyChars = %v, want 50000", m["bodyChars"])
	}

	// And the whole thing is still one fetch away, byte for byte.
	id := m["id"].(string)
	_, full := h.do(inv, "GET", "/messages/"+id, nil)
	if full["body"].(string) != body {
		t.Fatal("fetching by id must return the whole body")
	}
	if full["bodyChars"].(float64) != 50_000 {
		t.Fatalf("bodyChars on the full read = %v", full["bodyChars"])
	}
}

// An ended loop parks its agents. Only a dismissal stops one.
func TestEndedLoopIdlesAndOnlyDismissalStops(t *testing.T) {
	h := newHarness(t, nil, Config{})
	ctx := context.Background()
	inv := h.tokens["INVESTIGATION"]

	if _, err := h.st.EndLoop(ctx, h.loop.ID, store.LoopComplete, "round one done", false); err != nil {
		t.Fatalf("end: %v", err)
	}
	_, parked := h.do(inv, "GET", "/inbox?wait=0", nil)
	if parked["stop"] != false || parked["idle"] != true {
		t.Fatalf("an ended loop must park, not stop: %v", parked)
	}
	sig := parked["signal"].(map[string]any)
	if !strings.Contains(sig["idleReason"].(string), "Keep waiting") {
		t.Fatalf("the idle reason must tell it to keep waiting: %v", sig)
	}
	resp, body := h.do(inv, "POST", "/notes", map[string]any{"body": "late"})
	if resp.StatusCode != http.StatusConflict || errorOf(t, body)["code"] != "loop_ended" {
		t.Fatalf("an ended loop must refuse writes: %d %v", resp.StatusCode, body)
	}

	if _, err := h.st.DismissLoopMembers(ctx, h.loop.ID); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	_, stopped := h.do(inv, "GET", "/inbox?wait=0", nil)
	if stopped["stop"] != true || stopped["idle"] != false {
		t.Fatalf("a dismissed member must be told to stop: %v", stopped)
	}
	resp, body = h.do(inv, "POST", "/notes", map[string]any{"body": "later"})
	if resp.StatusCode != http.StatusConflict || errorOf(t, body)["code"] != "member_dismissed" {
		t.Fatalf("a dismissed member must be refused: %d %v", resp.StatusCode, body)
	}
}
