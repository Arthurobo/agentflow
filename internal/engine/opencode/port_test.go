package opencode

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/arthurobo/agentflow/internal/engine"
)

// Every recent OpenCode run sat in state='starting' with an empty session
// id, invisible to a Sessions list keyed on session id. The cause was two
// allocations for one run: the host reserved a port and recorded it on the
// row, then BuildArgs — which had no way to learn that port — allocated a
// SECOND one and put it in the argv. The child listened on one port while
// PostStart polled the other, WaitReady timed out, and nothing ever bound.
//
// Live evidence from the orchestrator: run 13e66da0 had control_port=35181
// on the row and `--port 41751` on the process; run db000d95 had 46511 and
// 36053. Both actual ports answered /session perfectly. OpenCode was fine;
// agentd was looking in the wrong place.

// countingAllocator hands out ports and counts how often it was asked.
type countingAllocator struct {
	mu    sync.Mutex
	calls int
	next  int
}

func (c *countingAllocator) alloc() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.next++
	return 40000 + c.next, nil
}

func (c *countingAllocator) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func portFromArgs(t *testing.T, args []string) int {
	t.Helper()
	for i, a := range args {
		if a == "--port" && i+1 < len(args) {
			p, err := strconv.Atoi(args[i+1])
			if err != nil {
				t.Fatalf("--port %q is not a number", args[i+1])
			}
			return p
		}
	}
	t.Fatalf("argv has no --port: %v", args)
	return 0
}

// The fix, stated as the property that was violated.
func TestBuildArgsUsesThePreallocatedPortAndDoesNotAllocateAgain(t *testing.T) {
	alloc := &countingAllocator{}
	e := New(Config{Hostname: "127.0.0.1", PortAllocator: alloc.alloc})

	args, err := e.BuildArgs(engine.Options{Cwd: "/w", ControlPort: 47312})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if got := portFromArgs(t, args); got != 47312 {
		t.Fatalf("argv --port = %d, want the pre-allocated 47312", got)
	}
	if alloc.count() != 0 {
		t.Fatalf("BuildArgs allocated a second port (%d calls); the run would never bind", alloc.count())
	}
}

// The un-prepared path still works: a caller that reserved nothing gets a
// port chosen for it.
func TestBuildArgsFallsBackToAllocationWithNoPreallocatedPort(t *testing.T) {
	alloc := &countingAllocator{}
	e := New(Config{Hostname: "127.0.0.1", PortAllocator: alloc.alloc})

	args, err := e.BuildArgs(engine.Options{Cwd: "/w"}) // ControlPort zero
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if got := portFromArgs(t, args); got == 0 {
		t.Fatal("with nothing pre-allocated BuildArgs must choose a port")
	}
	if alloc.count() != 1 {
		t.Fatalf("allocator called %d times, want exactly 1", alloc.count())
	}
}

// The whole-spawn property: ONE allocation across prepare + argv, and the
// port the host records is the port the child is told to listen on.
func TestOneAllocationPerSpawnAndTheRowMatchesTheArgv(t *testing.T) {
	alloc := &countingAllocator{}
	e := New(Config{Hostname: "127.0.0.1", PortAllocator: alloc.alloc})

	// 1. the host reserves the port and records the base URL on the row
	port, base, _, err := e.AllocateAndPrepare(context.Background(), "/w", "Namely")
	if err != nil {
		t.Fatalf("AllocateAndPrepare: %v", err)
	}
	// 2. the spawner builds the argv, passing that port through
	args, err := e.BuildArgs(engine.Options{Cwd: "/w", ControlPort: port})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	if alloc.count() != 1 {
		t.Fatalf("one spawn asked for %d ports; exactly one is the whole fix", alloc.count())
	}
	argvPort := portFromArgs(t, args)
	if argvPort != port {
		t.Fatalf("child told to listen on %d, host polling %d — this is the bug", argvPort, port)
	}
	if want := e.AllocatedBaseURL(argvPort); base != want {
		t.Fatalf("recorded base %q does not address the argv port (%q)", base, want)
	}
}

// The pre-allocated port must survive the rest of the argv builder.
func TestThePreallocatedPortSurvivesEveryOtherFlag(t *testing.T) {
	alloc := &countingAllocator{}
	e := New(Config{Hostname: "127.0.0.1", PortAllocator: alloc.alloc})

	args, err := e.BuildArgs(engine.Options{
		Cwd:             "/w",
		ControlPort:     51234,
		Model:           "anthropic/claude-sonnet-4-5",
		Agent:           "build",
		Prompt:          "do the thing",
		ResumeSessionID: "ses_abc",
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if got := portFromArgs(t, args); got != 51234 {
		t.Fatalf("--port = %d after the other flags, want 51234", got)
	}
	if alloc.count() != 0 {
		t.Fatalf("allocator called %d times", alloc.count())
	}
}
