package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/loopapi"
	"github.com/arthurobo/agentflow/internal/store"
)

// The catalogue costs a process, a port and a wait for it to fill. The picker
// asks for it, and then create asks again to check the model the picker just
// offered, so without a cache one loop paid for it two or three times.
func TestTheModelCatalogueIsProbedOncePerTTL(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	m := &loopModels{
		binary: "opencode",
		probe: func(context.Context, string, string) ([]engine.Model, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			return []engine.Model{{ID: "anthropic/claude-sonnet-4-5", Provider: "anthropic"}}, nil
		},
	}
	ctx := context.Background()

	first, err := m.Models(ctx, store.ToolOpenCode, "/tmp/a")
	if err != nil || len(first) != 1 {
		t.Fatalf("first: %v %v", first, err)
	}
	second, err := m.Models(ctx, store.ToolOpenCode, "/tmp/a")
	if err != nil || len(second) != 1 {
		t.Fatalf("second: %v %v", second, err)
	}
	// A different cwd is the SAME catalogue: the providers come from the
	// global config, and the appearance of directory dependence was a cold
	// probe answering before it had filled.
	third, err := m.Models(ctx, store.ToolOpenCode, "/tmp/somewhere-else")
	if err != nil || len(third) != 1 {
		t.Fatalf("third: %v %v", third, err)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("the probe must run once inside the TTL, ran %d times", got)
	}
}

// The cached slice must not be the one the caller can mutate, or one screen
// editing its list corrupts the next screen's.
func TestTheCachedCatalogueIsCopiedOut(t *testing.T) {
	m := &loopModels{
		binary: "opencode",
		probe: func(context.Context, string, string) ([]engine.Model, error) {
			return []engine.Model{{ID: "anthropic/claude-sonnet-4-5"}}, nil
		},
	}
	ctx := context.Background()
	first, _ := m.Models(ctx, store.ToolOpenCode, "")
	first[0].ID = "tampered"
	second, _ := m.Models(ctx, store.ToolOpenCode, "")
	if second[0].ID != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("the cache handed out its own backing array: %q", second[0].ID)
	}
}

// An empty answer is the answer of a machine with no authenticated provider
// AND of a probe that gave up early. Caching it would hold the wrong one of
// those for ten minutes.
func TestAnEmptyCatalogueIsNotCached(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	m := &loopModels{
		binary: "opencode",
		probe: func(context.Context, string, string) ([]engine.Model, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			return nil, nil
		},
	}
	ctx := context.Background()
	_, _ = m.Models(ctx, store.ToolOpenCode, "")
	_, _ = m.Models(ctx, store.ToolOpenCode, "")
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("an empty catalogue must be re-asked, probed %d times", got)
	}
}

// A failed probe is not a catalogue either.
func TestAFailedProbeIsNotCached(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	m := &loopModels{
		binary: "opencode",
		probe: func(context.Context, string, string) ([]engine.Model, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			return nil, errors.New("control API never came up")
		},
	}
	ctx := context.Background()
	if _, err := m.Models(ctx, store.ToolOpenCode, ""); err == nil {
		t.Fatal("a probe failure must surface")
	}
	_, _ = m.Models(ctx, store.ToolOpenCode, "")
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("a failure must be re-asked, probed %d times", got)
	}
}

// The key carries the binary's mtime, so upgrading opencode invalidates the
// catalogue without anyone remembering to.
func TestTheCacheKeyChangesWhenTheBinaryDoes(t *testing.T) {
	bin := t.TempDir() + "/opencode"
	write := func(mod time.Time) {
		if err := writeExecutable(bin); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := chtimes(bin, mod); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	m := &loopModels{binary: bin}
	write(time.Now().Add(-time.Hour))
	before := m.cacheKey("")
	write(time.Now())
	if after := m.cacheKey(""); after == before {
		t.Fatalf("an upgraded binary must change the key, both %q", before)
	}
}

func writeExecutable(path string) error      { return os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755) }
func chtimes(path string, t time.Time) error { return os.Chtimes(path, t, t) }

// The nudge is an ACCELERATOR. These pin what it may and may not do; every
// delivery guarantee is tested elsewhere and holds with this file deleted.

type fakeTTY struct {
	mu     sync.Mutex
	writes map[string][]string
}

func (f *fakeTTY) submit(runID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writes == nil {
		f.writes = map[string][]string{}
	}
	f.writes[runID] = append(f.writes[runID], text)
	return nil
}

func (f *fakeTTY) all(runID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes[runID]...)
}

// One line, no body, once. The body must never travel this way: the bus is the
// only contract every agent speaks, and a message arriving by two routes is
// two sources of truth.
func TestTheNudgeCarriesOneLineAndNeverTheMessage(t *testing.T) {
	f := &fakeTTY{}
	n := newNudger(f.submit, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	secret := "please check the second table, the one with the guest ids"
	ok := n.Nudge(context.Background(), store.StrandedMember{
		MemberID: "m1", RunID: "run_1", Role: "INVESTIGATION", Tool: store.ToolClaude,
	})
	if !ok {
		t.Fatal("a claude member with a run must be nudged")
	}
	got := f.all("run_1")
	if len(got) != 1 {
		t.Fatalf("exactly one write, got %d", len(got))
	}
	if !strings.Contains(got[0], "You have mail") {
		t.Fatalf("the nudge must say what to do: %q", got[0])
	}
	if strings.Contains(got[0], secret) || len(got[0]) > 64 {
		t.Fatalf("the nudge must never carry content: %q", got[0])
	}
	// The text goes through submit, which presses Enter as its own write
	// after a pause. A carriage return glued to the line arrives as part of
	// a paste and leaves the nudge sitting unsent in the prompt box.
	if strings.ContainsAny(got[0], "\r\n") {
		t.Fatalf("the nudge text must not carry its own Enter: %q", got[0])
	}
}

// A member with no run has no process here to poke, and must not be counted
// as nudged, or the board would claim something was done that was not.
func TestAMemberWithNoRunIsNotNudged(t *testing.T) {
	f := &fakeTTY{}
	n := newNudger(f.submit, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n.Nudge(context.Background(), store.StrandedMember{MemberID: "m1", Tool: store.ToolClaude}) {
		t.Fatal("a member running outside agentd cannot be nudged")
	}
	if len(f.all("")) != 0 {
		t.Fatal("nothing may be written for a member with no run")
	}
}

// Rate limited: once per transition, then not again until the retry window.
// Otherwise a member that ignores the first nudge gets a keystroke every
// thirty seconds into a terminal the engineer may be reading.
func TestTheNudgeIsRateLimitedPerMember(t *testing.T) {
	f := &fakeTTY{}
	n := newNudger(f.submit, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m := store.StrandedMember{MemberID: "m1", RunID: "run_1", Tool: store.ToolClaude}

	if !n.Nudge(context.Background(), m) {
		t.Fatal("the first nudge must be sent")
	}
	if n.Nudge(context.Background(), m) {
		t.Fatal("a second nudge inside the retry window must be suppressed")
	}
	if got := f.all("run_1"); len(got) != 1 {
		t.Fatalf("one write, got %d", len(got))
	}

	// Past the window it is due again.
	n.mu.Lock()
	n.last["m1"] = time.Now().Add(-nudgeRetry - time.Second)
	n.mu.Unlock()
	if !n.Nudge(context.Background(), m) {
		t.Fatal("past the retry window a still-stranded member is nudged again")
	}
	if got := f.all("run_1"); len(got) != 2 {
		t.Fatalf("two writes, got %d", len(got))
	}
}

// Two members are two budgets: one being nudged must not silence the other.
func TestTheRateLimitIsPerMemberNotGlobal(t *testing.T) {
	f := &fakeTTY{}
	n := newNudger(f.submit, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if !n.Nudge(ctx, store.StrandedMember{MemberID: "m1", RunID: "run_1", Tool: store.ToolClaude}) {
		t.Fatal("first member")
	}
	if !n.Nudge(ctx, store.StrandedMember{MemberID: "m2", RunID: "run_2", Tool: store.ToolClaude}) {
		t.Fatal("a different member has its own budget")
	}
}

// A member's token must not ride argv, where any local user can read it. It
// goes into an owner-only file, the brief's commands read it from there, and
// the brief says so.
func TestTheMemberTokenStaysOffArgv(t *testing.T) {
	l := newLoopLauncher(nil, nil, t.TempDir(), quietLog())
	const token = "mt_secret_token_value"
	brief := `curl -s "http://127.0.0.1:4344/api/v1/agent/whoami" -H "X-Agent-Token: ` + token + `"`
	prompt, err := l.hideToken("run-123", loopapi.LaunchSpec{Prompt: brief, Token: token})
	if err != nil {
		t.Fatalf("hide: %v", err)
	}
	if strings.Contains(prompt, token) {
		t.Fatalf("the token is still in the brief: %q", prompt)
	}
	path := filepath.Join(l.tokenDir, "run-123.token")
	if !strings.Contains(prompt, path) {
		t.Fatalf("the brief must say where the token is: %q", prompt)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the token file must be owner-only: %v %v", info, err)
	}
	if dir, _ := os.Stat(l.tokenDir); dir.Mode().Perm() != 0o700 {
		t.Fatalf("the token directory must be owner-only: %v", dir.Mode())
	}
	// The rewritten header really expands to the token in a shell.
	header := prompt[strings.Index(prompt, `"X-Agent-Token:`):]
	header = header[:strings.Index(header[1:], `"`)+2]
	out, err := exec.Command("sh", "-c", "printf '%s' "+header).Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	if string(out) != "X-Agent-Token: "+token {
		t.Fatalf("the command must read the token from the file, got %q", out)
	}
	l.forgetToken("run-123")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stopping a member removes its token file: %v", err)
	}
}

func TestATokenPathThatCannotBeQuotedIsRefused(t *testing.T) {
	l := newLoopLauncher(nil, nil, filepath.Join(t.TempDir(), "it's here"), quietLog())
	if _, err := l.hideToken("run-1", loopapi.LaunchSpec{Prompt: "X tok", Token: "tok"}); err == nil {
		t.Fatal("a path that would break the shell quoting must be refused")
	}
}
