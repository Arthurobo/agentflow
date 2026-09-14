package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Session is the minimal projection of an OpenCode session returned by
// POST /session.
type Session struct {
	ID        string `json:"id"`
	Slug      string `json:"slug,omitempty"`
	Directory string `json:"directory,omitempty"`
	ProjectID string `json:"projectID,omitempty"`
	Title     string `json:"title,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

// CreateSession issues POST /session to pre-create an OpenCode session for
// the given directory. Returns the session id so the spawner can persist it
// BEFORE launching the TUI process.
//
// body is the raw JSON document — currently {title, directory}. Pass nil for
// the minimum useful body.
//
// The OpenCode TUI must already be running on baseURL; call Control.WaitReady
// first.
func CreateSession(ctx context.Context, baseURL, dir, title string) (*Session, error) {
	if baseURL == "" {
		return nil, errors.New("opencode: CreateSession: empty baseURL")
	}
	payload, _ := json.Marshal(map[string]string{
		"directory": dir,
		"title":     title,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/session", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("opencode: POST /session: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out Session
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, fmt.Errorf("opencode: POST /session: response missing id")
	}
	return &out, nil
}

// FocusSession tells the live TUI to switch to an existing session via
// POST /tui/select-session. The TUI itself maintains "active session" —
// without this the engine may keep rendering a different session even after
// our control calls succeed. Best-effort: a 404 here is logged, not fatal.
func FocusSession(ctx context.Context, baseURL, sessionID string) error {
	if baseURL == "" || sessionID == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"sessionID": sessionID})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/tui/select-session",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		// surface but never block — the spawn proceeds
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("opencode: focus session: %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
