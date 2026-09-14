// loopapi_test.go — the engineer surface: a different token class from the
// agents', creation that hands out the only readable copy of each token, and
// the engineer talking to the loop as a participant rather than a side
// channel.
package loopapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/mailapi"
	"github.com/arthurobo/agentflow/internal/store"
)

type harness struct {
	t      *testing.T
	st     *store.Store
	srv    *Server
	agents *mailapi.Server
	http   *httptest.Server
	device string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "loops.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	device := "device-token-for-the-engineer"
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "dev1", Name: "laptop", Kind: "device", TokenHash: store.HashToken(device),
	}, 0); err != nil {
		t.Fatalf("device: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	agents := mailapi.New(st, log, mailapi.Config{})
	srv := New(st, agents, log, Config{BaseURL: "http://127.0.0.1:4344"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{t: t, st: st, srv: srv, agents: agents, http: ts, device: device}
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
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

// create names the loop after its own instruction and sends the instruction
// whole. The instruction has to clear the floor, so callers pass a real one.
func (h *harness) create(task string) (string, map[string]map[string]any) {
	h.t.Helper()
	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "harness loop", "task": task, "cwd": "/tmp/target", "play": "audit",
	})
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	loop := body["loop"].(map[string]any)
	handoffs := map[string]map[string]any{}
	for _, raw := range body["handoffs"].([]any) {
		h := raw.(map[string]any)
		handoffs[h["role"].(string)] = h
	}
	return loop["id"].(string), handoffs
}

// The engineer surface runs on device tokens. A member token is not one.
func TestEngineerSurfaceIsADifferentTokenClass(t *testing.T) {
	h := newHarness(t)
	loopID, handoffs := h.create("Fix the flaky guest export")

	memberToken := handoffs["ORCHESTRATOR"]["token"].(string)
	if resp, _ := h.do(memberToken, "GET", "/"+loopID, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a member token must not reach the engineer surface, got %d", resp.StatusCode)
	}
	for _, bad := range []string{"", "nonsense"} {
		if resp, _ := h.do(bad, "GET", "/"+loopID, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q must be refused, got %d", bad, resp.StatusCode)
		}
	}
	if resp, _ := h.do(h.device, "GET", "/"+loopID, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the device token must work, got %d", resp.StatusCode)
	}
}

// Creation is the only moment a token is readable, and it hands out a rendered
// prompt with it.
func TestCreateHandsOutTokensAndPromptsOnce(t *testing.T) {
	h := newHarness(t)
	loopID, handoffs := h.create("Fix the flaky guest export")

	for _, role := range []string{"ORCHESTRATOR", "INVESTIGATION", "REVIEW"} {
		hd, ok := handoffs[role]
		if !ok {
			t.Fatalf("no handoff for %s", role)
		}
		token, _ := hd["token"].(string)
		if token == "" {
			t.Fatalf("%s got no token", role)
		}
		prompt, _ := hd["prompt"].(string)
		if !strings.Contains(prompt, "You are "+role) || !strings.Contains(prompt, token) {
			t.Fatalf("%s prompt is not usable: %.120q", role, prompt)
		}
		// Not "WAIT": there is no such command any more. Mail is delivered,
		// so what an idle member is told is where to stop, not what to run.
		if !strings.Contains(prompt, "/api/v1/agent/inbox") ||
			!strings.Contains(prompt, "WHEN YOU HAVE NOTHING TO DO") {
			t.Fatalf("%s prompt carries no commands", role)
		}
		// The token works on the agent surface it names.
		m, _, err := h.st.LoopMemberByToken(context.Background(), token)
		if err != nil || m == nil || m.Role != role {
			t.Fatalf("%s token does not authenticate: %v %+v", role, err, m)
		}
	}
	// The engineer is a participant with no token and no prompt.
	engineer := handoffs["ENGINEER"]
	if engineer == nil {
		t.Fatal("the engineer must be on the roster")
	}
	if tok, _ := engineer["token"].(string); tok != "" {
		t.Fatal("the engineer must hold no token")
	}

	// The play is running and the entry brief is already queued.
	_, detail := h.do(h.device, "GET", "/"+loopID, nil)
	loop := detail["loop"].(map[string]any)
	if loop["play"] != "audit" || loop["stepId"] != "brief" {
		t.Fatalf("the loop must be standing on the play's entry step: %v", loop)
	}
	if detail["stepBrief"] == "" {
		t.Fatal("the detail view must show the current step's brief")
	}
	crew := detail["crew"].([]any)
	if len(crew) != 4 {
		t.Fatalf("crew has %d members, want the play's roles plus the engineer", len(crew))
	}
	for _, raw := range crew {
		m := raw.(map[string]any)
		if _, ok := m["charsIn"]; !ok {
			t.Fatalf("the roster must show charsIn: %v", m)
		}
		if _, ok := m["notes"]; !ok {
			t.Fatalf("the roster must show note counts: %v", m)
		}
	}
}

// A bad tool or model is refused at creation, with the reason.
func TestCreateRefusesAnUnknownToolOrModel(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "bad model", "task": "refuse a model this tool does not run", "play": "recon",
		"crew": []map[string]any{
			{"role": "ORCHESTRATOR", "tool": "claude", "model": "opus"},
			{"role": "INVESTIGATION", "tool": "claude", "model": "gpt-4"},
			{"role": "ENGINEER"},
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad model must be refused, got %d %v", resp.StatusCode, body)
	}
	if body["error"].(map[string]any)["code"] != "unknown_tool" {
		t.Fatalf("code: %v", body["error"])
	}
	if resp, body := h.do(h.device, "POST", "", map[string]any{"task": "refuse a play that does not exist", "play": "nonsense"}); resp.StatusCode != http.StatusBadRequest ||
		body["error"].(map[string]any)["code"] != "invalid_play" {
		t.Fatalf("an unknown play must be refused: %d %v", resp.StatusCode, body)
	}
	if resp, _ := h.do(h.device, "POST", "", map[string]any{"play": "recon"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a loop with no task must be refused, got %d", resp.StatusCode)
	}
	// The registry is readable, so the wizard renders what is actually known.
	_, tools := h.do(h.device, "GET", "/tools", nil)
	list := tools["tools"].([]any)
	if len(list) != 2 {
		t.Fatalf("the registry must be published: %v", tools)
	}
}

// Ending parks the agents; continuing adopts them with their tokens, and the
// tokens minted for the roles that were adopted are never handed out.
func TestEndParksAndContinueAdopts(t *testing.T) {
	h := newHarness(t)
	loopID, handoffs := h.create("run round one against the export path")
	invToken := handoffs["INVESTIGATION"]["token"].(string)

	resp, ended := h.do(h.device, "POST", "/"+loopID+"/end", map[string]any{"reason": "round one done"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("end: %d %v", resp.StatusCode, ended)
	}
	if !strings.Contains(ended["agents"].(string), "parked") {
		t.Fatalf("ending must park, not dismiss: %v", ended)
	}
	member, _, _ := h.st.LoopMemberByToken(context.Background(), invToken)
	if member == nil || member.Status != store.LoopMemberIdle {
		t.Fatalf("the member must be parked idle with a live token: %+v", member)
	}

	resp, next := h.do(h.device, "POST", "/"+loopID+"/continue", map[string]any{
		"task": "round two, with the reviewer kept on", "carryNotes": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("continue: %d %v", resp.StatusCode, next)
	}
	kept := next["keptAgents"].([]any)
	if len(kept) != 4 {
		t.Fatalf("every live member must carry over, got %v", kept)
	}
	successor := next["loop"].(map[string]any)
	if successor["parentLoopId"] != loopID || successor["task"] != "round two, with the reviewer kept on" {
		t.Fatalf("successor: %v", successor)
	}
	// The original token still works and now points at the successor.
	member, loop, _ := h.st.LoopMemberByToken(context.Background(), invToken)
	if member == nil || loop.ID != successor["id"] {
		t.Fatalf("the adopted member must be in the successor: %+v", member)
	}
	// No superseded token is handed out for a role that was adopted.
	for _, raw := range next["handoffs"].([]any) {
		hd := raw.(map[string]any)
		if tok, _ := hd["token"].(string); tok != "" {
			t.Fatalf("%v was adopted and must not be issued a second token", hd["role"])
		}
	}
}

func TestRetireAndRotateOneRole(t *testing.T) {
	h := newHarness(t)
	loopID, handoffs := h.create("staff this loop and rotate one member")
	reviewToken := handoffs["REVIEW"]["token"].(string)

	resp, _ := h.do(h.device, "POST", "/"+loopID+"/members/REVIEW/retire", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retire: %d", resp.StatusCode)
	}
	member, loop, _ := h.st.LoopMemberByToken(context.Background(), reviewToken)
	if stop, _, _ := store.LoopMemberSignal(member, loop); !stop {
		t.Fatal("a retired role must be told to stop")
	}

	resp, rotated := h.do(h.device, "POST", "/"+loopID+"/members/REVIEW/rotate", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: %d %v", resp.StatusCode, rotated)
	}
	fresh := rotated["handoff"].(map[string]any)
	if fresh["token"] == reviewToken || fresh["token"] == "" {
		t.Fatalf("rotation must issue a new token: %v", fresh)
	}
	if !strings.Contains(fresh["prompt"].(string), "You are REVIEW") {
		t.Fatal("rotation must hand back a fresh prompt")
	}
	if dead, _, _ := h.st.LoopMemberByToken(context.Background(), reviewToken); dead != nil {
		t.Fatal("the old token must stop working")
	}
	if resp, _ := h.do(h.device, "POST", "/"+loopID+"/members/NOBODY/rotate", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rotating an unknown role must 404, got %d", resp.StatusCode)
	}
	// Staffing a new role mid-loop hands back a token and a prompt.
	resp, added := h.do(h.device, "POST", "/"+loopID+"/members", map[string]any{
		"role": "INVESTIGATION#2", "tool": "claude", "model": "sonnet",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add member: %d %v", resp.StatusCode, added)
	}
	if added["handoff"].(map[string]any)["model"] != "sonnet" {
		t.Fatalf("the model must round-trip: %v", added["handoff"])
	}
}

// The engineer typing is a row in the inbox the member is already polling, and
// the orchestrator sees it.
func TestEngineerSaysAndTheOrchestratorSeesIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	loopID, handoffs := h.create("talk to the crew and report what they say")

	resp, said := h.do(h.device, "POST", "/"+loopID+"/say", map[string]any{
		"to": "INVESTIGATION", "body": "check the export first",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("say: %d %v", resp.StatusCode, said)
	}
	if said["mirroredToOrchestrator"] != true {
		t.Fatalf("a message to a worker must be mirrored: %v", said)
	}

	invToken := handoffs["INVESTIGATION"]["token"].(string)
	member, _, _ := h.st.LoopMemberByToken(ctx, invToken)
	claimed, err := h.st.ClaimInbox(ctx, member, store.ClaimOptions{Limit: 20})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	found := false
	for _, m := range claimed {
		if m.SenderRole == store.RoleEngineer && m.Body == "check the export first" {
			found = true
		}
	}
	if !found {
		t.Fatal("the engineer's message must land in the inbox the member is already polling")
	}

	// The orchestrator SEES it without being woken by it. A mirror is for
	// sight: waking the loop's most context-expensive member to read a copy
	// it need not act on is the cost this design exists to avoid.
	orchToken := handoffs["ORCHESTRATOR"]["token"].(string)
	orch, _, _ := h.st.LoopMemberByToken(ctx, orchToken)
	woke, err := h.st.ClaimInbox(ctx, orch, store.ClaimOptions{Limit: 20})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, m := range woke {
		if m.MirrorOf != "" {
			t.Fatalf("a mirror must never wake the orchestrator: %s", m.ID)
		}
	}
	thread, err := h.st.MailThread(ctx, orch.ID, 0)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	mirrored := false
	for _, m := range thread {
		if m.MirrorOf != "" && m.Body == "check the export first" {
			mirrored = true
		}
	}
	if !mirrored {
		t.Fatal("the orchestrator must see any instruction the engineer sends a worker")
	}

	// Saying it to the orchestrator directly needs no mirror.
	_, direct := h.do(h.device, "POST", "/"+loopID+"/say", map[string]any{"body": "status?"})
	if direct["mirroredToOrchestrator"] != false {
		t.Fatalf("a message to the orchestrator must not be mirrored to itself: %v", direct)
	}

	_, transcript := h.do(h.device, "GET", "/"+loopID+"/messages", nil)
	if len(transcript["messages"].([]any)) == 0 {
		t.Fatal("the transcript must be readable")
	}
	_, notes := h.do(h.device, "GET", "/"+loopID+"/notes", nil)
	if _, ok := notes["notes"]; !ok {
		t.Fatal("the notes must be readable")
	}
	_, list := h.do(h.device, "GET", "", nil)
	if len(list["loops"].([]any)) != 1 {
		t.Fatalf("list: %v", list)
	}
}

// The two surfaces sit on one listener, and /api/v1/agentd must not be
// swallowed by /api/v1/agent. This pins the routing rule agentd's mount
// depends on.
func TestTheSurfacesDoNotCollideOnOnePort(t *testing.T) {
	h := newHarness(t)
	control := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	// The same Mount agentd uses, so this covers the real wiring rather
	// than a copy of it.
	ts := httptest.NewServer(Mount(control, h.agents, h.srv, MountOptions{}))
	defer ts.Close()

	get := func(path, token, header string) int {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if token != "" {
			req.Header.Set(header, token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	// The existing control API keeps every one of its paths.
	if code := get("/api/v1/agentd/loops", "", ""); code != http.StatusTeapot {
		t.Fatalf("/api/v1/agentd/loops must still reach the control API, got %d", code)
	}
	if code := get("/health", "", ""); code != http.StatusTeapot {
		t.Fatalf("/health must still reach the control API, got %d", code)
	}
	// The member surface answers on its own prefix, with its own token class.
	if code := get("/api/v1/agent/whoami", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("/api/v1/agent/whoami must reach the member surface, got %d", code)
	}
	// The engineer surface answers on its own, and rejects a device-less call.
	if code := get(Prefix, "", ""); code != http.StatusUnauthorized {
		t.Fatalf("%s must reach the engineer surface, got %d", Prefix, code)
	}
	if code := get(Prefix, h.device, "Authorization-Not-Used"); code != http.StatusUnauthorized {
		t.Fatalf("the engineer surface must want a bearer token, got %d", code)
	}
}

// The neutral tools route exists so a caller that is not a loop can read the
// registry. It must be the SAME registry, behind the SAME device auth, and it
// must not cost the loops route its own copy.
func TestToolsIsReachableFromANeutralRouteAndFromLoops(t *testing.T) {
	h := newHarness(t)
	control := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	ts := httptest.NewServer(Mount(control, h.agents, h.srv, MountOptions{}))
	defer ts.Close()

	body := func(path, token string) (int, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	neutralCode, neutral := body(ToolsPrefix, h.device)
	loopsCode, viaLoops := body(Prefix+"/tools", h.device)
	if neutralCode != http.StatusOK {
		t.Fatalf("%s: want 200, got %d", ToolsPrefix, neutralCode)
	}
	if loopsCode != http.StatusOK {
		t.Fatalf("%s/tools: want 200, got %d", Prefix, loopsCode)
	}
	if neutral != viaLoops {
		t.Errorf("the two routes must serve one registry:\n neutral: %s\n loops:   %s", neutral, viaLoops)
	}
	if !strings.Contains(neutral, `"claude"`) {
		t.Errorf("registry did not come through: %s", neutral)
	}
	// A neutral name is not a neutral credential.
	if code, _ := body(ToolsPrefix, ""); code != http.StatusUnauthorized {
		t.Errorf("%s without a device token: want 401, got %d", ToolsPrefix, code)
	}
}

// The wizard asks what a tool can run. Claude's answer is fixed and comes from
// the registry; opencode's has to be asked, and when nobody can ask, the
// honest answer is an empty list that says so rather than a wrong list.
func TestToolModelsAnswersPerToolAndNeverInventsAList(t *testing.T) {
	h := newHarness(t)
	control := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	ts := httptest.NewServer(Mount(control, h.agents, h.srv, MountOptions{}))
	defer ts.Close()

	get := func(path string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+h.device)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	code, body := get(ToolsPrefix + "/claude/models")
	if code != http.StatusOK {
		t.Fatalf("claude models: %d", code)
	}
	models, _ := body["models"].([]any)
	if len(models) != 4 || body["probed"] != true {
		t.Fatalf("claude runs its four aliases from the registry: %v", body)
	}

	// No lister wired on this harness: opencode's list is unknown, and saying
	// so is the answer. An empty list presented as fact would be the lie.
	code, body = get(ToolsPrefix + "/opencode/models")
	if code != http.StatusOK {
		t.Fatalf("opencode models: %d", code)
	}
	models, _ = body["models"].([]any)
	if len(models) != 0 || body["probed"] != false {
		t.Fatalf("an unprobed tool must not report models: %v", body)
	}
	if note, _ := body["note"].(string); note == "" {
		t.Error("an empty list must say why it is empty")
	}

	if code, _ := get(ToolsPrefix + "/nosuchtool/models"); code != http.StatusNotFound {
		t.Errorf("unknown tool: want 404, got %d", code)
	}
	// and the route is still a device-token route
	req, _ := http.NewRequest("GET", ts.URL+ToolsPrefix+"/claude/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no-token request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a device token: want 401, got %d", resp.StatusCode)
	}
}

type fakeModels struct {
	mu    sync.Mutex
	out   []engine.Model
	err   error
	tool  string
	cwd   string
	calls int
}

func (f *fakeModels) Models(_ context.Context, tool, cwd string) ([]engine.Model, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tool, f.cwd = tool, cwd
	f.calls++
	return f.out, f.err
}

// The live path: what the tool itself reported, marked as live so the wizard
// can tell a real answer from the registry's silence.
func TestToolModelsServesTheLiveAnswerAndSaysItIsLive(t *testing.T) {
	h := newHarness(t)
	f := &fakeModels{out: []engine.Model{
		{ID: "opencode/omen-alpha", DisplayName: "Omen Alpha", Provider: "opencode", Status: "active"},
		{ID: "opencode-go/grok", DisplayName: "Grok", Provider: "opencode-go", Status: "active"},
	}}
	h.srv.SetModelLister(f)
	ts := httptest.NewServer(Mount(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), h.agents, h.srv, MountOptions{}))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+ToolsPrefix+"/opencode/models?cwd=/home/you/code", nil)
	req.Header.Set("Authorization", "Bearer "+h.device)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live models: %d %s", resp.StatusCode, raw)
	}
	models, _ := body["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("want the two the tool reported: %s", raw)
	}
	// A live answer must be distinguishable from the registry's "I don't know".
	if body["live"] != true || body["probed"] != true {
		t.Errorf("a live answer must say so, got live=%v probed=%v", body["live"], body["probed"])
	}
	// The cwd matters: the set is a property of the install AND the project.
	if f.tool != "opencode" || f.cwd != "/home/you/code" {
		t.Errorf("lister called with tool=%q cwd=%q", f.tool, f.cwd)
	}
	// And nothing the tool carries alongside a model may ride along.
	for _, forbidden := range []string{"apiKey", "request", "headers"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("the route leaked %q: %s", forbidden, raw)
		}
	}
}

// A probe that cannot answer is not an error to the caller: it is the state
// the registry already describes, and the wizard falls back to typing.
func TestToolModelsDegradesToTypingWhenTheProbeFails(t *testing.T) {
	h := newHarness(t)
	h.srv.SetModelLister(&fakeModels{err: errors.New("opencode: probe: control API never came up")})
	ts := httptest.NewServer(Mount(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), h.agents, h.srv, MountOptions{}))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+ToolsPrefix+"/opencode/models", nil)
	req.Header.Set("Authorization", "Bearer "+h.device)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 with an honest empty list, got %d", resp.StatusCode)
	}
	if models, _ := body["models"].([]any); len(models) != 0 {
		t.Error("a failed probe must not produce models")
	}
	if body["probed"] != false {
		t.Error("a failed probe is not a probe")
	}
	if note, _ := body["note"].(string); !strings.Contains(note, "never came up") {
		t.Errorf("the note must carry why, got %q", note)
	}
}

// A play's purpose is what it is FOR. The role chain says how it works, which
// is a different question, and a picker showing seven chains and no purpose
// asks the engineer to infer the point from the mechanism.
func TestPlaysCarryTheirPurposeOnTheWire(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(h.device, "GET", "/plays", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plays: %d", resp.StatusCode)
	}
	list, _ := body["plays"].([]any)
	if len(list) == 0 {
		t.Fatal("no plays")
	}
	for _, raw := range list {
		p, _ := raw.(map[string]any)
		name, _ := p["name"].(string)
		purpose, _ := p["purpose"].(string)
		if strings.TrimSpace(purpose) == "" {
			t.Errorf("play %q ships with no purpose, so the picker can only show its role chain", name)
		}
		// A purpose that is just the title back is not a purpose.
		if title, _ := p["title"].(string); strings.EqualFold(purpose, title) {
			t.Errorf("play %q restates its title instead of saying what it is for", name)
		}
	}
}

// The task the engineer types is the loop's instruction, so it must ARRIVE.
//
// Before this, the task was a label: a title on the board and a fragment of
// each session name. The orchestrator's rendered prompt never carried it and
// the play's entry brief never mentions it, so an orchestrator that was never
// spoken to had literally not been told what to do.
func TestTheTaskIsDeliveredToTheOrchestratorAsItsFirstMessage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, handoffs := h.create("delete the dead export path")

	orchToken := handoffs["ORCHESTRATOR"]["token"].(string)
	orch, _, _ := h.st.LoopMemberByToken(ctx, orchToken)
	claimed, err := h.st.ClaimInbox(ctx, orch, store.ClaimOptions{Limit: 20})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) == 0 {
		t.Fatal("the orchestrator's first poll must not be empty")
	}
	first := claimed[0]
	if first.Body != "delete the dead export path" {
		t.Fatalf("the engineer's own words must be the FIRST thing the orchestrator reads, got %q from %s",
			first.Body, first.SenderRole)
	}
	if first.SenderRole != store.RoleEngineer {
		t.Fatalf("the task is the engineer speaking, not the engine: sender %q", first.SenderRole)
	}

	// The play's entry brief still arrives, behind it. Two messages, because
	// procedure and instruction are different things and only one of them
	// changes per loop.
	brief := false
	for _, m := range claimed[1:] {
		if m.SenderRole == store.RoleEngine && strings.Contains(m.Body, "INVESTIGATION") {
			brief = true
		}
	}
	if !brief {
		t.Fatalf("the entry brief must still be delivered after the task, got %d messages", len(claimed))
	}
}

// The wizard sends no ENGINEER, and the task must still be delivered.
//
// This is the create path the product actually uses: rolesOf() filters the
// engineer out of the form, so the crew on the wire is agents only. The
// engineer row is added server-side, and everything about the task depends on
// it existing, because the task is posted FROM the engineer.
//
// Asserted on seq rather than on a claim, because seq is the order the rows
// were written and therefore the order the orchestrator reads them in. The
// task must sit before the play's own entry brief: what to do, then how this
// play wants it done.
func TestCreateWithTheWizardsCrewDeliversTheTaskBeforeTheEntryBrief(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	resp, body := h.do(h.device, "POST", "", map[string]any{
		"task": "Find why the guest export goes flaky above 500 rows",
		"cwd":  "/tmp/target",
		"play": "audit",
		"crew": []map[string]any{
			{"role": "ORCHESTRATOR", "tool": "claude", "model": ""},
			{"role": "INVESTIGATION", "tool": "claude", "model": ""},
			{"role": "REVIEW", "tool": "claude", "model": ""},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	loopID := body["loop"].(map[string]any)["id"].(string)

	// The engineer is a member even though the wizard never named one.
	members, err := h.st.ListLoopMembers(ctx, loopID)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	var engineer string
	for _, m := range members {
		if m.Role == store.RoleEngineer {
			engineer = m.ID
		}
	}
	if engineer == "" {
		t.Fatalf("a crew with no ENGINEER must still get one, got %d members", len(members))
	}

	rows, err := h.st.ListLoopMessages(ctx, loopID, 50)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("expected the task and the entry brief, got %d rows", len(rows))
	}
	first := rows[0]
	if first.Body != "Find why the guest export goes flaky above 500 rows" {
		t.Fatalf("the first row must be the engineer's own words, got %q from %s",
			first.Body, first.SenderRole)
	}
	if first.SenderRole != store.RoleEngineer || first.SenderID != engineer {
		t.Fatalf("the task is the engineer speaking: sender %q id %q", first.SenderRole, first.SenderID)
	}
	if store.BaseRole(first.RecipientRole) != store.RoleOrchestrator {
		t.Fatalf("the task goes to the orchestrator, got %q", first.RecipientRole)
	}

	briefAt := -1
	for i, m := range rows {
		if m.SenderRole == store.RoleEngine && strings.Contains(m.Body, "INVESTIGATION") {
			briefAt = i
			break
		}
	}
	if briefAt < 0 {
		t.Fatal("the play's entry brief must still be queued")
	}
	if briefAt == 0 {
		t.Fatal("the entry brief must not precede the task")
	}
}

// A name is not an instruction, and the loop refuses to start on one.
//
// Every loop created before this carried two to four words, because one field
// was labelled as the instruction and read as a name by three surfaces. The
// orchestrator then had nothing to work from: its transcript said "The task
// registered for this loop is just 'New PR Test' with no further detail".
func TestCreateRefusesATitleWearingTheInstructionsLabel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, tc := range []struct{ name, title, task string }{
		{"too short", "New PR Test", "New PR Test"},
		{"too few words", "Exports", "fix the exports properly and thoroughly indeed"[:19]},
		{"repeats the name", "Flaky guest export", "flaky   guest export"},
		{"empty", "New PR Test", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := h.do(h.device, "POST", "", map[string]any{
				"title": tc.title, "task": tc.task, "cwd": "/tmp/target", "play": "audit",
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("must be refused, got %d %v", resp.StatusCode, body)
			}
			if code := body["error"].(map[string]any)["code"]; code != "instruction_too_thin" {
				t.Fatalf("the reason must name what is missing, got %v", code)
			}
		})
	}

	// Refused BEFORE anything is written: no loop row, so no orphaned members
	// and no tokens handed out for a loop that does not exist.
	loops, err := h.st.ListLoops(ctx, "", 0)
	if err != nil {
		t.Fatalf("loops: %v", err)
	}
	if len(loops) != 0 {
		t.Fatalf("a refused create must leave no loop row, found %d", len(loops))
	}
}

// The two fields stay two things all the way through: the title names, the
// task instructs, and neither is derived from the other once both are given.
func TestTitleNamesTheLoopAndTheTaskInstructsIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "Flaky guest export",
		"task":  "Find why the guest export goes flaky above 500 rows and fix it",
		"cwd":   "/tmp/target", "play": "audit",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	loop := body["loop"].(map[string]any)
	if loop["title"] != "Flaky guest export" {
		t.Fatalf("the title must survive the round trip, got %v", loop["title"])
	}
	if loop["task"] != "Find why the guest export goes flaky above 500 rows and fix it" {
		t.Fatalf("the task must not be truncated into a name, got %v", loop["task"])
	}

	// And the instruction is what the orchestrator is sent, not the name.
	rows, err := h.st.ListLoopMessages(ctx, loop["id"].(string), 10)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if rows[0].Body != "Find why the guest export goes flaky above 500 rows and fix it" {
		t.Fatalf("the orchestrator is sent the instruction, got %q", rows[0].Body)
	}
}

// An omitted title is derived rather than refused: the name is for the
// engineer's eye, and a form that will not submit over it is worse.
func TestAnOmittedTitleIsDerivedFromTheInstruction(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(h.device, "POST", "", map[string]any{
		"task": "Find why the guest export goes flaky above 500 rows and fix it",
		"cwd":  "/tmp/target", "play": "audit",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	if title := body["loop"].(map[string]any)["title"]; title != "Find why the guest export goes" {
		t.Fatalf("expected the head of the instruction, got %v", title)
	}
}

// A continuation is briefed like a create, so it is held to the same floor.
//
// The parent's title carries over, but the successor's orchestrator reads the
// task, so a two-word round two leaves it exactly as uninstructed as a
// two-word round one did.
func TestContinueRefusesATitleShapedTask(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	loopID, _ := h.create("run round one against the export path")

	resp, body := h.do(h.device, "POST", "/"+loopID+"/continue", map[string]any{
		"task": "round two", "carryNotes": true,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a title-shaped continuation must be refused, got %d %v", resp.StatusCode, body)
	}
	if code := body["error"].(map[string]any)["code"]; code != "instruction_too_thin" {
		t.Fatalf("the reason must name what is missing, got %v", code)
	}

	// And no successor row: a refused continuation must not fork the loop.
	loops, err := h.st.ListLoops(ctx, "", 0)
	if err != nil {
		t.Fatalf("loops: %v", err)
	}
	for _, l := range loops {
		if l.ParentLoopID != "" {
			t.Fatalf("a refused continuation left a successor: %s", l.ID)
		}
	}
	// The parent is untouched: still active, not ended into a successor that
	// was never created.
	parent, err := h.st.GetLoop(ctx, loopID)
	if err != nil || parent == nil {
		t.Fatalf("parent: %v", err)
	}
	if parent.Ended() {
		t.Fatalf("a refused continuation must not end the parent, status %q", parent.Status)
	}
}

// A well-shaped model this install does not have is refused at create, and
// the refusal happens BEFORE anything is written.
//
// The shape floor stops "minimax". It cannot stop "anthropic/claude-9",
// because the registry does not know what this machine has authenticated;
// only the machine knows, so the machine is asked.
// auditCrew staffs the audit play on opencode with one model, so a test about
// models is not also a test about rosters.
func auditCrew(model string) []map[string]any {
	out := []map[string]any{}
	for _, role := range []string{"ORCHESTRATOR", "INVESTIGATION", "REVIEW"} {
		out = append(out, map[string]any{"role": role, "tool": "opencode", "model": model})
	}
	return out
}

func TestCreateRefusesAModelThisInstallDoesNotHave(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.srv.SetModelLister(&fakeModels{out: []engine.Model{
		{ID: "anthropic/claude-sonnet-4-5", DisplayName: "Sonnet", Provider: "anthropic"},
	}})

	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "Flaky export", "task": "Find why the guest export goes flaky above 500 rows",
		"cwd": "/tmp/target", "play": "audit",
		"crew": auditCrew("anthropic/claude-9"),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("must be refused, got %d %v", resp.StatusCode, body)
	}
	if code := body["error"].(map[string]any)["code"]; code != "unknown_model" {
		t.Fatalf("code: %v", code)
	}
	loops, _ := h.st.ListLoops(ctx, "", 0)
	if len(loops) != 0 {
		t.Fatalf("a refused create must leave no loop row, found %d", len(loops))
	}

	// And the model the catalogue DOES name is accepted.
	resp, body = h.do(h.device, "POST", "", map[string]any{
		"title": "Flaky export", "task": "Find why the guest export goes flaky above 500 rows",
		"cwd": "/tmp/target", "play": "audit",
		"crew": auditCrew("anthropic/claude-sonnet-4-5"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a model the catalogue names must be accepted: %d %v", resp.StatusCode, body)
	}
}

// No catalogue is not evidence of a bad model. A machine with no opencode, or
// a probe that timed out, must not turn into a refusal: the shape floor is
// what stands in that case, and it already ran.
func TestCreateAcceptsAWellShapedModelWhenTheCatalogueCannotBeObtained(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lister ModelLister
	}{
		{"the lister errors", &fakeModels{err: errors.New("opencode: probe: control API never came up")}},
		{"the lister answers empty", &fakeModels{out: []engine.Model{}}},
		{"there is no lister at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.lister != nil {
				h.srv.SetModelLister(tc.lister)
			}
			resp, body := h.do(h.device, "POST", "", map[string]any{
				"title": "Flaky export", "task": "Find why the guest export goes flaky above 500 rows",
				"cwd": "/tmp/target", "play": "audit",
				"crew": auditCrew("anthropic/claude-sonnet-4-5"),
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("must be accepted: %d %v", resp.StatusCode, body)
			}
		})
	}
}

// The shape floor runs with no lister and no network: it is the one check
// that needs nothing from the machine.
func TestCreateRefusesABareModelIdWithNoCatalogueAtAll(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "Flaky export", "task": "Find why the guest export goes flaky above 500 rows",
		"cwd": "/tmp/target", "play": "audit",
		"crew": auditCrew("minimax"),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bare id must be refused, got %d %v", resp.StatusCode, body)
	}
	if code := body["error"].(map[string]any)["code"]; code != "unknown_tool" {
		t.Fatalf("code: %v", code)
	}
}

// The catalogue is asked once per create, not once per member.
func TestCreateAsksTheCatalogueOncePerTool(t *testing.T) {
	h := newHarness(t)
	f := &fakeModels{out: []engine.Model{{ID: "anthropic/claude-sonnet-4-5", Provider: "anthropic"}}}
	h.srv.SetModelLister(f)

	resp, body := h.do(h.device, "POST", "", map[string]any{
		"title": "Flaky export", "task": "Find why the guest export goes flaky above 500 rows",
		"cwd": "/tmp/target", "play": "audit",
		"crew": []map[string]any{
			{"role": "ORCHESTRATOR", "tool": "opencode", "model": "anthropic/claude-sonnet-4-5"},
			{"role": "INVESTIGATION", "tool": "opencode", "model": "anthropic/claude-sonnet-4-5"},
			{"role": "REVIEW", "tool": "claude", "model": "opus"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	if f.calls != 1 {
		t.Fatalf("the catalogue must be asked once per tool, asked %d times", f.calls)
	}
}

// The board reads health off the crew rows. Before this it derived "running"
// from a session id, which is true forever once it is true once.
func TestTheCrewCarriesEachMembersHealth(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	loopID, handoffs := h.create("Find why the guest export goes flaky above 500 rows")

	_, body := h.do(h.device, "GET", "/"+loopID, nil)
	crew, _ := body["crew"].([]any)
	if len(crew) == 0 {
		t.Fatalf("no crew: %v", body)
	}
	seen := map[string]string{}
	for _, raw := range crew {
		row, _ := raw.(map[string]any)
		role, _ := row["role"].(string)
		if role == store.RoleEngineer {
			// A human, not a poller: no health is computed for him, and a
			// state invented for him would be a lie on the board.
			if _, ok := row["health"]; ok {
				t.Fatalf("the engineer must carry no health: %v", row)
			}
			continue
		}
		state, ok := row["health"].(string)
		if !ok {
			t.Fatalf("%s carries no health: %v", role, row)
		}
		seen[role] = state
	}
	if len(seen) == 0 {
		t.Fatal("no member carried health")
	}

	// Nothing is polling, nothing is held, and the entry brief only just
	// arrived, so nobody is stranded yet: fresh mail is not evidence.
	for role, state := range seen {
		if state == store.MemberStranded {
			t.Fatalf("%s must not read stranded seconds after creation", role)
		}
	}

	// And a parked poll shows through as listening.
	orch, _, _ := h.st.LoopMemberByToken(ctx, handoffs[store.RoleOrchestrator]["token"].(string))
	release := h.st.EnterPolling(orch.ID)
	defer release()
	_, body = h.do(h.device, "GET", "/"+loopID, nil)
	for _, raw := range body["crew"].([]any) {
		row, _ := raw.(map[string]any)
		if row["role"] == store.RoleOrchestrator {
			if row["health"] != store.MemberListening {
				t.Fatalf("a parked poll must read as listening, got %v", row["health"])
			}
			if row["listening"] != true {
				t.Fatalf("listening flag: %v", row["listening"])
			}
		}
	}
}

// A healthy loop carries NO attention entry.
//
// Paired with the client's "an empty entry renders nothing": each side alone
// leaves the empty-badge case unobserved, and an always-present badge is a
// badge nobody looks at.
func TestAHealthyLoopCarriesNoAttentionEntry(t *testing.T) {
	h := newHarness(t)
	loopID, _ := h.create("Find why the guest export goes flaky above 500 rows")

	_, body := h.do(h.device, "GET", "", nil)
	attention, ok := body["attention"].(map[string]any)
	if !ok {
		t.Fatalf("the list must carry an attention map: %v", body)
	}
	if entry, present := attention[loopID]; present {
		t.Fatalf("a loop created seconds ago needs nothing, got %v", entry)
	}
}

// And a loop that does need something says which member and why.
func TestALoopWithAFailedSpawnSaysSoInTheList(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	loopID, _ := h.create("Find why the guest export goes flaky above 500 rows")
	if err := h.st.MarkLoopMemberFailed(ctx, loopID, "REVIEW", "opencode: model must be provider/model"); err != nil {
		t.Fatalf("fail member: %v", err)
	}

	_, body := h.do(h.device, "GET", "", nil)
	attention, _ := body["attention"].(map[string]any)
	entry, _ := attention[loopID].(map[string]any)
	if entry == nil {
		t.Fatalf("the loop must appear in attention: %v", body["attention"])
	}
	failed, _ := entry["failed"].([]any)
	if len(failed) != 1 || failed[0] != "REVIEW" {
		t.Fatalf("the failed member must be named, got %v", entry)
	}
}
