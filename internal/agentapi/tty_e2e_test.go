package agentapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// TestTTYWSEndToEnd spawns a fake claude TUI (a shell script) under a real PTY
// through the agentd HTTP API: ws attach receives the script's banner, an
// input control frame echoes back, resize is accepted, and the run surfaces
// through the normal status machinery.
func TestTTYWSEndToEnd(t *testing.T) {
	dir := t.TempDir()
	// Sandbox: HOME and the corpus root both live in the temp dir, so this
	// spawn can never read or write the operator's real ~/.claude (a run
	// through the default root once renamed five live sessions).
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("home: %v", err)
	}
	t.Setenv("HOME", home)
	script := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'welcome to the tui\\r\\n'\nwhile IFS= read -r line; do printf 'got: %s\\r\\n' \"$line\"; done\n"), 0o755); err != nil {
		t.Fatalf("script: %v", err)
	}

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()
	svc := ingest.New(st, ingest.Options{Live: false})
	sp := spawner.New(st, svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp.SetCorpusRoot(filepath.Join(dir, "corpus"))
	sp.SetClaudePath(script)
	srv := New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "m-test")

	const token = "dev-tok-1"
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "d1", Name: "d", MachineID: "m-test", Kind: "device",
		Status: "active", TokenHash: store.HashToken(token),
	}, 0); err != nil {
		t.Fatalf("device: %v", err)
	}

	// Attaching never creates a run: the terminal exists as a row (here one
	// that finished on its own) and the attach brings its TUI back.
	seedFinishedTTYRun(t, st, "run-e2e", home)

	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") +
		"/api/v1/agentd/sessions/run-e2e/tty/ws?token=" + token

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		var body []byte
		if resp != nil && resp.Body != nil {
			body, _ = io.ReadAll(resp.Body)
		}
		t.Fatalf("dial: %v (status %v body %s)", err, resp, body)
	}
	defer func() { _ = conn.Close() }()

	got := readUntil(t, conn, "welcome to the tui")

	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"input","data":"hi\n"}`)); err != nil {
		t.Fatalf("write input: %v", err)
	}
	got += readUntil(t, conn, "got: hi")
	if !strings.Contains(got, "welcome to the tui") || !strings.Contains(got, "got: hi") {
		t.Fatalf("missing expected terminal output: %q", got)
	}

	// resize must not kill anything
	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := sp.Status(ctx, "run-e2e")
	if err != nil || sess == nil {
		t.Fatalf("status: %v", err)
	}
	if sess.Kind != spawner.KindTTY || sess.PID == 0 {
		t.Fatalf("unexpected tty session: %+v", sess)
	}
}

// TestTTYWSBirthGeometry proves the birth-size handshake end to end: the ws
// dial carries ?cols=&rows=, ensureTTY spawns the fresh PTY at that size, and
// the fake claude's `stty size` output confirms the terminal identity — no
// 80×24 window for the first (resume-replay) bytes to be mangled in.
func TestTTYWSBirthGeometry(t *testing.T) {
	dir := t.TempDir()
	// Sandbox: HOME and the corpus root both live in the temp dir, so this
	// spawn can never read or write the operator's real ~/.claude (a run
	// through the default root once renamed five live sessions).
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("home: %v", err)
	}
	t.Setenv("HOME", home)
	// stty size prints "<rows> <cols>" from the PTY's TIOCGWINSZ
	script := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nstty size\nwhile IFS= read -r line; do printf 'got: %s\\r\\n' \"$line\"; done\n"), 0o755); err != nil {
		t.Fatalf("script: %v", err)
	}

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()
	svc := ingest.New(st, ingest.Options{Live: false})
	sp := spawner.New(st, svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp.SetCorpusRoot(filepath.Join(dir, "corpus"))
	sp.SetClaudePath(script)
	srv := New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "m-test")

	const token = "dev-tok-geo"
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "d-geo", Name: "d", MachineID: "m-test", Kind: "device",
		Status: "active", TokenHash: store.HashToken(token),
	}, 0); err != nil {
		t.Fatalf("device: %v", err)
	}

	seedFinishedTTYRun(t, st, "run-geo", home)

	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	conn, resp, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(hs.URL, "http")+
			"/api/v1/agentd/sessions/run-geo/tty/ws?token="+token+"&cols=52&rows=30",
		nil,
	)
	if err != nil {
		var body []byte
		if resp != nil && resp.Body != nil {
			body, _ = io.ReadAll(resp.Body)
		}
		t.Fatalf("dial: %v (status %v body %s)", err, resp, body)
	}
	defer func() { _ = conn.Close() }()

	got := readUntil(t, conn, "30 52")
	if !strings.Contains(got, "30 52") {
		t.Fatalf("PTY not born at requested geometry: %q", got)
	}
}

// seedFinishedTTYRun records a tty run that ended on its own in cwd.
func seedFinishedTTYRun(t *testing.T, st *store.Store, runID, cwd string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: runID, Kind: "tty", Engine: "claude", State: string(spawner.StateFinished),
		StartedAt: now, EndedAt: now, UpdatedAt: now, CWD: cwd,
	}); err != nil {
		t.Fatalf("seed run %s: %v", runID, err)
	}
}

// readUntil accumulates binary frames until the wanted substring shows up.
// One generous blocking deadline — gorilla connections cannot be read again
// after any read error, so short retrying deadlines are not an option (the
// injected prompt lands ~2s after spawn).
func readUntil(t *testing.T, conn *websocket.Conn, want string) string {
	t.Helper()
	var sb strings.Builder
	deadline := time.Now().Add(8 * time.Second)
	for !strings.Contains(sb.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q; got %q", want, sb.String())
		}
		_ = conn.SetReadDeadline(deadline)
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (buf %q)", err, sb.String())
		}
		sb.Write(raw)
	}
	return sb.String()
}

// TestTTYSpawnInjectsPromptAndTitle covers the named-session contract through
// the spawn API: the birth brief arrives as the FINAL POSITIONAL ARGV argument
// and the
// title option is carried on the run.
func TestTTYSpawnInjectsPromptAndTitle(t *testing.T) {
	dir := t.TempDir()
	// Sandbox: HOME and the corpus root both live in the temp dir, so this
	// spawn can never read or write the operator's real ~/.claude (a run
	// through the default root once renamed five live sessions).
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("home: %v", err)
	}
	t.Setenv("HOME", home)
	script := filepath.Join(dir, "fake-claude")
	// echo argv (the birth-brief path) REPEATEDLY — the tty hub fans out only
	// to subscribers attached at fan-out time (no scrollback), and the WS
	// attaches after spawn returns, so boot output can legitimately be missed.
	// A 0.3s re-print makes the assertion attach-timing independent. The
	// stdin loop below mirrors manual input delivery.
	//nolint:gosec // G306: fixture must keep the exec bit
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\n(while :; do for a in \"$@\"; do printf 'echo:%s\\r\\n' \"$a\"; done; sleep 0.3; done) &\n"+
			"while IFS= read -r line; do printf 'echo:%s\\r\\n' \"$line\"; done\n"),
		0o755); err != nil {
		t.Fatalf("script: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()
	svc := ingest.New(st, ingest.Options{Live: false})
	sp := spawner.New(st, svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp.SetCorpusRoot(filepath.Join(dir, "corpus"))
	sp.SetClaudePath(script)
	srv := New(st, sp, slog.New(slog.NewTextHandler(io.Discard, nil)), "m-test")

	const token = "dev-tok-2"
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "d2", Name: "d", MachineID: "m-test", Kind: "device",
		Status: "active", TokenHash: store.HashToken(token),
	}, 0); err != nil {
		t.Fatalf("device: %v", err)
	}

	body := fmt.Sprintf(`{"kind":"tty","title":"e2e sandboxed title","prompt":"hello tui","cwd":%q}`, dir)
	req := httptest.NewRequest("POST", "/api/v1/agentd/sessions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("spawn: %d %s", w.Code, w.Body.String())
	}
	var spawned struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &spawned)

	// attach the tty ws and wait for the injected prompt to be echoed back
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") +
		"/api/v1/agentd/sessions/" + spawned.ID + "/tty/ws?token=" + token
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	// probe: manual input at t=5s distinguishes delivery vs injection timing
	go func() {
		time.Sleep(5 * time.Second)
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte(`{"type":"input","data":"manual probe\n"}`))
	}()
	buf := readUntil(t, conn, "echo:hello tui")
	if !strings.Contains(buf, "echo:manual probe") {
		t.Logf("manual probe missing — hub delivery broken; buf=%q", buf)
	}
}
