package agentapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/agentapi"
	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/engine/claude"
	"github.com/arthurobo/agentflow/internal/engine/opencode"
	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// fakeOpenCodeServer stands in for the OpenCode TUI's HTTP API: answers
// /api/health, captures POST /session, and answers a single model + agent
// list. Used to verify the agentapi control surface answers truthfully for
// an OpenCode engine row.
func fakeOpenCodeServer(t *testing.T) (*httptest.Server, *opencodeSession) {
	t.Helper()
	sess := &opencodeSession{models: map[string]bool{"anthropic/claude-sonnet-4-5": true}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"healthy":true}`)
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			sess.id = "ses_fake"
			_, _ = io.WriteString(w, `{"id":"ses_fake","directory":"x"}`)
			return
		}
	})
	// The verified routes: /api/model, and a credential on every entry.
	mux.HandleFunc("/api/model", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"location": map[string]any{"directory": "/tmp"},
			"data": []map[string]any{{
				"id": "anthropic/claude-sonnet-4-5", "name": "Sonnet 4.5",
				"providerID": "anthropic", "status": "active",
				"request": map[string]any{"body": map[string]any{"apiKey": "sk-live-secret"}},
			}},
		})
	})
	mux.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"name":"build","description":"Build things","model":"anthropic/claude-sonnet-4-5","default":true}]`)
	})
	mux.HandleFunc("/api/session/ses_fake/model", func(w http.ResponseWriter, r *http.Request) {
		sess.setModel = true
		sess.lastModel = "anthropic/claude-sonnet-4-5"
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/session/ses_fake/agent", func(w http.ResponseWriter, r *http.Request) {
		sess.setAgent = true
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/session/ses_fake", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"ses_fake","model":%q,"agent":"build"}`, sess.lastModel))
	})
	return httptest.NewServer(mux), sess
}

type opencodeSession struct {
	id        string
	models    map[string]bool
	setModel  bool
	setAgent  bool
	lastModel string
}

// TestControlEngineSurfacingForOpenCode is the contract test: a managed run
// whose engine is "opencode" must expose the engine's capabilities, the
// model list, and round-trip a SetModel through the live control API. The
// fake server stands in for the TUI's API; the spawner is not exercised
// (the goal is the control surface).
func TestControlEngineSurfacingForOpenCode(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv, fakeSess := fakeOpenCodeServer(t)
	t.Cleanup(srv.Close)

	ingSvc := ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"})
	sp := spawner.New(st, ingSvc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Register the opencode engine pointed at the fake server.
	registry := engine.NewRegistry()
	registry.Register(opencode.New(opencode.Config{Hostname: "127.0.0.1", PortAllocator: func() (int, error) { return portFromURL(t, srv.URL), nil }}))

	srvAgentAPI := agentapi.New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	srvAgentAPI.Engines = registry

	// Seed a device so requireAuth passes (the control endpoints are
	// device-authed; tests use a deterministic token).
	const testToken = "test-token"
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "test-device", Name: "test", MachineID: "test",
		Kind: "device", TokenHash: store.HashToken(testToken),
	}, 0); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	// seed a managed_sessions row that looks like an OpenCode run
	now := time.Now().UnixMilli()
	if err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: "run_opencode_1", SessionID: "ses_fake", Kind: "tty",
		Engine: "opencode", ControlPort: portFromURL(t, srv.URL),
		ControlBase: srv.URL,
		State:       "running", StartedAt: now, UpdatedAt: now,
		CWD: "/tmp", Project: "demo", Model: "anthropic/claude-sonnet-4-5",
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	// 1. /control/caps reflects opencode's full capability mask
	caps := doJSON(t, srvAgentAPI, "GET", "/api/v1/agentd/sessions/run_opencode_1/control/caps", nil)
	if caps["engine"] != "opencode" {
		t.Fatalf("caps.engine = %v, want opencode", caps["engine"])
	}
	capMap, ok := caps["caps"].(map[string]any)
	if !ok {
		t.Fatalf("caps.caps is not a map: %T", caps["caps"])
	}
	if capMap["listModels"] != true || capMap["setModel"] != true {
		t.Fatalf("opencode caps should advertise listModels+setModel, got %+v", capMap)
	}

	// 2. /control/models serves the engine's catalog
	models := doJSON(t, srvAgentAPI, "GET", "/api/v1/agentd/sessions/run_opencode_1/control/models", nil)
	modelList, _ := models["models"].([]any)
	if len(modelList) != 1 {
		t.Fatalf("models list = %v, want 1 entry", modelList)
	}

	// 3. /control/model round-trips: POST sets, GET reflects
	set := doJSON(t, srvAgentAPI, "POST", "/api/v1/agentd/sessions/run_opencode_1/control/model",
		map[string]any{"model": "anthropic/claude-sonnet-4-5"})
	if set["applied"] != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("set.applied = %v", set["applied"])
	}
	if !fakeSess.setModel {
		t.Fatalf("server did not observe SetModel")
	}
	state := doJSON(t, srvAgentAPI, "GET", "/api/v1/agentd/sessions/run_opencode_1/control/state", nil)
	if stateMap, _ := state["state"].(map[string]any); stateMap == nil || stateMap["model"] != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("state did not reflect applied model: %+v", state)
	}

	// 4. /control/agents lists the engine's agents
	agents := doJSON(t, srvAgentAPI, "GET", "/api/v1/agentd/sessions/run_opencode_1/control/agents", nil)
	agentList, _ := agents["agents"].([]any)
	if len(agentList) != 1 {
		t.Fatalf("agents list = %v, want 1 entry", agentList)
	}
}

// TestControlEngineSurfacingForClaude verifies the claude control path:
// the surface answers, but caps.listModels is false and the model endpoint
// returns the empty list with an explanatory note.
func TestControlEngineSurfacingForClaude(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "claude-control.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ingSvc := ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"})
	sp := spawner.New(st, ingSvc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry := engine.NewRegistry()
	registry.Register(engineForTest(t, "claude"))

	srvAgentAPI := agentapi.New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	srvAgentAPI.Engines = registry

	// Seed a device for auth.
	const testToken = "test-token"
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "test-device-claude", Name: "test", MachineID: "test",
		Kind: "device", TokenHash: store.HashToken(testToken),
	}, 0); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	now := time.Now().UnixMilli()
	if err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: "run_claude_1", SessionID: "ses_claude", Kind: "tty",
		Engine: "claude",
		State:  "running", StartedAt: now, UpdatedAt: now,
		CWD: "/tmp", Project: "demo", Model: "claude-sonnet-4-5",
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	// Claude's control is PTY-backed now: the injector the old comment
	// pointed at exists, so the sheet's rows are served by typing rather
	// than greyed out. Listing was always real — the aliases are the CLI's
	// own and do not vary by machine.
	caps := doJSON(t, srvAgentAPI, "GET", "/api/v1/agentd/sessions/run_claude_1/control/caps", nil)
	capMap, _ := caps["caps"].(map[string]any)
	for _, want := range []string{"listModels", "setModel", "listCommands", "runCommand", "listSkills"} {
		if capMap[want] != true {
			t.Fatalf("injection serves it; caps.%s = %v", want, capMap[want])
		}
	}
	// The agent row stays off: there is no slash command that switches a
	// running session's agent, so offering the switcher would be the same
	// lie the model row used to tell.
	if capMap["setAgent"] != false || capMap["listAgents"] != false {
		t.Fatalf("claude cannot switch agents live; caps = %v", capMap)
	}
	models := doJSON(t, srvAgentAPI, "GET", "/api/v1/agentd/sessions/run_claude_1/control/models", nil)
	list, _ := models["models"].([]any)
	if len(list) == 0 {
		t.Fatalf("the picker must never be empty for claude: %v", models)
	}
	names := map[string]bool{}
	for _, raw := range list {
		if m, ok := raw.(map[string]any); ok {
			names[m["id"].(string)] = true
		}
	}
	for _, want := range []string{"opus", "sonnet", "haiku", "fable"} {
		if !names[want] {
			t.Fatalf("the alias set must carry %q, got %v", want, names)
		}
	}

	// The command catalog is the CLI's builtins plus whatever this machine
	// defines; the composer's slash autocomplete reads exactly this.
	cmds := doJSON(t, srvAgentAPI, "GET", "/api/v1/agentd/sessions/run_claude_1/control/commands", nil)
	cmdList, _ := cmds["commands"].([]any)
	if len(cmdList) == 0 {
		t.Fatalf("the claude catalog must never be empty: %v", cmds)
	}
	found := map[string]bool{}
	for _, raw := range cmdList {
		if m, ok := raw.(map[string]any); ok {
			found[m["name"].(string)] = true
		}
	}
	if !found["model"] || !found["context"] {
		t.Fatalf("the builtins must be in the catalog, got %v", found)
	}

	// This row has no live process, so nothing is bound and the write is
	// refused rather than reported as applied. A control that claimed
	// success with no terminal to type into would be claiming what it
	// cannot know — the same dishonesty the old caps avoided by lying the
	// other way.
	rec := doRaw(t, srvAgentAPI, "POST", "/api/v1/agentd/sessions/run_claude_1/control/model",
		map[string]any{"model": "opus"})
	if rec.Code < 400 {
		t.Fatalf("a model change with no live PTY must be refused, got %d %s", rec.Code, rec.Body.String())
	}
}

// doJSON issues an authenticated request through the agentapi handler (no
// real network) and decodes the JSON response. Token is irrelevant — the
// test wires the registry directly so the auth path is satisfied via
// the in-process transport.
func doJSON(t *testing.T, s *agentapi.Server, method, path string, body any) map[string]any {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// mint a fake device so requireAuth succeeds; we only care about the
	// control surface, so a missing-device path is fine if the registry is
	// wired (requireAuth looks up the token hash in the devices table).
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code >= 400 {
		t.Fatalf("%s %s: %d %s", method, path, rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return out
}

// portFromURL extracts the port from a httptest URL. Tests use it to seed
// managed_sessions.control_port.
func portFromURL(t *testing.T, raw string) int {
	t.Helper()
	u := strings.TrimPrefix(raw, "http://")
	i := strings.Index(u, ":")
	if i < 0 {
		t.Fatalf("no port in %s", raw)
	}
	j := strings.Index(u[i:], "/")
	var p string
	if j < 0 {
		p = u[i+1:]
	} else {
		p = u[i+1 : i+j]
	}
	var port int
	if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
		t.Fatalf("parse port %q: %v", p, err)
	}
	return port
}

// engineForTest returns a no-path claude engine for the test registry
// (LookupBinary isn't exercised by the control endpoints).
func engineForTest(t *testing.T, _ string) engine.Engine {
	t.Helper()
	return claude.New("")
}

// Sanity: make sure portFromURL works on a real listener (defends against
// httptest URL format drift).
func TestPortFromURL(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	got := portFromURL(t, url)
	if got != port {
		t.Fatalf("portFromURL = %d, want %d", got, port)
	}
}

// doRaw is doJSON without the decode, for the responses that are refusals.
func doRaw(t *testing.T, s *agentapi.Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// Applying a model must not wipe the kill identity of a live process.
//
// persistSessionModel used to re-upsert the whole row from an in-memory
// Session, which carries no Pgid, ProcStartTicks, Generation or StopReason.
// Every successful model change therefore zeroed the four fields a later stop
// uses to prove it is killing the right process, on a run that was still
// running at the time.
func TestPersistingAModelLeavesTheKillIdentityIntact(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now().UnixMilli()

	if err := st.UpsertManagedSession(ctx, &store.ManagedSession{
		ID: "run_ident", SessionID: "ses_ident", Kind: "tty", Engine: "claude",
		State: "running", StartedAt: now, UpdatedAt: now, CWD: "/tmp",
		Model: "sonnet",
		Pgid:  4242, ProcStartTicks: 999888, Generation: "gen-7", StopReason: "",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := st.SetManagedSessionModel(ctx, "run_ident", "opus"); err != nil {
		t.Fatalf("set model: %v", err)
	}

	got, err := st.GetManagedSession(ctx, "run_ident")
	if err != nil || got == nil {
		t.Fatalf("reread: %v", err)
	}
	if got.Model != "opus" {
		t.Fatalf("the model must actually change, got %q", got.Model)
	}
	if got.Pgid != 4242 || got.ProcStartTicks != 999888 || got.Generation != "gen-7" {
		t.Fatalf("the kill identity was wiped: pgid=%d ticks=%d gen=%q",
			got.Pgid, got.ProcStartTicks, got.Generation)
	}
	if got.State != "running" || got.SessionID != "ses_ident" {
		t.Fatalf("nothing else may move: %+v", got)
	}
}
