package agentapi

import (
	"bytes"
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
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arthurobo/agentflow/internal/approvals"
	"github.com/arthurobo/agentflow/internal/ingest"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

const lifeToken = "life-device-token"

type lifeFixture struct {
	t      *testing.T
	dir    string
	home   string
	marker string
	st     *store.Store
	sp     *spawner.Spawner
	srv    *Server
	hs     *httptest.Server
}

// newLifeFixture wires a real store and spawner under a sandboxed HOME. The
// fake engine appends a line to marker each time it starts as a terminal and
// then echoes its input, so tests can count spawns.
func newLifeFixture(t *testing.T) *lifeFixture {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("home: %v", err)
	}
	t.Setenv("HOME", home)
	marker := filepath.Join(dir, "spawned")
	fake := filepath.Join(dir, "fake-claude")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo '2.1.270 (Claude Code)'; exit 0; fi\n" +
		"echo started >> '" + marker + "'\n" +
		"printf 'tui up\\r\\n'\n" +
		"while IFS= read -r line; do printf 'got: %s\\r\\n' \"$line\"; done\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { //nolint:gosec // the fixture must be executable
		t.Fatalf("fake: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sp := spawner.New(st, ingest.New(st, ingest.Options{Live: false, CorpusRoot: "/dev/null-nontailing"}), logger)
	sp.SetClaudePath(fake)
	sp.SetCorpusRoot(filepath.Join(dir, "corpus"))
	t.Cleanup(func() { _ = sp.StopAll(context.Background()) })
	if err := st.UpsertDevice(context.Background(), &store.Device{
		ID: "dev-life", Kind: "device", Status: "active", TokenHash: store.HashToken(lifeToken),
	}, 0); err != nil {
		t.Fatalf("device: %v", err)
	}
	srv := New(st, sp, logger, "m-life")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &lifeFixture{t: t, dir: dir, home: home, marker: marker, st: st, sp: sp, srv: srv, hs: hs}
}

func (f *lifeFixture) spawns() int {
	raw, _ := os.ReadFile(f.marker)
	return strings.Count(string(raw), "started")
}

func (f *lifeFixture) seedRun(id, state, stopReason, sessionID string) {
	f.t.Helper()
	now := time.Now().UnixMilli()
	if err := f.st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: id, SessionID: sessionID, Kind: "tty", Engine: "claude", State: state,
		StopReason: stopReason, StartedAt: now, EndedAt: now, UpdatedAt: now, CWD: f.dir,
	}); err != nil {
		f.t.Fatalf("seed %s: %v", id, err)
	}
}

func (f *lifeFixture) post(path string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+lifeToken)
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	return w
}

func (f *lifeFixture) dialTTY(runID, extra string) (*websocket.Conn, *http.Response, error) {
	u := "ws" + strings.TrimPrefix(f.hs.URL, "http") + "/api/v1/agentd/sessions/" + runID + "/tty/ws?token=" + lifeToken + extra
	return websocket.DefaultDialer.Dial(u, nil)
}

// --- stopped runs stay stopped, finished runs resume -----------------------------

func TestAttachDoesNotReviveAUserStoppedRun(t *testing.T) {
	f := newLifeFixture(t)
	f.seedRun("run-stopped", string(spawner.StateStopped), "user_stop", "sess-stopped")
	for _, extra := range []string{"", "&restart=1", "&restart=true"} {
		_, resp, err := f.dialTTY("run-stopped", extra)
		if err == nil || resp == nil || resp.StatusCode != http.StatusGone {
			t.Fatalf("ws attach%s on a stopped run: %v %v, want 410", extra, resp, err)
		}
	}
	if w := f.post("/api/v1/agentd/sessions/run-stopped/tty"); w.Code != http.StatusGone {
		t.Fatalf("POST tty without restart: %d, want 410", w.Code)
	}
	if w := f.post("/api/v1/agentd/sessions/by-session/sess-stopped/tty"); w.Code != http.StatusGone {
		t.Fatalf("by-session on a stopped run: %d, want 410 (%s)", w.Code, w.Body.String())
	}
	time.Sleep(300 * time.Millisecond)
	if n := f.spawns(); n != 0 {
		t.Fatalf("a stopped run was revived %d times", n)
	}
}

func TestReapedRunsAreNotRevivedEither(t *testing.T) {
	f := newLifeFixture(t)
	f.seedRun("run-orphan", string(spawner.StateStopped), "orphaned_unverified", "")
	if w := f.post("/api/v1/agentd/sessions/run-orphan/tty"); w.Code != http.StatusGone {
		t.Fatalf("POST tty on a reaped run: %d, want 410", w.Code)
	}
	if n := f.spawns(); n != 0 {
		t.Fatalf("a reaped run was revived %d times", n)
	}
}

func TestExplicitRestartRevivesAStoppedRun(t *testing.T) {
	f := newLifeFixture(t)
	f.seedRun("run-stopped", string(spawner.StateStopped), "user_stop", "")
	if w := f.post("/api/v1/agentd/sessions/run-stopped/tty?restart=true"); w.Code != http.StatusOK {
		t.Fatalf("POST tty?restart=true: %d %s", w.Code, w.Body.String())
	}
	// The fake-claude process writes its marker asynchronously after the PTY
	// spawns, so wait for it rather than reading the count synchronously.
	waitFor(t, func() bool { return f.spawns() == 1 })
}

func TestAFinishedRunResumesOnAttachWithoutAFlag(t *testing.T) {
	f := newLifeFixture(t)
	f.seedRun("run-finished", string(spawner.StateFinished), "", "")
	conn, resp, err := f.dialTTY("run-finished", "")
	if err != nil {
		t.Fatalf("attach a finished run: %v %v", resp, err)
	}
	defer func() { _ = conn.Close() }()
	readUntil(t, conn, "tui up")
	if n := f.spawns(); n != 1 {
		t.Fatalf("finished run resumed %d times, want 1", n)
	}
}

// --- one process per claude session ------------------------------------------------

func TestASessionALiveRunHoldsIsNeverStartedTwice(t *testing.T) {
	f := newLifeFixture(t)
	f.seedRun("run-live", string(spawner.StateRunning), "", "sess-shared")
	master := newRecordingMaster()
	f.srv.RegisterTTY("run-live", master, nil, 0)
	t.Cleanup(func() { _ = master.Close() })
	f.seedRun("run-old", string(spawner.StateFinished), "", "sess-shared")

	if w := f.post("/api/v1/agentd/sessions/run-old/tty"); w.Code != http.StatusConflict {
		t.Fatalf("resuming a session a live run holds: %d, want 409 (%s)", w.Code, w.Body.String())
	}
	w := f.post("/api/v1/agentd/sessions/by-session/sess-shared/tty")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"run-live"`) {
		t.Fatalf("by-session must hand back the live run: %d %s", w.Code, w.Body.String())
	}
	if n := f.spawns(); n != 0 {
		t.Fatalf("a held session was started %d times", n)
	}
}

func TestConcurrentOpensOfOneSessionStartOneProcess(t *testing.T) {
	f := newLifeFixture(t)
	var wg sync.WaitGroup
	ids := make([]string, 4)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := f.post("/api/v1/agentd/sessions/by-session/sess-unmanaged/tty")
			var out struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			ids[i] = out.ID
		}(i)
	}
	wg.Wait()
	time.Sleep(300 * time.Millisecond)
	if n := f.spawns(); n != 1 {
		t.Fatalf("four concurrent opens started %d processes, want 1 (ids %v)", n, ids)
	}
	for _, id := range ids {
		if id != ids[0] || id == "" {
			t.Fatalf("every open must get the same run, got %v", ids)
		}
	}
}

// --- revocation closes live sockets -----------------------------------------------

func TestRevocationClosesEveryOpenTerminalAndStopsItsInput(t *testing.T) {
	for _, via := range []string{"http", "server"} {
		t.Run(via, func(t *testing.T) {
			f := newLifeFixture(t)
			f.seedRun("run-live", string(spawner.StateRunning), "", "")
			master := newRecordingMaster()
			f.srv.RegisterTTY("run-live", master, nil, 0)
			t.Cleanup(func() { _ = master.Close() })

			tab1, _, err := f.dialTTY("run-live", "")
			if err != nil {
				t.Fatalf("tab 1: %v", err)
			}
			defer func() { _ = tab1.Close() }()
			tab2, _, err := f.dialTTY("run-live", "")
			if err != nil {
				t.Fatalf("tab 2: %v", err)
			}
			defer func() { _ = tab2.Close() }()
			// Closing a third tab must not unregister the other two.
			tab3, _, err := f.dialTTY("run-live", "")
			if err != nil {
				t.Fatalf("tab 3: %v", err)
			}
			_ = tab3.Close()

			if err := tab1.WriteMessage(websocket.BinaryMessage, []byte("before")); err != nil {
				t.Fatalf("write: %v", err)
			}
			waitFor(t, func() bool { w, _ := master.snapshot(); return strings.Contains(strings.Join(w, ""), "before") })
			waitFor(t, func() bool { return f.srv.wsRegistry.Len() == 2 })

			if via == "http" {
				if w := f.post("/api/v1/agentd/devices/dev-life/revoke"); w.Code != http.StatusOK {
					t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
				}
			} else if n, err := f.srv.RevokeDevice(context.Background(), "dev-life"); err != nil || n != 2 {
				t.Fatalf("RevokeDevice closed %d (%v), want 2", n, err)
			}
			_ = tab1.WriteMessage(websocket.BinaryMessage, []byte("after"))
			_ = tab2.WriteMessage(websocket.TextMessage, []byte(`{"type":"input","data":"after"}`))

			for i, tab := range []*websocket.Conn{tab1, tab2} {
				if !closesWithin(tab, 2*time.Second) {
					t.Fatalf("tab %d still open 2s after revocation", i+1)
				}
			}
			time.Sleep(200 * time.Millisecond)
			if w, _ := master.snapshot(); strings.Contains(strings.Join(w, ""), "after") {
				t.Fatalf("input sent after revocation reached the PTY: %q", w)
			}
		})
	}
}

func TestLongLivedSocketsNoticeARevocationFromAnyPath(t *testing.T) {
	f := newLifeFixture(t)
	f.srv.wsReverifyEvery = 100 * time.Millisecond
	f.seedRun("run-live", string(spawner.StateRunning), "", "")
	master := newRecordingMaster()
	f.srv.RegisterTTY("run-live", master, nil, 0)
	t.Cleanup(func() { _ = master.Close() })
	conn, _, err := f.dialTTY("run-live", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	// Revoke in the store only: no live-connection bookkeeping is involved.
	if err := f.st.RevokeDevice(context.Background(), "dev-life"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !closesWithin(conn, 2*time.Second) {
		t.Fatal("socket of a revoked device still open")
	}
}

func TestCloseAllConnectionsClosesEverySocket(t *testing.T) {
	f := newLifeFixture(t)
	f.seedRun("run-live", string(spawner.StateRunning), "", "")
	master := newRecordingMaster()
	f.srv.RegisterTTY("run-live", master, nil, 0)
	t.Cleanup(func() { _ = master.Close() })
	tty, _, err := f.dialTTY("run-live", "")
	if err != nil {
		t.Fatalf("tty: %v", err)
	}
	defer func() { _ = tty.Close() }()
	waitFor(t, func() bool { return f.srv.wsRegistry.Len() == 1 })
	if n := f.srv.CloseAllConnections(); n != 1 {
		t.Fatalf("CloseAllConnections closed %d, want 1", n)
	}
	if !closesWithin(tty, 2*time.Second) {
		t.Fatal("socket still open after CloseAllConnections")
	}
}

// --- terminate hands back only a confirmed-dead session -------------------------------

func TestTerminateLeavesTheTranscriptAloneWhenTheRunIsStillLive(t *testing.T) {
	f := newLifeFixture(t)
	f.srv.AutoResumable = true
	f.srv.terminateWait = 300 * time.Millisecond
	f.srv.terminatePoll = 50 * time.Millisecond
	const sid = "8c5d3f7a-1111-2222-3333-444455556666"
	now := time.Now().UnixMilli()
	// A chat run whose row still reads running: terminate cannot confirm it.
	if err := f.st.UpsertManagedSession(context.Background(), &store.ManagedSession{
		ID: "run-chat", SessionID: sid, Kind: "chat", State: string(spawner.StateRunning),
		StartedAt: now, UpdatedAt: now, CWD: f.dir,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	projDir := filepath.Join(f.home, ".claude", "projects", encodeSlug(f.dir))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	transcript := filepath.Join(projDir, sid+".jsonl")
	body := []byte(`{"type":"user","entrypoint":"sdk-cli","promptSource":"sdk","sessionId":"` + sid + `"}` + "\n")
	if err := os.WriteFile(transcript, body, 0o600); err != nil {
		t.Fatalf("transcript: %v", err)
	}

	if w := f.post("/api/v1/agentd/sessions/run-chat/terminate"); w.Code != http.StatusOK {
		t.Fatalf("terminate: %d %s", w.Code, w.Body.String())
	}
	time.Sleep(f.srv.terminateWait + 500*time.Millisecond)
	got, _ := os.ReadFile(transcript)
	if !bytes.Equal(got, body) {
		t.Fatalf("transcript rewritten while the run was not confirmed dead: %s", got)
	}
	if backups, _ := filepath.Glob(transcript + ".agentflow-bak-*"); len(backups) != 0 {
		t.Fatalf("make-resumable ran: %v", backups)
	}
}

func encodeSlug(p string) string {
	var b strings.Builder
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// --- approvals ------------------------------------------------------------------------

func TestALateApprovalDecisionIsAConflict(t *testing.T) {
	f := newLifeFixture(t)
	f.srv.SetApprovals(approvals.New(f.st, nil))
	if err := f.st.UpsertApproval(context.Background(), &store.Approval{
		ID: "ap-1", ToolName: "Bash", State: store.ApprovalExpired, CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agentd/approvals/ap-1/decide", strings.NewReader(`{"decision":"allow"}`))
	req.Header.Set("Authorization", "Bearer "+lifeToken)
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("deciding an expired approval: %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if a, _ := f.st.GetApproval(context.Background(), "ap-1"); a == nil || a.State != store.ApprovalExpired {
		t.Fatalf("expired approval changed: %+v", a)
	}
}

func TestApprovalHookRequiresTheSecret(t *testing.T) {
	f := newLifeFixture(t)
	f.srv.SetApprovals(approvals.New(f.st, nil))
	f.srv.SetHookSecret([]byte("the-secret"))
	for _, secret := range []string{"", "the-secreT", "the-secret-and-more"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agentd/approvals/request", strings.NewReader(`{}`))
		if secret != "" {
			req.Header.Set("X-AgentFlow-Hook-Secret", secret)
		}
		w := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("secret %q: %d, want 401", secret, w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agentd/approvals/request", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	f.srv.PublicHandler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("hook route on the public handler: %d, want 404", w.Code)
	}
}

// --- hub -------------------------------------------------------------------------------

func TestUnsubscribeClosesTheChannelExactlyOnce(t *testing.T) {
	hub := newTTYHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
	master := newRecordingMaster()
	hub.Register("run", master, nil, 0)
	_, ch, unsub, err := hub.Subscribe("run")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	unsub()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("unexpected chunk")
		}
	case <-time.After(time.Second):
		t.Fatal("unsubscribe left the channel open: its consumer would leak")
	}
	unsub()            // idempotent
	_ = master.Close() // the read loop's cleanup must not close it again
	waitFor(t, func() bool { return !hub.Live("run") })
}

// slowMaster records writes, taking a moment for each, so interleaving shows.
type slowMaster struct {
	*recordingMaster
}

func (m slowMaster) Write(b []byte) (int, error) {
	time.Sleep(time.Millisecond)
	return m.recordingMaster.Write(b)
}

func TestConcurrentSubmitsAndWritesNeverInterleave(t *testing.T) {
	hub := newTTYHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := newRecordingMaster()
	master := &slowMaster{rec}
	hub.Register("run", master, nil, 0)
	t.Cleanup(func() { _ = rec.Close() })

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = hub.Submit("run", fmt.Sprintf("prompt-%d", i), false)
		}(i)
		go func() {
			defer wg.Done()
			_ = hub.Write("run", []byte("x"))
		}()
	}
	wg.Wait()
	writes, _ := rec.snapshot()
	for i, w := range writes {
		if strings.HasPrefix(w, "prompt-") {
			if i+1 >= len(writes) || writes[i+1] != "\r" {
				t.Fatalf("a submit's text and its carriage return were split: %q", writes)
			}
		}
	}
}

func TestRegisteringANewMasterReplacesTheOldOne(t *testing.T) {
	hub := newTTYHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
	oldMaster := newRecordingMaster()
	hub.Register("run", oldMaster, nil, 0)
	newMaster := newRecordingMaster()
	t.Cleanup(func() { _ = newMaster.Close() })
	hub.Register("run", newMaster, nil, 0)

	// The old process's read loop ending must not forget the new one.
	_ = oldMaster.Close()
	time.Sleep(100 * time.Millisecond)
	if !hub.Live("run") {
		t.Fatal("the old read loop's cleanup removed the new master")
	}
	if err := hub.Write("run", []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if w, _ := newMaster.snapshot(); len(w) != 1 || w[0] != "hello" {
		t.Fatalf("input went to the wrong master: new=%q", w)
	}
	if w, _ := oldMaster.snapshot(); len(w) != 0 {
		t.Fatalf("input reached the replaced master: %q", w)
	}
}

// --- helpers ----------------------------------------------------------------------------

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// closesWithin reports whether the server closes conn within d. Data frames
// still in flight are drained; only a read error before the deadline counts.
func closesWithin(conn *websocket.Conn, d time.Duration) bool {
	deadline := time.Now().Add(d)
	_ = conn.SetReadDeadline(deadline)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return time.Now().Before(deadline)
		}
	}
}
