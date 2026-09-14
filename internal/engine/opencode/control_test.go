package opencode_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/engine/opencode"
)

// fakeServer is the minimum OpenCode API surface needed by the Control
// client + the binding helpers. It records each call so the test can assert
// shape, and lets us simulate a successful SetModel -> State round-trip.
type fakeServer struct {
	models     []engine.Model
	agents     []engine.Agent
	modelsErr  error
	setModel   bool
	setAgent   bool
	promptBody map[string]any
	lastState  engine.SessionState
	created    *opencode.Session
}

func newFake(t *testing.T) (*httptest.Server, *fakeServer) {
	t.Helper()
	fs := &fakeServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"healthy":true}`))
	})
	// The legacy unprefixed routes are the trap, reproduced exactly: opencode
	// 1.18.29 answers them with its web UI as HTML at HTTP 200, never a 404,
	// so a client on the wrong path decodes a page into an empty list and
	// reports success. Verified against the live GET /doc route table.
	webUI := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!doctype html><html><body>opencode</body></html>"))
	}
	mux.HandleFunc("/model", webUI)
	mux.HandleFunc("/api/model", func(w http.ResponseWriter, r *http.Request) {
		if fs.modelsErr != nil {
			http.Error(w, fs.modelsErr.Error(), http.StatusInternalServerError)
			return
		}
		// Shape from the live spec: {location, data}. Every entry carries a
		// real provider credential at request.body.apiKey — 66 of 66 on the
		// probe — so the fake carries one too. A projection that widens is
		// then a test failure rather than a leak nobody notices.
		entries := []map[string]any{}
		for _, m := range fs.models {
			entries = append(entries, map[string]any{
				"id": m.ID, "name": m.DisplayName, "providerID": m.Provider,
				"status": "active", "enabled": true,
				"cost":  map[string]any{"input": 3, "output": 15},
				"limit": map[string]any{"context": 200000},
				"request": map[string]any{
					"headers": map[string]any{"x-api-key": "sk-live-header-secret"},
					"body":    map[string]any{"apiKey": "sk-live-body-secret"},
				},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"location": map[string]any{"directory": "/tmp"},
			"data":     entries,
		})
	})
	mux.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) {
		out := []agentWire{}
		for _, a := range fs.agents {
			out = append(out, agentWire{Name: a.Name, Description: a.Description, Model: a.Model, Default: a.Default})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/session/")
		switch {
		case strings.HasSuffix(path, "/model") && r.Method == http.MethodPost:
			fs.setModel = true
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/agent") && r.Method == http.MethodPost:
			fs.setAgent = true
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/session/")
		switch {
		case strings.HasSuffix(path, "/model") && r.Method == http.MethodPost:
			webUI(w, r) // the legacy path: looks fine, changes nothing
		case strings.HasSuffix(path, "/agent") && r.Method == http.MethodPost:
			webUI(w, r)
		case strings.HasSuffix(path, "/message") && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			fs.promptBody = body
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/abort") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
		default:
			// GET /session/{id} returns the canonical state
			if fs.created != nil && path == fs.created.ID {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":       fs.created.ID,
					"model":    fs.lastState.Model,
					"agent":    fs.lastState.Agent,
					"provider": fs.lastState.Provider,
				})
				return
			}
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		fs.created = &opencode.Session{ID: "ses_fake", Directory: "x"}
		_ = json.NewEncoder(w).Encode(fs.created)
	})
	mux.HandleFunc("/tui/select-session", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux), fs
}

type agentWire struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Model       string `json:"model"`
	Default     bool   `json:"default"`
}

// TestModelsAndAgents asserts the wire format matches what the live binary
// returns: models.{id,name,providerID,default}, agents as a flat array.
func TestModelsAndAgents(t *testing.T) {
	srv, fs := newFake(t)
	defer srv.Close()
	fs.models = []engine.Model{
		{ID: "anthropic/claude-sonnet-4-5", DisplayName: "Sonnet 4.5", Provider: "anthropic", Default: true},
		{ID: "anthropic/claude-haiku-4-5", DisplayName: "Haiku 4.5", Provider: "anthropic"},
	}
	fs.agents = []engine.Agent{{Name: "build", Description: "Build things", Default: true}}

	c := opencode.NewControl(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	models, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("Models: %+v", models)
	}
	// GET /api/model has no "default" key at all, so nothing may claim one.
	if models[0].Default || models[0].Status != "active" {
		t.Fatalf("Models: default must not be invented, status must come through: %+v", models[0])
	}
	agents, err := c.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "build" || !agents[0].Default {
		t.Fatalf("Agents: %+v", agents)
	}
}

// TestSetModelAndState asserts the live cycle: SetModel -> POST /session/{id}/model,
// State -> GET /session/{id} reflects the change. This is the contract the
// control sheet's "optimistic apply + re-read truth" depends on.
func TestSetModelAndState(t *testing.T) {
	srv, fs := newFake(t)
	defer srv.Close()
	fs.created = &opencode.Session{ID: "ses_fake"}

	c := opencode.NewControl(srv.URL)
	c.BindSession("ses_fake")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.SetModel(ctx, "anthropic/claude-haiku-4-5"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if !fs.setModel {
		t.Fatalf("server did not see SetModel")
	}
	// simulate the engine reflecting the new model in GET /session/{id}
	fs.lastState.Model = "anthropic/claude-haiku-4-5"
	state, err := c.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Model != "anthropic/claude-haiku-4-5" {
		t.Fatalf("state.Model = %q, want %q", state.Model, "anthropic/claude-haiku-4-5")
	}
}

// TestUnsupportedNoBaseURL asserts the empty-baseURL path returns
// ErrUnsupported for every per-session method (so tests can wire a Control
// without an HTTP server and still get clean "unsupported" errors).
func TestUnsupportedNoBaseURL(t *testing.T) {
	c := opencode.NewControl("")
	ctx := context.Background()
	if _, err := c.Models(ctx); err != engine.ErrUnsupported {
		t.Fatalf("Models: want ErrUnsupported, got %v", err)
	}
	if _, err := c.Agents(ctx); err != engine.ErrUnsupported {
		t.Fatalf("Agents: want ErrUnsupported, got %v", err)
	}
	if err := c.SetModel(ctx, "x"); err != engine.ErrUnsupported {
		t.Fatalf("SetModel: want ErrUnsupported, got %v", err)
	}
}

// TestCreateSession validates the pre-bind helper used by AllocateAndPrepare
// (in production this fires after WaitReady; the test exercises the POST
// shape directly).
func TestCreateSession(t *testing.T) {
	srv, _ := newFake(t)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := opencode.CreateSession(ctx, srv.URL, "/tmp/demo", "title")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.ID != "ses_fake" {
		t.Fatalf("session id = %q, want ses_fake", sess.ID)
	}
}

// The response opencode gives us carries a live provider credential on every
// entry (request.body.apiKey, 66 of 66 on the probe). Nothing that reaches a
// caller may contain it, and this test fails on the whole marshalled payload
// rather than on a field list, so widening the projection is caught even if
// the new field is named something else.
func TestModelsProjectionCannotCarryAProviderCredential(t *testing.T) {
	srv, fs := newFake(t)
	defer srv.Close()
	fs.models = []engine.Model{
		{ID: "anthropic/claude-sonnet-4-5", DisplayName: "Sonnet 4.5", Provider: "anthropic"},
		{ID: "openai/gpt-5", DisplayName: "GPT-5", Provider: "openai"},
	}
	c := opencode.NewControl(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	models, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
	raw, err := json.Marshal(models)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"apiKey", "sk-live-body-secret", "sk-live-header-secret", "request", "headers"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("model listing carries %q upstream: %s", forbidden, raw)
		}
	}
	// and the fields the caller actually needs did survive
	if models[0].ID == "" || models[0].Provider == "" || models[0].DisplayName == "" || models[0].Status == "" {
		t.Errorf("projection dropped something a caller needs: %+v", models[0])
	}
}

// All three of these answer with the web UI at HTTP 200 on the legacy path, so
// a wrong path is not an error anywhere: it is a success that does nothing.
func TestControlUsesTheApiPrefixedRoutes(t *testing.T) {
	srv, fs := newFake(t)
	defer srv.Close()
	c := opencode.NewControl(srv.URL)
	c.BindSession("ses_fake")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.SetModel(ctx, "anthropic/claude-sonnet-4-5"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if !fs.setModel {
		t.Error("SetModel did not reach POST /api/session/{id}/model")
	}
	if err := c.SetAgent(ctx, "build"); err != nil {
		t.Fatalf("SetAgent: %v", err)
	}
	if !fs.setAgent {
		t.Error("SetAgent did not reach POST /api/session/{id}/agent")
	}
}

// The message body puts parts at the TOP level; the spec rejects unknown
// properties, so the old {message:{role,parts}} envelope was refused.
func TestPromptSendsPartsAtTheTopLevel(t *testing.T) {
	srv, fs := newFake(t)
	defer srv.Close()
	c := opencode.NewControl(srv.URL)
	c.BindSession("ses_fake")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Prompt(ctx, "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if fs.promptBody == nil {
		t.Fatal("Prompt never reached the message route")
	}
	if _, wrapped := fs.promptBody["message"]; wrapped {
		t.Errorf("parts must be top level, got an envelope: %v", fs.promptBody)
	}
	parts, ok := fs.promptBody["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("want one top-level part, got %v", fs.promptBody)
	}
}

// A serve that answers /api/health is not a serve that knows its models.
//
// This is the bug the engineer hit as "I see no single models at all": the
// prober waited on health, asked once, got an empty list with NO error, and
// every layer above reported that faithfully. Measured against opencode
// 1.18.29, /api/model returns 0 entries when health first goes green and 66
// about three seconds later.
func TestWaitModelsKeepsAskingWhileTheCatalogueIsStillFilling(t *testing.T) {
	var asks int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/model" {
			http.NotFound(w, r)
			return
		}
		asks++
		w.Header().Set("Content-Type", "application/json")
		if asks < 3 {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"anthropic/claude-opus-5","name":"Opus 5","providerID":"anthropic"}]}`))
	}))
	defer srv.Close()

	got, err := opencode.NewControl(srv.URL).WaitModels(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("WaitModels: %v", err)
	}
	if len(got) != 1 || got[0].ID != "anthropic/claude-opus-5" {
		t.Fatalf("must return the catalogue once it fills, got %+v", got)
	}
	if asks < 3 {
		t.Fatalf("must keep asking past the empty answers, asked %d times", asks)
	}
}

// The deadline path is the machine with no authenticated provider. Its answer
// is genuinely empty and the picker's "OpenCode named nothing" message is
// right for it, so this must return that empty list rather than hang or error.
func TestWaitModelsReturnsAnHonestEmptyAtTheDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	start := time.Now()
	got, err := opencode.NewControl(srv.URL).WaitModels(context.Background(), 600*time.Millisecond)
	if err != nil {
		t.Fatalf("an empty catalogue is an answer, not an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected the honest empty, got %+v", got)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("returned before the budget elapsed (%s): it cannot have waited", elapsed)
	}
}

// A transport failure is not an empty catalogue and must not be smoothed into
// one by the retry loop.
func TestWaitModelsReturnsTheErrorRatherThanRetryingPastIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := opencode.NewControl(srv.URL).WaitModels(context.Background(), 2*time.Second); err == nil {
		t.Fatal("a 500 from /api/model must surface, not read as no models")
	}
}

// The optional interface is what lets the session-scoped model picker use the
// same wait without every engine having to implement one. If the assertion
// silently stops matching, the control sheet quietly goes back to asking once.
func TestOpenCodeControlSatisfiesModelWaiter(t *testing.T) {
	var _ engine.ModelWaiter = opencode.NewControl("http://127.0.0.1:1")
}
