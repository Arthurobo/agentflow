// model_ref_test.go — the model wire shape, which was wrong twice over and
// where the second error was hidden by the first.
//
// Verified against a live `opencode serve` and its own /doc: the body is
// {"model": {"providerID", "id"}}. Sending {"modelID": ...} answers
// 400 Missing key at ["model"]; correcting only the outer key answers
// 400 Missing key at ["model"]["id"]. Every apply failed as a 502, and
// Session.model is that same object, so the active model never read back
// either.
package opencode_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/engine/opencode"
)

// modelServer answers the two routes this is about, in the real shapes.
func modelServer(t *testing.T, state map[string]any) (*httptest.Server, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/model"):
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &got); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			// The real server validates the shape and this must too, or the
			// test would pass on a body opencode rejects.
			m, ok := got["model"].(map[string]any)
			if !ok {
				http.Error(w, `{"message":"Missing key at [\"model\"]"}`, http.StatusBadRequest)
				return
			}
			if id, _ := m["id"].(string); id == "" {
				http.Error(w, `{"message":"Missing key at [\"model\"][\"id\"]"}`, http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/session/"):
			_ = json.NewEncoder(w).Encode(state)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestSetModelSendsTheProviderAndIdAsSeparateFields(t *testing.T) {
	srv, got := modelServer(t, nil)
	c := opencode.NewControl(srv.URL)
	c.BindSession("ses_1")

	if err := c.SetModel(context.Background(), "anthropic/claude-sonnet-4-5"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	model, ok := (*got)["model"].(map[string]any)
	if !ok {
		t.Fatalf("the body must nest the ref under model, got %v", *got)
	}
	if model["providerID"] != "anthropic" {
		t.Fatalf("providerID: %v", model["providerID"])
	}
	if model["id"] != "claude-sonnet-4-5" {
		t.Fatalf("id: %v", model["id"])
	}
	// The whole body, so nothing extra rides along: the server rejects
	// unknown properties.
	if len(*got) != 1 || len(model) != 2 {
		t.Fatalf("the body must be exactly {model:{providerID,id}}, got %v", *got)
	}
}

// Only the FIRST slash is structural. An openrouter route carries its own.
func TestSetModelSplitsOnlyTheFirstSlash(t *testing.T) {
	srv, got := modelServer(t, nil)
	c := opencode.NewControl(srv.URL)
	c.BindSession("ses_1")

	if err := c.SetModel(context.Background(), "openrouter/anthropic/claude-3"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	model := (*got)["model"].(map[string]any)
	if model["providerID"] != "openrouter" || model["id"] != "anthropic/claude-3" {
		t.Fatalf("a routed model must keep its own slashes: %v", model)
	}
}

// A bare id is refused before the call, because opencode does not error on
// one: it resolves to nothing and the session silently keeps its default.
func TestSetModelRefusesABareIdWithoutCallingTheServer(t *testing.T) {
	srv, got := modelServer(t, nil)
	c := opencode.NewControl(srv.URL)
	c.BindSession("ses_1")

	if err := c.SetModel(context.Background(), "minimax"); err == nil {
		t.Fatal("a bare id must be refused")
	}
	if *got != nil {
		t.Fatalf("nothing may have been sent, got %v", *got)
	}
}

// Session.model is the SAME object, so reading it as a string returned empty
// on every read and the picker could never show what was selected.
func TestStateDecodesTheModelObject(t *testing.T) {
	srv, _ := modelServer(t, map[string]any{
		"id":    "ses_1",
		"model": map[string]any{"providerID": "anthropic", "id": "claude-sonnet-4-5"},
		"agent": "build",
	})
	c := opencode.NewControl(srv.URL)
	c.BindSession("ses_1")

	st, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Model != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("the model must read back in provider/model form, got %q", st.Model)
	}
	if st.Provider != "anthropic" {
		t.Fatalf("the provider comes from the same object, got %q", st.Provider)
	}
	if st.Agent != "build" {
		t.Fatalf("agent is a string and still reads: %q", st.Agent)
	}
}

// An older or different server that really does send a string must still
// parse: the decode is a widening, not a swap.
func TestStateStillAcceptsAPlainStringModel(t *testing.T) {
	srv, _ := modelServer(t, map[string]any{"id": "ses_1", "model": "anthropic/claude-sonnet-4-5"})
	c := opencode.NewControl(srv.URL)
	c.BindSession("ses_1")

	st, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Model != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("a string model must still read, got %q", st.Model)
	}
}
