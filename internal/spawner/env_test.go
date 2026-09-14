package spawner

import (
	"strings"
	"testing"
)

// TestChildSpawnEnvStripsColorSuppressors pins the contract that spawned
// claude processes can never inherit capability-hostile variables from
// agentd's own environment (TERM=dumb leaked from an automation shell once
// and turned every phone TUI black and white).
func TestChildSpawnEnvStripsColorSuppressors(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/user",
		"TERM=dumb",
		"COLORTERM=truecolor",
		"NO_COLOR=1",
		"FORCE_COLOR=0",
		"CLICOLOR=0",
		"CI=true",
	}

	ttyEnv := ChildSpawnEnv(base, KindTTY, nil)
	assertEnv(t, ttyEnv, map[string]string{
		"PATH":      "/usr/bin",
		"HOME":      "/home/user",
		"TERM":      "xterm-256color",
		"COLORTERM": "truecolor",
	}, map[string]bool{
		"NO_COLOR": true, "FORCE_COLOR": true,
		"CLICOLOR": true, "CI": true,
	})

	headlessEnv := ChildSpawnEnv(base, KindOneShot, nil)
	assertEnv(t, headlessEnv, map[string]string{
		"PATH": "/usr/bin",
		"HOME": "/home/user",
	}, map[string]bool{
		// stripped so nothing downstream re-enables or misreads capabilities
		"NO_COLOR": true, "FORCE_COLOR": true, "CI": true,
		// never injected outside a real PTY
		"TERM": true, "COLORTERM": true,
	})

	extraEnv := ChildSpawnEnv(base, KindTTY, []string{"AF_TEST=1", "MODEL=x"})
	assertEnv(t, extraEnv, map[string]string{
		"AF_TEST": "1", "MODEL": "x",
		"TERM": "xterm-256color",
	}, nil)
}

func assertEnv(t *testing.T, env []string, want map[string]string, banned map[string]bool) {
	t.Helper()
	got := map[string]string{}
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			t.Fatalf("malformed env entry %q", kv)
		}
		got[kv[:i]] = kv[i+1:]
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for k := range banned {
		if _, ok := got[k]; ok {
			t.Errorf("%s must be stripped, got %q", k, got[k])
		}
	}
}

// TestChildSpawnEnvStripsParentClaudeSession pins the fix for the
// "phone session works but never appears" bug.
//
// agentd is routinely restarted from inside a Claude Code session (any agent
// restarting the service does exactly that), so it inherits that session's
// identity variables. Passing them to a spawned claude made the child adopt
// the PARENT's session identity — it answered questions normally but wrote no
// transcript of its own, so session-id discovery never resolved, the run sat
// in `starting` forever, it never showed up in the session list, and the
// conversation died with the process.
func TestChildSpawnEnvStripsParentClaudeSession(t *testing.T) {
	parent := []string{
		"CLAUDECODE=1",
		"CLAUDE_CODE_CHILD_SESSION=1",
		"CLAUDE_CODE_SESSION_ID=ff8abab7-d93f-463f-a61c-19f2be38e0f5",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_EXECPATH=/home/u/.local/bin/claude",
		"CLAUDE_CODE_MESSAGING_SOCKET=/run/user/1000/cc-socks/8641.sock",
		"CLAUDE_CODE_MESSAGING_TOKEN=secret",
		"CLAUDE_PID=8641",
		"CLAUDE_EFFORT=high",
		// must SURVIVE: not session identity, and needed by the child
		"HOME=/home/u",
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-test",
		"CLAUDE_CONFIG_DIR=/home/u/.claude",
	}
	got := ChildSpawnEnv(parent, KindTTY, nil)

	mustGo := []string{
		"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXECPATH",
		"CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
		"CLAUDE_PID", "CLAUDE_EFFORT",
	}
	for _, k := range mustGo {
		for _, kv := range got {
			if strings.HasPrefix(kv, k+"=") {
				t.Errorf("%s leaked into the spawned session — the child would adopt the parent's identity and write no transcript", k)
			}
		}
	}
	mustStay := map[string]string{
		"HOME":              "/home/u",
		"PATH":              "/usr/bin",
		"ANTHROPIC_API_KEY": "sk-test",
		"CLAUDE_CONFIG_DIR": "/home/u/.claude",
	}
	for k, want := range mustStay {
		found := ""
		for _, kv := range got {
			if strings.HasPrefix(kv, k+"=") {
				found = strings.TrimPrefix(kv, k+"=")
			}
		}
		if found != want {
			t.Errorf("%s = %q, want %q (stripping must not remove real config)", k, found, want)
		}
	}
}
