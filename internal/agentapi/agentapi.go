// Package agentapi is the agentd control-plane HTTP API — the port the
// frontend (locally, and from a phone through the remote listener) uses to
// spawn / message / stop / resume claude sessions and to pair devices. The
// cross-cutting protections (security headers, Host checks, rate limits, body
// caps) are applied around it by internal/httpserve.
//
// Auth: every mutating and list endpoint requires `Authorization: Bearer
// <deviceToken>` verified hash-wise against the devices registry.
// /health and /pair/complete are the two unauthenticated seams (health leaks
// nothing; pair/complete takes the one-shot pairing token).
package agentapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/approvals"
	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/httpserve/ratelimit"
	"github.com/arthurobo/agentflow/internal/resumable"

	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
	"github.com/arthurobo/agentflow/internal/uploads"
)

// Server is the agentd control API.
type Server struct {
	st        *store.Store
	spawner   *spawner.Spawner
	log       *slog.Logger
	machineID string
	// Approvals is the remote-approval engine. The hook
	// calling /approvals/request blocks until it resolves — deny-by-default.
	Approvals *approvals.Service
	// AutoResumable, when true, rewrites a chat session's launch markers to
	// `cli`/`typed` when it is Terminated (the "hand the session back to
	// local tooling" moment) so it shows up in `claude -r` / /resume — the
	// upstream sdk-picker-filter workaround (internal/resumable).
	AutoResumable bool

	// doctorExtras is appended to the standard checks.
	doctorExtras []Check

	// tty fan-out hub (KindTTY runs).
	tty *ttyHub
	// ttyLocks serializes terminal starts per claude session id (or per run
	// id before one is known), so a reconnect storm or two viewers opening
	// the same session never start two processes for it.
	ttyLocks keyedMutex

	// pendingSpawns holds a channel per run this server is starting in the
	// background, closed when the spawner returns. Between the 202 and the
	// spawner persisting the row the run is unknown to the store, and an
	// attach in that window must wait rather than 404.
	pendingSpawns sync.Map // run id -> chan struct{}

	// wsRegistry tracks authenticated WebSocket connections so a
	// revocation can close them.
	wsRegistry *connRegistry
	// terminateWait bounds how long a terminate waits for the child to be
	// gone before giving up on making the session resumable; terminatePoll
	// is how often it looks.
	terminateWait, terminatePoll time.Duration

	// wsReverifyEvery is how often a long-lived WebSocket re-checks its
	// device token, so a revocation from any path closes it.
	wsReverifyEvery time.Duration

	// hookSecret is the constant-time check value for the PreToolUse
	// approval hook. Set by SetHookSecret before the handler is mounted;
	// nil means no hook is wired and every caller gets 401.
	hookSecret []byte

	// pairLimiter bounds pairing attempts per client address.
	pairLimiter *ratelimit.Bucket
	// pairReqs holds access requests from browsers without a pairing link.
	pairReqs *pairRequests

	// mu guards the settings below, which the daemon sets after New.
	mu sync.Mutex
	// publicOrigin reports the remote listener's origin; see SetPublicOrigin.
	publicOrigin func() string
	// listenAddr is the local listener's configured host:port.
	listenAddr string

	// uploads policy gate (paths, quotas, rate limits), built on first use.
	uploadsOnce sync.Once
	uploadStore *uploads.Store

	// Engines is the dual-engine registry. Wired by agentd at boot. Nil is
	// tolerated for backward-compat tests; engine-aware endpoints 503.
	Engines *engine.Registry
}

// New builds the agentd control API.
func New(st *store.Store, sp *spawner.Spawner, log *slog.Logger, machineID string) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		st: st, spawner: sp, log: log, machineID: machineID,
		tty:             newTTYHub(log),
		wsRegistry:      newConnRegistry(),
		wsReverifyEvery: time.Minute,
		terminateWait:   2 * time.Minute,
		terminatePoll:   time.Second,
		// A phone scanning a QR code has a handful of legitimate attempts; a
		// brute force of the pairing endpoint has tens of thousands.
		pairLimiter: ratelimit.NewBucket(5.0/60.0, 5),
		pairReqs:    newPairRequests(),
	}
}

// SetHookSecret installs the PreToolUse hook secret. The daemon reads
// dataDir/hook.secret at first start; an empty file disables the hook
// endpoint (the public handler never mounts it).
func (s *Server) SetHookSecret(b []byte) { s.hookSecret = b }

// SetApprovals wires the approvals engine. The handler
// reads s.Approvals on each request; tests use this to attach a
// minimal approvals.New(store, log) without going through the
// spawner / couple API.
func (s *Server) SetApprovals(a *approvals.Service) { s.Approvals = a }

// TTYWrite injects bytes into a run's PTY master.
//
// Exposed for the nudge, which is an ACCELERATOR and nothing else: it carries
// one line, never a message body, and only for runs this agentd owns. Every
// delivery guarantee holds with it deleted.
func (s *Server) TTYWrite(runID string, b []byte) error {
	return s.tty.Write(runID, b)
}

// TTYSubmit sends one composed prompt to a run's PTY, text then carriage
// return, with the pacing the composer uses (see ttySubmit).
//
// Unlike TTYWrite this is NOT an accelerator: the courier delivers message
// bodies through it, so a member never has to block on a poll to hear
// anything. It returns an error when the run has no live PTY here, which is
// how the courier knows to leave the mail on the queue instead of writing it
// into nothing.
func (s *Server) TTYSubmit(runID, text string) error {
	return s.tty.Submit(runID, text, s.tty.BracketedPaste(runID))
}

// Handler returns the control mux for the local listener.
//
// The local mux and the public mux share the SAME `s` so the connection
// registry, rate limiter and hook secret work identically on both sides.
func (s *Server) Handler() http.Handler {
	return s.handler(false)
}

// PublicHandler is the control mux for the remote listener: the approvals
// hook is not mounted and /health is minimal.
func (s *Server) PublicHandler() http.Handler {
	return s.handler(true)
}

func (s *Server) handler(public bool) http.Handler {
	mux := http.NewServeMux()
	// /health is always mounted; the public mux returns the minimal
	// shape (no relay/sessions diagnostics, see handleHealthPublic).
	mux.HandleFunc("GET /api/v1/agentd/health", s.withLog(s.healthHandler(public)))
	mux.HandleFunc("POST /api/v1/agentd/pair/complete", s.withLog(s.handlePairComplete))
	mux.HandleFunc("POST /api/v1/agentd/pair/request", s.withLog(s.handlePairRequestCreate))
	mux.HandleFunc("GET /api/v1/agentd/pair/request/{id}", s.withLog(s.handlePairRequestPoll))
	// Approving an access request from a browser is only offered on the
	// local listener: whoever approves must be at this computer, which a
	// paired phone on the public URL need not be.
	if !public {
		mux.HandleFunc("GET /api/v1/agentd/pair/requests", s.requireAuth(s.withLog(s.handlePairRequestsList)))
		mux.HandleFunc("POST /api/v1/agentd/pair/requests/{id}/approve", s.requireAuth(s.withLog(s.pairRequestDecider(true))))
		mux.HandleFunc("POST /api/v1/agentd/pair/requests/{id}/deny", s.requireAuth(s.withLog(s.pairRequestDecider(false))))
	}
	mux.HandleFunc("GET /api/v1/agentd/devices", s.requireAuth(s.withLog(s.handleDevices)))
	mux.HandleFunc("POST /api/v1/agentd/devices/{id}/revoke", s.requireAuth(s.withLog(s.handleRevokeDevice)))
	mux.HandleFunc("GET /api/v1/agentd/runs/by-session/{sid}", s.requireAuth(s.withLog(s.handleRunBySession)))
	mux.HandleFunc("GET /api/v1/agentd/sessions", s.requireAuth(s.withLog(s.handleListSessions)))
	mux.HandleFunc("GET /api/v1/agentd/all-sessions", s.requireAuth(s.withLog(s.handleAllSessions)))
	mux.HandleFunc("POST /api/v1/agentd/sessions", s.requireAuth(s.withLog(s.handleSpawn)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/resume", s.requireAuth(s.withLog(s.handleResume)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}", s.requireAuth(s.withLog(s.handleStatus)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/message", s.requireAuth(s.withLog(s.handleMessage)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/stop", s.requireAuth(s.withLog(s.handleStop)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/terminate", s.requireAuth(s.withLog(s.handleTerminate)))
	// tty runs: idempotent start, byte-stream WS, stop. The WS does its own
	// auth (upgrades can't set headers — ?token=).
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/tty", s.requireAuth(s.withLog(s.handleTTYStart)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/by-session/{sid}/tty", s.requireAuth(s.withLog(s.handleTTYBySession)))
	mux.HandleFunc("DELETE /api/v1/agentd/sessions/{id}/tty", s.requireAuth(s.withLog(s.handleTTYStop)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/tty/ws", s.withLog(s.handleTTYWS))
	// Image attachments upload over this authenticated POST — the same
	// channel as every other request, no peer-to-peer path.
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/uploads", s.requireAuth(s.withLog(s.handleUpload)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/control/prompt", s.requireAuth(s.withLog(s.handleControlPrompt)))
	mux.HandleFunc("GET /api/v1/agentd/doctor", s.requireAuth(s.withLog(s.handleDoctor)))
	mux.HandleFunc("GET /api/v1/agentd/cwds", s.requireAuth(s.withLog(s.handleCwds)))

	// Approvals: the hook endpoint is local-only because it
	// must not be reachable from a Funnel URL — the URL is in the
	// public Certificate Transparency logs. Decide/list/policies are
	// device-authed and intentionally mounted on both.
	if !public {
		mux.HandleFunc("POST /api/v1/agentd/approvals/request", s.withLog(s.handleApprovalRequest))
	}
	mux.HandleFunc("POST /api/v1/agentd/approvals/{id}/decide", s.requireAuth(s.withLog(s.handleApprovalDecide)))
	mux.HandleFunc("GET /api/v1/agentd/approvals", s.requireAuth(s.withLog(s.handleApprovalList)))
	mux.HandleFunc("GET /api/v1/agentd/policies", s.requireAuth(s.withLog(s.handlePoliciesList)))
	mux.HandleFunc("POST /api/v1/agentd/policies", s.requireAuth(s.withLog(s.handlePoliciesUpsert)))
	mux.HandleFunc("DELETE /api/v1/agentd/policies/{id}", s.requireAuth(s.withLog(s.handlePoliciesDelete)))

	// Dual-engine control layer.
	mux.HandleFunc("GET /api/v1/agentd/engines", s.requireAuth(s.withLog(s.handleListEngines)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/control/caps", s.requireAuth(s.withLog(s.handleControlCaps)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/control/state", s.requireAuth(s.withLog(s.handleControlState)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/control/models", s.requireAuth(s.withLog(s.handleControlModels)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/control/model", s.requireAuth(s.withLog(s.handleControlSetModel)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/control/agents", s.requireAuth(s.withLog(s.handleControlAgents)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/control/agent", s.requireAuth(s.withLog(s.handleControlSetAgent)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/control/providers", s.requireAuth(s.withLog(s.handleControlProviders)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/control/provider", s.requireAuth(s.withLog(s.handleControlSetProvider)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/control/mcp", s.requireAuth(s.withLog(s.handleControlMCP)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/control/skills", s.requireAuth(s.withLog(s.handleControlSkills)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/control/commands", s.requireAuth(s.withLog(s.handleControlCommands)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/control/command", s.requireAuth(s.withLog(s.handleControlRunCommand)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/control/permissions", s.requireAuth(s.withLog(s.handleControlPermissions)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/control/permission", s.requireAuth(s.withLog(s.handleControlReplyPermission)))
	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/control/questions", s.requireAuth(s.withLog(s.handleControlQuestions)))
	mux.HandleFunc("POST /api/v1/agentd/sessions/{id}/control/question", s.requireAuth(s.withLog(s.handleControlReplyQuestion)))

	mux.HandleFunc("GET /api/v1/agentd/sessions/{id}/config", s.requireAuth(s.withLog(s.handleSessionConfig)))
	mux.HandleFunc("POST /api/v1/agentd/pair/cookie", s.requireAuth(s.withLog(s.handlePairCookie)))
	if public {
		return cors(onPublicListener(mux))
	}
	return cors(mux)
}

// healthHandler returns the right health handler for the local or public
// listener. The public one says only that the daemon is up: nothing about
// the machine, its sessions or the agentflow version (which would tell a
// visitor which known bugs to try).
func (s *Server) healthHandler(public bool) http.HandlerFunc {
	if public {
		return func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		}
	}
	return s.handleHealth
}

// --- auth -----------------------------------------------------------------------

type ctxKey int

const deviceKey ctxKey = 0

// requireAuth verifies the Bearer device token through VerifyClientDevice
// (kind=device, status=active): pairing tokens, machine tokens and revoked
// rows all return 401.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r.Header.Get("Authorization"))
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		d, err := s.st.VerifyClientDevice(r.Context(), tok)
		if err != nil || d == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or revoked device token")
			return
		}
		_ = s.st.TouchDevice(r.Context(), d.ID)
		ctx := context.WithValue(r.Context(), deviceKey, d)
		next(w, r.WithContext(ctx))
	}
}

func deviceFrom(ctx context.Context) *store.Device {
	d, _ := ctx.Value(deviceKey).(*store.Device)
	return d
}

func bearer(h string) string {
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// --- helpers ----------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

// cors is off unless AF_DEV_CORS_ORIGIN names the one extra origin allowed
// (the `next dev` server while developing the web UI). The UI is embedded and
// same-origin, so no other origin ever needs to call the API.
func cors(next http.Handler) http.Handler {
	origin := devCORSOrigin()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, PUT, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-AgentFlow-*")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func devCORSOrigin() string { return strings.TrimSuffix(os.Getenv("AF_DEV_CORS_ORIGIN"), "/") }

func (s *Server) withLog(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingWriter{ResponseWriter: w, status: 200}
		next(lw, r)
		s.log.Info("agentd-api", "method", r.Method, "path", r.URL.Path,
			"status", lw.status, "dur_ms", time.Since(start).Milliseconds())
	}
}

type loggingWriter struct {
	http.ResponseWriter
	status int
}

func (lw *loggingWriter) WriteHeader(code int) {
	lw.status = code
	lw.ResponseWriter.WriteHeader(code)
}

// Hijack keeps WebSocket upgrades working through the logging middleware
// (gorilla/websocket requires an http.Hijacker).
func (lw *loggingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := lw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hijack not supported by %T", lw.ResponseWriter)
	}
	return h.Hijack()
}

func newSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b)
}

// --- handlers ----------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	claudeVersion := s.spawner.Version(r.Context())
	sess, err := s.spawner.List(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	running := 0
	for _, sc := range sess {
		if !sc.State.Terminal() {
			running++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"machineId": s.machineID,
		"claude":    claudeVersion,
		"sessions":  map[string]int{"running": running, "total": len(sess)},
	})
}

// handlePairComplete registers a device from a one-shot pairing token
// (issued by `agentflow pair`). Each completion mints a fresh device token
// (only its hash is stored); the pairing token itself is consumed atomically
// so a re-scan of the same QR returns 401. Attempts are limited to 5 a minute
// per client address.
func (s *Server) handlePairComplete(w http.ResponseWriter, r *http.Request) {
	if !s.pairLimiter.Allow(ratelimit.ClientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "pair rate exceeded; try again in a minute")
		return
	}
	var body struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.Token == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing token")
		return
	}
	verifyStart := time.Now()
	pair, err := s.st.VerifyDevice(r.Context(), body.Token)
	if err != nil || pair == nil {
		writeError(w, http.StatusUnauthorized, "invalid_pairing", "pairing token invalid or expired")
		return
	}
	s.log.Debug("pair: token verified", "ms", time.Since(verifyStart).Milliseconds())
	// Only one-shot `kind=pairing` tokens drive a pair.
	if pair.Kind != "pairing" {
		writeError(w, http.StatusUnauthorized, "invalid_pairing", "pairing token invalid or expired")
		return
	}

	// One-shot pairing: atomically mark the token consumed BEFORE we mint
	// the device. A second pair call against the same QR either raced us
	// (ConsumePairingToken returns false and we 401) or arrived after
	// expiry (VerifyDevice would already have rejected).
	consumed, cerr := s.st.ConsumePairingToken(r.Context(), pair.ID)
	if cerr != nil {
		writeError(w, http.StatusInternalServerError, "db_error", cerr.Error())
		return
	}
	if !consumed {
		// Lost the race to a concurrent pair — refuse rather than mint
		// a duplicate device under a token the operator assumed was
		// one-shot.
		writeError(w, http.StatusUnauthorized, "invalid_pairing",
			"pairing token already used (one-shot; re-run `agentflow pair` for another device)")
		return
	}

	name := cleanDeviceName(body.Name)
	if strings.TrimSpace(body.Name) == "" {
		name = "device-" + pair.ID[:min(8, len(pair.ID))]
	}
	devID, deviceToken, err := s.registerDevice(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	setDeviceCookie(w, r, deviceToken)

	// Audit-log every successful pairing so the operator can answer "who
	// paired with this machine, and when?". The client IP is the real
	// client behind the Funnel relay on the remote listener, loopback
	// locally.
	s.log.Info("pair: device registered",
		"deviceId", devID, "name", name, "clientIp", ratelimit.ClientIP(r),
		"machineId", s.machineID, "userAgent", r.UserAgent())

	writeJSON(w, http.StatusCreated, map[string]any{
		"deviceId":    devID,
		"deviceToken": deviceToken, // returned exactly once; only its hash is stored
		"machineId":   s.machineID,
		"expiresAt":   0,
	})
}

// registerDevice mints a device token for a newly trusted device and stores
// its hash. The token is returned to the caller, who hands it out exactly
// once.
func (s *Server) registerDevice(ctx context.Context, name string) (id, token string, err error) {
	token = newSecret(24)
	id = newSecret(10)
	dev := &store.Device{
		ID: id, Name: name, MachineID: s.machineID, Kind: "device",
		Status: "active", TokenHash: store.HashToken(token),
	}
	// The agentd's opencode ingest writer can hold a long-running insert
	// transaction (1500+ rows with FTS5 triggers), blocking the single
	// SQLite writer lock. SQLite's busy_timeout (5s) then expires, the
	// device upsert returns SQLITE_BUSY, and the phone shows "no
	// connected machine knows this device token" (or worse, a generic
	// 500). Retry with backoff so a brief contention doesn't surface as
	// a pair failure to the user.
	upsertStart := time.Now()
	if err := s.upsertDeviceWithRetry(ctx, dev, 0); err != nil {
		return "", "", err
	}
	s.log.Debug("pair: device row written", "ms", time.Since(upsertStart).Milliseconds())
	return id, token, nil
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.st.ListDevices(r.Context(), "device", 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(devs))
	for _, d := range devs {
		out = append(out, map[string]any{
			"id": d.ID, "name": d.Name, "machineId": d.MachineID,
			"status": d.Status, "createdAt": d.CreatedAt, "lastSeen": d.LastSeen,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing device id")
		return
	}
	closed, err := s.RevokeDevice(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": id, "connectionsClosed": closed})
}

// spawnRequest is the POST /sessions body.
type spawnRequest struct {
	Kind             string   `json:"kind"`
	Prompt           string   `json:"prompt"`
	Title            string   `json:"title,omitempty"`
	Model            string   `json:"model,omitempty"`
	Agent            string   `json:"agent,omitempty"`
	Engine           string   `json:"engine,omitempty"`
	Cwd              string   `json:"cwd,omitempty"`
	Project          string   `json:"project,omitempty"`
	AllowedTools     []string `json:"allowedTools,omitempty"`
	PermissionMode   string   `json:"permissionMode,omitempty"`
	Effort           string   `json:"effort,omitempty"`
	ResumeSessionID  string   `json:"resumeSessionId,omitempty"`
	ResumeTitle      string   `json:"resumeTitle,omitempty"`
	AddDirs          []string `json:"addDirs,omitempty"`
	ApprovalsEnabled bool     `json:"approvalsEnabled,omitempty"`
	// PromptMode selects the standing text a solo session carries:
	// "standing" (the default) or "scratch". Scratch is NOT off: it drops the
	// advisory rules and keeps the binding ones, because someone reaching for
	// scratch wants to skip the reporting contract on a throwaway, not to
	// authorise a push to main.
	PromptMode string `json:"promptMode,omitempty"`
}

func (r *spawnRequest) options(createdBy string) spawner.Options {
	kind := spawner.KindChat
	switch r.Kind {
	case "one_shot", "oneshot":
		kind = spawner.KindOneShot
	case "tty":
		kind = spawner.KindTTY
	}
	return spawner.Options{
		Kind: kind, Prompt: r.Prompt, Title: r.Title, Model: r.Model, Agent: r.Agent,
		Cwd: r.Cwd, Project: r.Project,
		AllowedTools: r.AllowedTools, PermissionMode: r.PermissionMode,
		Effort: r.Effort, ResumeSessionID: r.ResumeSessionID, ResumeTitle: r.ResumeTitle,
		AddDirs: r.AddDirs, CreatedBy: createdBy, InitTimeout: 20 * time.Second,
		ApprovalsEnabled: r.ApprovalsEnabled,
		Engine:           engine.ResolveID(r.Engine),
	}
}

// prepareEngine fills the engine-specific spawn knobs on opts, and is the
// ONLY place that does so.
//
// It exists because it did not. handleSpawn resolved the engine and allocated
// OpenCode's control port; the three resume paths — ensureTTY,
// handleTTYBySession and handleResume — each built their own Options and set
// only Cwd and Model. An OpenCode run resumed through any of them therefore
// carried an empty Engine, resolved to claude by default, and ran
// `claude --resume ses_...`, which is a session id Claude has never heard of.
// What the engineer saw was a blank terminal and Claude's own resume picker
// saying "No sessions match ses_f88c4e99...".
//
// Called BEFORE the spawner is invoked: the port has to be on the row and in
// the argv, and the spawner is what builds the argv.
func (s *Server) prepareEngine(ctx context.Context, opts *spawner.Options) error {
	if s.Engines == nil {
		return nil
	}
	eng, err := s.Engines.Get(opts.Engine)
	if err != nil {
		return err
	}
	if !eng.NeedsControlPort() {
		return nil
	}
	allocator, ok := eng.(interface {
		AllocateAndPrepare(ctx context.Context, cwd, title string) (port int, base string, sessionID string, err error)
	})
	if !ok {
		return errors.New("engine " + opts.Engine + " requires port allocation but exposes no allocator")
	}
	port, base, sid, err := allocator.AllocateAndPrepare(ctx, opts.Cwd, opts.Title)
	if err != nil {
		return err
	}
	opts.ControlPort = port
	opts.ControlBase = base
	// Only a FRESH spawn adopts a pre-created session id. On a resume the
	// session already exists and the id to bind is the one being resumed;
	// overwriting it here would point the row at a session the child was
	// never told to open.
	if opts.ResumeSessionID == "" {
		opts.ControlSessionID = sid
	}
	return nil
}

// handleSpawn starts a run in the background and answers 202 with the stable
// run key; the run view streams it live via the shared outbox/WS and polls
// /sessions/{id} for the claude session id + state.
//
// Engine-aware (Plan / migration 0015): when the requested engine is
// OpenCode, the engine registry is consulted, a control port is allocated
// and pre-bound to the engine session id via POST /session BEFORE the TUI
// process starts. Claude keeps its existing argv + corpus-discovery path.
func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var body spawnRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "bad json body")
		return
	}
	if strings.TrimSpace(body.Prompt) == "" && body.ResumeSessionID == "" && body.ResumeTitle == "" && body.Kind != "tty" {
		writeError(w, http.StatusBadRequest, "bad_request", "prompt or resume target required")
		return
	}
	dev := deviceFrom(r.Context())
	createdBy := "local"
	if dev != nil {
		createdBy = "device:" + dev.ID
	}
	opts := body.options(createdBy)
	runID := spawner.NewRunID()

	// Pre-allocate a control port + pre-create the OpenCode session when the
	// requested engine exposes one (the spawner is responsible for argv
	// shaping and lifecycle — this layer's job is just to fill the
	// engine-specific knobs BEFORE the spawner is invoked).
	if err := s.prepareEngine(r.Context(), &opts); err != nil {
		writeError(w, http.StatusBadRequest, "engine_prepare_failed", err.Error())
		return
	}

	// A run-form session is not a loop and still needs its standing rules.
	// A failure here refuses THIS spawn with a clear message rather than
	// starting an agent that never saw them.
	opts, carrier, err := attachStandingPrompt(r.Context(), s.st, body.PromptMode, opts)
	if err != nil {
		writeError(w, http.StatusBadRequest, "standing_prompt_failed", err.Error())
		return
	}
	s.log.Info("agentd: standing prompt attached", "run", runID,
		"engine", opts.Engine, "mode", store.NormalisePromptMode(body.PromptMode), "carrier", carrier)

	s.startInBackground(runID, opts)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":          runID,
		"state":       string(spawner.StateStarting),
		"kind":        firstNonEmpty(body.Kind, string(spawner.KindChat)),
		"prompt":      body.Prompt,
		"engine":      opts.Engine,
		"controlPort": opts.ControlPort,
		"controlBase": opts.ControlBase,
	})
}

// startInBackground starts a run without waiting for it, recording it as
// pending until the spawner returns (see pendingSpawns).
func (s *Server) startInBackground(runID string, opts spawner.Options) {
	done := make(chan struct{})
	s.pendingSpawns.Store(runID, done)
	go func() {
		defer func() {
			s.pendingSpawns.Delete(runID)
			close(done)
		}()
		_, _ = s.spawner.StartWithID(context.Background(), runID, opts)
	}()
}

// waitPendingSpawn blocks until a background start of runID has returned, if
// one is in flight, or ctx ends.
func (s *Server) waitPendingSpawn(ctx context.Context, runID string) {
	v, ok := s.pendingSpawns.Load(runID)
	if !ok {
		return
	}
	select {
	case <-v.(chan struct{}):
	case <-ctx.Done():
	}
}

// handleResume resumes a terminal (or live) run via --resume <sessionId> into
// a fresh run key.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, err := s.spawner.Status(r.Context(), id)
	if err != nil || cur == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if cur.SessionID == "" {
		writeError(w, http.StatusBadRequest, "not_resumable", "run never captured a claude session id")
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	opts := spawner.Options{
		Kind: cur.Kind, Cwd: cur.CWD, Project: cur.Project, Model: cur.Model,
		Prompt: body.Prompt, ResumeSessionID: cur.SessionID,
		// The engine the run WAS. Resuming an OpenCode session as Claude
		// hands Claude a session id it has never seen.
		Engine:           cur.Engine,
		Title:            cur.Title,
		ApprovalsEnabled: cur.ApprovalsEnabled,
		AllowedTools:     nil, PermissionMode: "acceptEdits", InitTimeout: 20 * time.Second,
	}
	if err := s.prepareEngine(r.Context(), &opts); err != nil {
		writeError(w, http.StatusBadRequest, "engine_prepare_failed", err.Error())
		return
	}
	dev := deviceFrom(r.Context())
	createdBy := "local"
	if dev != nil {
		createdBy = "device:" + dev.ID
	}
	opts.CreatedBy = createdBy
	newID := spawner.NewRunID()
	s.startInBackground(newID, opts)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": newID, "state": string(spawner.StateStarting),
		"resumeFrom": cur.SessionID, "kind": cur.Kind,
	})
}

// handleRunBySession is the session→run bridge (watch↔control): given a claude
// session id, return the managed run that owns it (or 404 when it's not a
// managed run / never captured a session id).
func (s *Server) handleRunBySession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.spawner.ListBySessionID(r.Context(), r.PathValue("sid"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, "not_found", "no managed run for this session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": sess})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	sess, err := s.spawner.List(r.Context(), state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.fillSessionTitles(r.Context(), sess)
	writeJSON(w, http.StatusOK, map[string]any{"items": sess})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	sess, err := s.spawner.Status(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	// A loop member's session is not an anonymous terminal. Its title lived
	// only in memory, in the --name argv and in the corpus title record, so
	// the page that shows the terminal called every member "Terminal session"
	// and its subtitle showed the loop cwd, which is identical for all of
	// them. The row that knows better is loop_members, joined on run id.
	// The header on the run page reads this. Without it a resumed run was
	// titled "resumed 290d8…", which is the run key and not the name anyone
	// gave the session — the same gap the runs LIST had, fixed the same way
	// and from the same source, so the two never disagree.
	s.fillSessionTitles(r.Context(), []*spawner.Session{sess})

	out := map[string]any{"session": sess}
	if member, loop, err := s.st.LoopMemberByRunID(r.Context(), sess.ID); err == nil && member != nil {
		out["loop"] = map[string]any{
			"id": loop.ID, "title": loop.Title, "task": loop.Task,
			"role": member.Role, "play": loop.Play, "stepId": loop.StepID,
			"health": memberHealthState(r.Context(), s.st, loop.ID, member.ID),
		}
	}
	writeJSONMerged(w, sess, out)
}

// memberHealthState reads one member's verdict, or "" when it cannot.
func memberHealthState(ctx context.Context, st *store.Store, loopID, memberID string) string {
	rows, err := st.LoopHealth(ctx, loopID)
	if err != nil {
		return ""
	}
	for _, h := range rows {
		if h.MemberID == memberID {
			return h.State
		}
	}
	return ""
}

// writeJSONMerged answers the session object with extra top-level keys.
//
// The session shape is what every existing caller parses, so the loop context
// is added ALONGSIDE it rather than nesting it: an older client sees exactly
// what it saw before and ignores the new key.
func writeJSONMerged(w http.ResponseWriter, sess any, extra map[string]any) {
	raw, err := json.Marshal(sess)
	if err != nil {
		writeJSON(w, http.StatusOK, sess)
		return
	}
	merged := map[string]any{}
	if err := json.Unmarshal(raw, &merged); err != nil {
		writeJSON(w, http.StatusOK, sess)
		return
	}
	for k, v := range extra {
		if k == "session" {
			continue
		}
		merged[k] = v
	}
	writeJSON(w, http.StatusOK, merged)
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "text required")
		return
	}
	if err := s.spawner.SendMessage(r.Context(), id, body.Text); err != nil {
		writeError(w, http.StatusConflict, "send_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "session": id})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.spawner.Interrupt(r.Context(), id, os.Interrupt); err != nil {
		writeError(w, http.StatusConflict, "stop_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interrupted": true})
}

func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.spawner.CloseStdin(r.Context(), id); err != nil {
		writeError(w, http.StatusConflict, "terminate_failed", err.Error())
		return
	}
	// Hand-back convenience: once the claude child has actually exited,
	// rewrite the session's launch markers so it appears in `claude -r`.
	// Best-effort + logged; never blocks the terminate response.
	if s.AutoResumable {
		// The rewrite outlives the request, so it keeps the request's values
		// but not its cancellation.
		bg := context.WithoutCancel(r.Context())
		go func() {
			cur, err := s.spawner.Status(bg, id)
			if err != nil || cur == nil || cur.SessionID == "" {
				return
			}
			sid := cur.SessionID
			// Wait until the process is fully gone before touching the file.
			// If that is never confirmed, leave the transcript alone: the
			// child may still be appending to it, and rewriting it under a
			// live writer corrupts the session.
			confirmed := false
			for deadline := time.Now().Add(s.terminateWait); ; {
				c, _ := s.spawner.Status(bg, id)
				if c == nil || c.State.Terminal() {
					if err := resumable.RequireNotLive(sid); err == nil {
						confirmed = true
						break
					}
				}
				if !time.Now().Before(deadline) {
					break
				}
				time.Sleep(s.terminatePoll)
			}
			if !confirmed {
				s.log.Warn("agentd: terminated run still live; not making it resumable",
					"run", id, "session", sid)
				return
			}
			if rep, err := resumable.MakeResumable(s.st, id); err != nil {
				s.log.Warn("agentd: auto make-resumable", "run", id, "session", sid, "err", err)
			} else {
				s.log.Info("agentd: terminated run made resumable locally",
					"run", id, "session", sid,
					"entrypoint", rep.RewrittenEntry, "promptSource", rep.RewrittenPrompt,
					"backup", rep.Backup)
			}
		}()
	}
	writeJSON(w, http.StatusOK, map[string]any{"terminated": true})
}

// handleDoctor runs the checks as an HTTP surface (agentd doctor
// is the CLI entry; this shares the same checks).
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	checks := s.doctorChecks(r.Context())
	allOK := true
	for _, c := range checks {
		if !c.OK {
			allOK = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": allOK, "checks": checks})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// upsertDeviceWithRetry retries UpsertDevice on SQLITE_BUSY. The agentd
// shares its SQLite writer lock with the dashboard's eventpipe and its
// own opencode ingest writer; during a backfill or a hot ingest, the
// writer can hold the lock for 5-15s (FTS5 trigger on the events table
// is the heavy lift). A bare UpsertDevice returns SQLITE_BUSY after the
// 5s busy_timeout, which the pair endpoint surfaces as a confusing
// "no connected machine knows this device token" 500. The pair request
// is interactive and the user can wait; 8 retries over 5s of
// exponential backoff is the right shape — it absorbs a typical ingest
// burst without making the user stare at a spinner.
func (s *Server) upsertDeviceWithRetry(ctx context.Context, dev *store.Device, expiresAt int64) error {
	var err error
	start := time.Now()
	// pair/control endpoints need to survive a long-running opencode
	// backfill on the same writer. The backfill is 17 sessions × ~90 parts
	// each, with one FTS5 trigger fire per row inside a single shared
	// transaction. SQLite's busy_timeout is already 30s; on busy, that
	// 30s is enough for the backfill's batches (16 events/batch, each
	// batch <100ms with 16 FTS5 fires) to release the lock. We retry
	// up to 20 times with exponential backoff so a long backfill that
	// holds the lock for ~3s doesn't surface as a 500 to the user.
	for attempt := 0; attempt < maxDeviceUpsertAttempts; attempt++ {
		err = s.st.UpsertDevice(ctx, dev, expiresAt)
		if err == nil {
			if attempt > 0 {
				s.log.Debug("pair: device upsert retried",
					"attempts", attempt+1, "ms", time.Since(start).Milliseconds())
			}
			return nil
		}
		if !strings.Contains(err.Error(), "SQLITE_BUSY") &&
			!strings.Contains(err.Error(), "database is locked") {
			return err
		}
		// Exponential from 10ms, capped at 100ms. The old 200/400/600…
		// ladder cost most of a second before the second try, which is a
		// long time to spend on a lock the busy handler is already waiting
		// on — SQLite's own busy_timeout (30s) does the real waiting, and
		// this loop only exists for the cases it does not cover.
		wait := time.Duration(10<<uint(attempt)) * time.Millisecond
		if wait > maxDeviceUpsertBackoff {
			wait = maxDeviceUpsertBackoff
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return err
}

// Retry budget for the device upsert. SQLite's busy_timeout already waits
// out a held write lock, so this loop is for the errors it does not cover
// (snapshot conflicts) — which resolve immediately or not at all. Twelve
// attempts on a 10ms→100ms ladder is about a second of trying, spent in
// tens of milliseconds rather than hundreds.
const (
	maxDeviceUpsertAttempts = 12
	maxDeviceUpsertBackoff  = 100 * time.Millisecond
)
