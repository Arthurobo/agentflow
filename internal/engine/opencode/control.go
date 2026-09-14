package opencode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
)

// SessionID is the in-process session id used for routing Control calls to
// the right OpenCode session. Set after PreBind; required for Set* / Prompt /
// Abort calls. The spawner persists it as managed_sessions.session_id.
type SessionID = string

// Control is the OpenCode HTTP control implementation. Every method maps to
// one of the 162 verified OpenCode API paths; the package deliberately does
// not import any SDK — every endpoint is hand-rolled from the live shape so
// the contract stays visible and testable.
type Control struct {
	baseURL string
	hc      *http.Client

	mu     sync.Mutex
	sessID string // pinned at PreBind
}

// NewControl builds a control client bound to the given OpenCode base URL
// (e.g. "http://127.0.0.1:47312"). An empty baseURL is allowed for tests
// that never make HTTP calls; methods that need it return engine.ErrUnsupported.
func NewControl(baseURL string) *Control {
	return &Control{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc: &http.Client{
			Timeout: 8 * time.Second,
		},
	}
}

// BindSession pins the OpenCode session id this Control talks to. Call once
// after POST /session resolves the id, before any per-session method.
func (c *Control) BindSession(id string) {
	c.mu.Lock()
	c.sessID = id
	c.mu.Unlock()
}

// SessionID returns the bound session id ("" if not bound).
func (c *Control) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessID
}

// Capabilities implements engine.Control — OpenCode serves everything natively.
func (c *Control) Capabilities() engine.Caps {
	return engine.Caps{
		ListModels:      true,
		SetModel:        true,
		ListAgents:      true,
		SetAgent:        true,
		ListCommands:    true,
		AttachFiles:     true,
		RunCommand:      true,
		ListSkills:      true,
		ListProviders:   true,
		SetProvider:     true,
		ListMCPServers:  true,
		Prompt:          true,
		Abort:           true,
		ReplyPermission: true,
		ReplyQuestion:   true,
		Events:          true,
	}
}

// Permissions implements engine.Control via GET /permission.
func (c *Control) Permissions(ctx context.Context) ([]engine.PermissionRequest, error) {
	if c.baseURL == "" {
		return nil, engine.ErrUnsupported
	}
	var raw []engine.PermissionRequest
	if err := c.get(ctx, "/permission", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ReplyPermission implements engine.Control via POST /permission/{id}/reply.
func (c *Control) ReplyPermission(ctx context.Context, id string, allow bool, scope string) error {
	if c.baseURL == "" || id == "" {
		return engine.ErrUnsupported
	}
	response := "deny"
	if allow {
		switch scope {
		case "always":
			response = "always"
		default:
			response = "allow"
		}
	}
	body, _ := json.Marshal(map[string]string{
		"requestID": id,
		"response":  response,
	})
	return c.post(ctx, "/permission/"+id+"/reply", body, nil)
}

// Questions implements engine.Control via GET /question.
func (c *Control) Questions(ctx context.Context) ([]engine.Question, error) {
	if c.baseURL == "" {
		return nil, engine.ErrUnsupported
	}
	var raw []engine.Question
	if err := c.get(ctx, "/question", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ReplyQuestion implements engine.Control via POST /question/{id}/reply.
func (c *Control) ReplyQuestion(ctx context.Context, id, answer string) error {
	if c.baseURL == "" || id == "" {
		return engine.ErrUnsupported
	}
	body, _ := json.Marshal(map[string]any{
		"answers": [][]string{{answer}},
	})
	return c.post(ctx, "/question/"+id+"/reply", body, nil)
}

// Models implements engine.Control via GET /api/model.
//
// The path and the shape were BOTH wrong before. Read from the live spec at
// GET /doc on opencode 1.18.29: the only model route is /api/model, and the
// unprefixed /model is not a 404 but the web UI, served as HTML with HTTP 200,
// so the old call decoded a page into an empty list and Caps kept claiming
// ListModels while the picker came up empty. The body is {location, data},
// never {all, default}.
//
// modelEntry is deliberately NARROW, and that is a security property rather
// than tidiness. A live entry carries request.body.apiKey, a real provider
// credential, on EVERY model: verified on this machine, 66 of 66. Decoding
// into a struct with no such field is what keeps it out of anything we hand
// upstream. Do not widen this to map[string]any, and do not add a Raw field.
func (c *Control) Models(ctx context.Context) ([]engine.Model, error) {
	if c.baseURL == "" {
		return nil, engine.ErrUnsupported
	}
	var raw struct {
		Data []modelEntry `json:"data"`
	}
	if err := c.get(ctx, "/api/model", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]engine.Model, 0, len(raw.Data))
	for _, m := range raw.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, engine.Model{
			ID:          m.ID,
			DisplayName: firstNonEmpty(m.Name, m.ID),
			Provider:    m.ProviderID,
			Status:      m.Status,
		})
	}
	return out, nil
}

// modelEntry is the projection of OpenCode's ModelV2Info. Everything the
// credential could ride in (request, api, variants) is absent on purpose.
type modelEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ProviderID string `json:"providerID"`
	Status     string `json:"status"`
}

// Agents implements engine.Control via GET /agent.
func (c *Control) Agents(ctx context.Context) ([]engine.Agent, error) {
	if c.baseURL == "" {
		return nil, engine.ErrUnsupported
	}
	var raw []agentEntry
	if err := c.get(ctx, "/agent", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]engine.Agent, 0, len(raw))
	for _, a := range raw {
		if a.Name == "" {
			continue
		}
		out = append(out, engine.Agent{
			Name:        a.Name,
			Description: a.Description,
			Model:       a.Model,
			Default:     a.Default,
		})
	}
	return out, nil
}

type agentEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Model       string `json:"model"`
	Default     bool   `json:"default"`
}

// Commands implements engine.Control via GET /command.
func (c *Control) Commands(ctx context.Context) ([]engine.Command, error) {
	if c.baseURL == "" {
		return nil, engine.ErrUnsupported
	}
	var raw []commandEntry
	if err := c.get(ctx, "/command", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]engine.Command, 0, len(raw))
	for _, k := range raw {
		if k.Name == "" {
			continue
		}
		out = append(out, engine.Command{
			Name:        k.Name,
			Description: k.Description,
		})
	}
	return out, nil
}

type commandEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Skills implements engine.Control via GET /skill.
func (c *Control) Skills(ctx context.Context) ([]engine.Skill, error) {
	if c.baseURL == "" {
		return nil, engine.ErrUnsupported
	}
	var raw []skillEntry
	if err := c.get(ctx, "/skill", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]engine.Skill, 0, len(raw))
	for _, s := range raw {
		if s.Name == "" {
			continue
		}
		out = append(out, engine.Skill{
			Name:        s.Name,
			Description: s.Description,
			Path:        s.Path,
		})
	}
	return out, nil
}

type skillEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
}

// MCPServers implements engine.Control via GET /mcp.
func (c *Control) MCPServers(ctx context.Context) ([]engine.MCPServer, error) {
	if c.baseURL == "" {
		return nil, engine.ErrUnsupported
	}
	var raw []mcpEntry
	if err := c.get(ctx, "/mcp", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]engine.MCPServer, 0, len(raw))
	for _, m := range raw {
		out = append(out, engine.MCPServer{
			Name:   m.Name,
			Status: m.Status,
			URL:    m.URL,
		})
	}
	return out, nil
}

type mcpEntry struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	URL    string `json:"url"`
}

// Providers implements engine.Control via GET /provider.
func (c *Control) Providers(ctx context.Context) ([]engine.Provider, error) {
	if c.baseURL == "" {
		return nil, engine.ErrUnsupported
	}
	var raw map[string]providerEntry
	if err := c.get(ctx, "/provider", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]engine.Provider, 0, len(raw))
	for id, p := range raw {
		out = append(out, engine.Provider{
			ID:   firstNonEmpty(id, p.ID),
			Name: firstNonEmpty(p.Name, id),
		})
	}
	return out, nil
}

type providerEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// State implements engine.Control via GET /session/{id}.
func (c *Control) State(ctx context.Context) (engine.SessionState, error) {
	sid := c.SessionID()
	if c.baseURL == "" || sid == "" {
		return engine.SessionState{}, engine.ErrUnsupported
	}
	var raw map[string]any
	if err := c.get(ctx, "/session/"+sid, nil, &raw); err != nil {
		return engine.SessionState{}, err
	}
	out := engine.SessionState{Raw: raw}
	// model is an OBJECT on a session, not a string. Reading it as a string
	// silently produced an empty model on every read, so the picker could
	// never show what was actually selected.
	if ref, ok := modelRefFrom(raw["model"]); ok {
		out.Model = ref.String()
		out.Provider = ref.ProviderID
	} else if m, ok := raw["model"].(string); ok {
		out.Model = m
	} else if m, ok := raw["modelID"].(string); ok {
		out.Model = m
	}
	if a, ok := raw["agent"].(string); ok {
		out.Agent = a
	} else if a, ok := raw["agentName"].(string); ok {
		out.Agent = a
	}
	if p, ok := raw["provider"].(string); ok && p != "" {
		out.Provider = p
	}
	return out, nil
}

// SetModel implements engine.Control via POST /api/session/{id}/model.
//
// The body is {"model": {"providerID", "id"}}, verified against a live
// opencode serve and its own /doc. Two things were wrong before, and the
// second was hidden by the first: the payload was {"modelID": ...} at the top
// level, which answers 400 Missing key at ["model"]; correcting only the
// outer key answers 400 Missing key at ["model"]["id"], because the inner key
// is id and not modelID. Every apply failed as a 502.
//
// Only the FIRST slash is structural. An openrouter route is
// openrouter/anthropic/claude-3, where the provider is openrouter and the
// model half carries its own slashes.
func (c *Control) SetModel(ctx context.Context, model string) error {
	sid := c.SessionID()
	if c.baseURL == "" || sid == "" {
		return engine.ErrUnsupported
	}
	ref, err := ParseModelRef(model)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"model": ref})
	return c.post(ctx, "/api/session/"+sid+"/model", body, nil)
}

// ModelRef is OpenCode's own model reference: the provider and the model id
// as separate fields, never a single string.
type ModelRef struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
}

// String renders a ref back to the provider/model form the rest of the product
// uses, so one shape crosses the boundary and the other stays inside it.
func (m ModelRef) String() string {
	if m.ProviderID == "" {
		return m.ID
	}
	return m.ProviderID + "/" + m.ID
}

// ParseModelRef splits provider/model on the FIRST slash.
func ParseModelRef(model string) (ModelRef, error) {
	if err := ValidateModelRef(model); err != nil {
		return ModelRef{}, err
	}
	provider, rest, _ := strings.Cut(strings.TrimSpace(model), "/")
	return ModelRef{ProviderID: provider, ID: rest}, nil
}

// modelRefFrom decodes the object OpenCode actually stores on a session.
//
// Session.model is that same {providerID, id} object, so reading it as a
// string never matched and the active model never read back even after a
// correct set.
func modelRefFrom(v any) (ModelRef, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return ModelRef{}, false
	}
	id, _ := obj["id"].(string)
	if id == "" {
		id, _ = obj["modelID"].(string)
	}
	if id == "" {
		return ModelRef{}, false
	}
	provider, _ := obj["providerID"].(string)
	if provider == "" {
		provider, _ = obj["provider"].(string)
	}
	return ModelRef{ProviderID: provider, ID: id}, true
}

// SetAgent implements engine.Control via POST /api/session/{id}/agent. Same
// silent-HTML failure as SetModel on the unprefixed path.
func (c *Control) SetAgent(ctx context.Context, agent string) error {
	sid := c.SessionID()
	if c.baseURL == "" || sid == "" {
		return engine.ErrUnsupported
	}
	body, _ := json.Marshal(map[string]string{"agent": agent})
	return c.post(ctx, "/api/session/"+sid+"/agent", body, nil)
}

// SetProvider implements engine.Control via POST /session/{id}/model with a
// provider-prefixed id (e.g. "anthropic/claude-sonnet-4-5"). The OpenCode
// API does not expose a separate /session/{id}/provider — provider switching
// is implicit in model selection. We honor the call by rejecting it; the UI
// should switch via SetModel.
func (c *Control) SetProvider(_ context.Context, _ string) error {
	return engine.ErrUnsupported
}

// RunCommand implements engine.Control via POST /session/{id}/command.
func (c *Control) RunCommand(ctx context.Context, name, args string) error {
	sid := c.SessionID()
	if c.baseURL == "" || sid == "" {
		return engine.ErrUnsupported
	}
	body, _ := json.Marshal(map[string]string{"name": name, "args": args})
	return c.post(ctx, "/session/"+sid+"/command", body, nil)
}

// Prompt implements engine.Control via POST /session/{id}/message, which is
// one of the routes that really is unprefixed. The BODY was wrong: the spec
// puts parts at the TOP level, not inside a {role, parts} message envelope,
// and rejects unknown properties. Not on the critical path now that a member
// gets its brief from argv, but it is two lines and it was broken.
func (c *Control) Prompt(ctx context.Context, text string) error {
	sid := c.SessionID()
	if c.baseURL == "" || sid == "" {
		return engine.ErrUnsupported
	}
	body, _ := json.Marshal(map[string]any{
		"parts": []map[string]any{
			{"type": "text", "text": text},
		},
	})
	return c.post(ctx, "/session/"+sid+"/message", body, nil)
}

// PromptWithFiles implements engine.Control natively: OpenCode's message API
// takes file parts alongside the text.
//
// The shape is FilePartInput, verified against the installed 1.18.29's own
// SDK types (@opencode-ai/sdk types.gen.d.ts):
//
//	{ id?, type: "file", mime, filename?, url, source? }
//
// url carries a base64 DATA URL rather than a file:// path. That is not a
// preference — the binary's image pipeline normalizes every part whose mime
// starts with "image/" and raises ImageInvalidDataUrlError ("Image URL must
// be a base64 data URL") for anything else, and its call site only catches
// ResizerUnavailableError, so a file:// image part fails the whole message
// save. The bytes therefore go agentd -> the LOCAL opencode control API on
// 127.0.0.1; they still never touch the relay, which is what the design
// promises.
//
// File parts come BEFORE the text part so the model reads the images as
// context for the sentence rather than the other way round.
func (c *Control) PromptWithFiles(ctx context.Context, text string, files []engine.Attachment) error {
	sid := c.SessionID()
	if c.baseURL == "" || sid == "" {
		return engine.ErrUnsupported
	}
	if len(files) == 0 {
		return c.Prompt(ctx, text)
	}

	parts := make([]map[string]any, 0, len(files)+1)
	for _, f := range files {
		raw, err := os.ReadFile(f.Path) //nolint:gosec // path came from our own attachment registry, never from a client
		if err != nil {
			return fmt.Errorf("opencode: reading attachment %s: %w", f.ID, err)
		}
		mime := f.Mime
		if mime == "" {
			mime = "application/octet-stream"
		}
		name := f.Name
		if name == "" {
			name = filepath.Base(f.Path)
		}
		parts = append(parts, map[string]any{
			"type":     "file",
			"mime":     mime,
			"filename": name,
			"url":      "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw),
		})
	}
	if strings.TrimSpace(text) == "" {
		text = "Please look at the attached image(s)."
	}
	parts = append(parts, map[string]any{"type": "text", "text": text})

	body, err := json.Marshal(map[string]any{"parts": parts})
	if err != nil {
		return err
	}
	return c.post(ctx, "/session/"+sid+"/message", body, nil)
}

// Abort implements engine.Control via POST /session/{id}/abort.
func (c *Control) Abort(ctx context.Context) error {
	sid := c.SessionID()
	if c.baseURL == "" || sid == "" {
		return engine.ErrUnsupported
	}
	return c.post(ctx, "/session/"+sid+"/abort", nil, nil)
}

// WaitReady polls GET /api/health until it answers or the budget elapses.
// The OpenCode TUI starts its HTTP server inside its first ~250 ms; the
// budget here (default 12 s) leaves generous headroom for cold start on
// slow disks.
func (c *Control) WaitReady(ctx context.Context, budget time.Duration) error {
	if c.baseURL == "" {
		return errors.New("opencode: WaitReady: empty baseURL")
	}
	if budget <= 0 {
		budget = 12 * time.Second
	}
	end := time.Now().Add(budget)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/health", nil)
		resp, err := c.hc.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(end) {
			return fmt.Errorf("opencode: control API never came up at %s within %s", c.baseURL, budget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// WaitModels polls GET /api/model until it names something or the budget
// elapses, and returns whatever it has at that point.
//
// Health readiness is NOT model readiness. Measured on this machine against
// opencode 1.18.29: after /api/health first answers, /api/model returns 0
// entries, and about three seconds later it returns 66 and stays there. A
// caller that asks once immediately after WaitReady therefore gets an empty
// list with no error, which every layer above then reports faithfully as "it
// named nothing" — honest, and wrong.
//
// The empty return at the deadline is deliberate and is NOT an error: a
// machine with no authenticated provider genuinely has no models, and that is
// the answer the picker should show. The budget is the only thing separating
// the two cases, so it has to be long enough to cover the fill and short
// enough that the genuine-empty case is not a hang. 12s, matching the wait
// budget PostStart already uses, against a measured 3s fill.
func (c *Control) WaitModels(ctx context.Context, budget time.Duration) ([]engine.Model, error) {
	if c.baseURL == "" {
		return nil, engine.ErrUnsupported
	}
	if budget <= 0 {
		budget = 12 * time.Second
	}
	end := time.Now().Add(budget)
	var last []engine.Model
	for {
		out, err := c.Models(ctx)
		if err != nil {
			return nil, err
		}
		if len(out) > 0 {
			return out, nil
		}
		last = out
		if time.Now().After(end) {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// --- HTTP plumbing --------------------------------------------------------------

func (c *Control) get(ctx context.Context, path string, query url.Values, into any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("opencode: GET %s: %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if into != nil {
		return json.NewDecoder(resp.Body).Decode(into)
	}
	return nil
}

func (c *Control) post(ctx context.Context, path string, body []byte, into any) error {
	u := c.baseURL + path
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("opencode: POST %s: %d: %s", path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if into != nil {
		return json.NewDecoder(resp.Body).Decode(into)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
