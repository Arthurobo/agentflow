package engine_test

import (
	"context"
	"testing"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/engine/claude"
	"github.com/arthurobo/agentflow/internal/engine/opencode"
)

// TestRegistryRegisterAndResolve is the minimal lifecycle: an engine
// registers, Get returns it, an empty id resolves to claude.
func TestRegistryRegisterAndResolve(t *testing.T) {
	r := engine.NewRegistry()
	r.Register(claude.New(""))
	r.Register(opencode.New(opencode.Config{Hostname: "127.0.0.1"}))

	if got, err := r.Get(engine.IDClaude); err != nil || got.ID() != engine.IDClaude {
		t.Fatalf("claude lookup: %v %s", err, got)
	}
	if got, err := r.Get(engine.IDOpenCode); err != nil || got.ID() != engine.IDOpenCode {
		t.Fatalf("opencode lookup: %v %s", err, got)
	}
	// empty id resolves to claude (every pre-existing row is a claude run)
	if got, err := r.Get(""); err != nil || got.ID() != engine.IDClaude {
		t.Fatalf("empty id lookup: %v %s", err, got)
	}
}

// TestClaudeBuildArgs pins the argv shape that today's spawner
// TestBuildArgsTTYLoopMember already verifies. The engine must produce the
// same surface (--name, --model, --dangerously-skip-permissions, prompt).
func TestClaudeBuildArgs(t *testing.T) {
	e := claude.New("")
	args, err := e.BuildArgs(engine.Options{
		Title:  "demo",
		Model:  "claude-sonnet-4-5",
		Prompt: "hi",
		Cwd:    "/tmp",
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	want := []string{"--name", "demo", "--model", "claude-sonnet-4-5", "--dangerously-skip-permissions", "hi"}
	if !equal(args, want) {
		t.Fatalf("argv = %v, want %v", args, want)
	}
	if e.NeedsControlPort() {
		t.Fatalf("claude must not allocate a control port")
	}
}

// TestOpenCodeBuildArgs pins the opencode argv shape: --port, --hostname,
// optional --session / --agent / --model / cwd.
func TestOpenCodeBuildArgs(t *testing.T) {
	e := opencode.New(opencode.Config{
		Hostname:      "127.0.0.1",
		PortAllocator: func() (int, error) { return 47312, nil },
	})
	args, err := e.BuildArgs(engine.Options{
		Cwd:             "/tmp/demo",
		Model:           "anthropic/claude-sonnet-4-5",
		Agent:           "build",
		ResumeSessionID: "ses_abc",
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	want := []string{"--port", "47312", "--hostname", "127.0.0.1",
		"--session", "ses_abc", "--agent", "build",
		"--model", "anthropic/claude-sonnet-4-5", "--auto", "/tmp/demo"}
	if !equal(args, want) {
		t.Fatalf("argv = %v, want %v", args, want)
	}
	if !e.NeedsControlPort() {
		t.Fatalf("opencode must allocate a control port")
	}
}

// TestOpenCodeAllocateAndPrepare verifies port allocation never returns 0
// (the spawner contract: --port 0 lets the child pick its own port which we
// cannot discover) and that the base URL points at the configured
// hostname.
func TestOpenCodeAllocateAndPrepare(t *testing.T) {
	e := opencode.New(opencode.Config{
		Hostname:      "127.0.0.1",
		PortAllocator: func() (int, error) { return 49152, nil },
	})
	port, base, sid, err := e.AllocateAndPrepare(context.Background(), "/tmp/demo", "title")
	if err != nil {
		t.Fatalf("AllocateAndPrepare: %v", err)
	}
	if port != 49152 {
		t.Fatalf("port = %d, want 49152", port)
	}
	if base != "http://127.0.0.1:49152" {
		t.Fatalf("base = %q, want %q", base, "http://127.0.0.1:49152")
	}
	if sid != "" {
		t.Fatalf("session id should be empty pre-bind, got %q", sid)
	}
}

// TestValidatePort pins the "never --port 0" guard.
func TestValidatePort(t *testing.T) {
	if err := opencode.ValidatePort(0); err == nil {
		t.Fatalf("port 0 must error")
	}
	if err := opencode.ValidatePort(70000); err == nil {
		t.Fatalf("port 70000 must error")
	}
	if err := opencode.ValidatePort(47312); err != nil {
		t.Fatalf("port 47312 must validate: %v", err)
	}
}

// TestCapabilitiesShape pins the Caps struct layout — the frontend renders
// pickers off these flags, so a silent rename would silently break the
// control sheet.
func TestCapabilitiesShape(t *testing.T) {
	capSet := map[string]bool{}
	for _, c := range []engine.Caps{
		claude.NewControl().Capabilities(),
		opencode.NewControl("http://127.0.0.1:0").Capabilities(),
	} {
		// spot-check both engines set at least one capability flag
		if !c.SetModel && !c.Prompt {
			t.Fatalf("engine has neither SetModel nor Prompt: %+v", c)
		}
		capSet["seen"] = true
	}
	if !capSet["seen"] {
		t.Fatalf("no engine Capabilities returned")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An OpenCode member with no --prompt boots with no instructions and never
// starts polling, which is a member that exists and does nothing. Verified
// live on 1.18.29: --prompt is processed as the first turn at boot, the same
// contract claude gets from its trailing positional.
func TestOpenCodeCarriesTheBriefOnArgv(t *testing.T) {
	e := opencode.New(opencode.Config{
		Hostname:      "127.0.0.1",
		PortAllocator: func() (int, error) { return 47312, nil },
	})
	brief := "You are REVIEW.\nWAIT\n  while true; do curl ...; done"
	args, err := e.BuildArgs(engine.Options{Cwd: "/tmp/demo", Prompt: brief})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	at := -1
	for i, a := range args {
		if a == "--prompt" {
			at = i
		}
	}
	if at < 0 || at+1 >= len(args) || args[at+1] != brief {
		t.Fatalf("the brief must ride --prompt verbatim: %v", args)
	}
	// cwd stays the trailing positional; a flag after it would be read as one.
	if args[len(args)-1] != "/tmp/demo" {
		t.Fatalf("cwd must stay last: %v", args)
	}
	// and nothing may stall on a permission prompt nobody can answer
	if !contains(args, "--auto") {
		t.Fatalf("a managed member must skip permission prompts: %v", args)
	}
}

// A bare model id is not an error to opencode: it resolves to nothing and the
// member silently runs on the default. Refusing is the honest answer.
func TestOpenCodeRefusesABareModelID(t *testing.T) {
	e := opencode.New(opencode.Config{
		Hostname:      "127.0.0.1",
		PortAllocator: func() (int, error) { return 47312, nil },
	})
	if _, err := e.BuildArgs(engine.Options{Cwd: "/tmp/demo", Model: "sonnet"}); err == nil {
		t.Fatal("a bare model id must be refused, not silently ignored")
	}
	// an openrouter-style route has slashes in the model half and is fine
	if _, err := e.BuildArgs(engine.Options{Cwd: "/tmp/demo", Model: "openrouter/anthropic/claude-3"}); err != nil {
		t.Fatalf("a multi-segment model is valid: %v", err)
	}
	// and empty still means "let opencode choose"
	if _, err := e.BuildArgs(engine.Options{Cwd: "/tmp/demo"}); err != nil {
		t.Fatalf("an empty model must stay allowed: %v", err)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
