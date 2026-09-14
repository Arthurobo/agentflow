// Package loopapi is the ENGINEER-facing surface: create a loop, watch it,
// staff it, end it, and talk to it. It runs on the existing device-token auth
// , deliberately a different class from the member tokens the
// agents carry, so neither surface can be reached with the other's credential.
//
// The engineer is a participant rather than a side channel. Typing at a member
// is a row from ENGINEER landing in the inbox that member is already polling,
// and it is mirrored to the orchestrator so nothing happens behind it.
package loopapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/arthurobo/agentflow/internal/mailapi"
	"github.com/arthurobo/agentflow/internal/store"
)

// Prefix is the route prefix for every engineer endpoint.
const Prefix = "/api/v1/loops"

// ToolsPrefix serves the SAME code-tool registry as Prefix+"/tools" from a
// name that does not claim loops own it. The registry is the only
// session-independent list of Claude model names in the product: the control
// layer's Models() is session-scoped and returns ErrUnsupported for claude, so
// anything spawning an agent needs this list and none of them is a loop.
// Prefix+"/tools" keeps working; this is an addition, not a move.
const ToolsPrefix = "/api/v1/tools"

// PromptsPrefix is the override surface: the engineer's edits to the shipped
// prompts. Machine-scoped, so any paired device edits the same set.
const PromptsPrefix = "/api/v1/prompts"

// Config is fixed at construction, like mailapi's: these are read by live
// handlers and a settable knob would be a race.
type Config struct {
	// BaseURL is where an agent reaches the member surface from. It is
	// baked into the prompts handed out at creation.
	BaseURL string
	// Generation is the agentd boot that owns the loops this surface
	// creates. Every loop born here carries it, which
	// is what lets the NEXT boot tell this life's loops from a dead one's.
	// Empty means the deployment does not stamp, and such loops are reaped
	// on the following restart exactly like the ones that predate the
	// column.
	Generation string
}

// Server is the engineer surface.
type Server struct {
	st     *store.Store
	agents *mailapi.Server
	log    *slog.Logger
	cfg    Config
	// models answers "what can this tool run right now". Nil means only the
	// registry's static answer is available.
	models ModelLister
	// launcher starts members. Nil means this deployment does not spawn: the
	// loop is still created and the engineer still gets a prompt per member to
	// paste, which is exactly how the feature worked before.
	launcher Launcher
}

// SetModelLister wires the live model source. Called once at boot.
func (s *Server) SetModelLister(m ModelLister) { s.models = m }

// SetLauncher wires the process launcher. Called once at boot, before the
// server handles anything.
func (s *Server) SetLauncher(l Launcher) { s.launcher = l }

// New builds the engineer surface. agents renders the prompts handed out at
// creation and rotation.
func New(st *store.Store, agents *mailapi.Server, log *slog.Logger, cfg Config) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{st: st, agents: agents, log: log, cfg: cfg}
}

// Handler mounts every engineer route.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Prefix+"/tools", s.auth(s.handleTools))
	mux.HandleFunc("GET "+ToolsPrefix, s.auth(s.handleTools))
	mux.HandleFunc("GET "+ToolsPrefix+"/{id}/models", s.auth(s.handleToolModels))
	mux.HandleFunc("GET "+PromptsPrefix, s.auth(s.handlePromptsGet))
	mux.HandleFunc("PUT "+PromptsPrefix, s.auth(s.handlePromptsPut))
	mux.HandleFunc("DELETE "+PromptsPrefix+"/{id}", s.auth(s.handlePromptsDelete))
	mux.HandleFunc("GET "+Prefix+"/plays", s.auth(s.handlePlays))
	mux.HandleFunc("POST "+Prefix, s.auth(s.handleCreate))
	mux.HandleFunc("GET "+Prefix, s.auth(s.handleList))
	mux.HandleFunc("GET "+Prefix+"/{id}", s.auth(s.handleGet))
	mux.HandleFunc("POST "+Prefix+"/{id}/end", s.auth(s.handleEnd))
	mux.HandleFunc("POST "+Prefix+"/{id}/dismiss", s.auth(s.handleDismiss))
	mux.HandleFunc("POST "+Prefix+"/{id}/continue", s.auth(s.handleContinue))
	mux.HandleFunc("POST "+Prefix+"/{id}/members", s.auth(s.handleAddMember))
	mux.HandleFunc("POST "+Prefix+"/{id}/members/{role}/retire", s.auth(s.handleRetire))
	mux.HandleFunc("POST "+Prefix+"/{id}/members/{role}/rotate", s.auth(s.handleRotate))
	mux.HandleFunc("GET "+Prefix+"/{id}/messages", s.auth(s.handleMessages))
	mux.HandleFunc("GET "+Prefix+"/{id}/notes", s.auth(s.handleNotes))
	mux.HandleFunc("GET "+Prefix+"/{id}/refusals", s.auth(s.handleRefusals))
	mux.HandleFunc("POST "+Prefix+"/{id}/say", s.auth(s.handleSay))
	return mux
}

// auth verifies the device token through the same gate agentapi uses: only an
// active, paired device (kind=device) passes. A pending pairing token, a
// member token or a revoked device does not.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing bearer token", nil)
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing bearer token", nil)
			return
		}
		d, err := s.st.VerifyClientDevice(r.Context(), token)
		if err != nil || d == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or revoked device token", nil)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string, details any) {
	body := map[string]any{"code": code, "message": msg}
	if details != nil {
		body["details"] = details
	}
	writeJSON(w, status, map[string]any{"error": body})
}

func writeStoreErr(w http.ResponseWriter, err error) {
	var violation *store.StepViolation
	if errors.As(err, &violation) {
		writeErr(w, http.StatusConflict, "step_violation", violation.Error(), violation)
		return
	}
	var capped *store.RoundCapReached
	if errors.As(err, &capped) {
		writeErr(w, http.StatusConflict, "round_cap_reached", capped.Error(), capped)
		return
	}
	switch {
	case errors.Is(err, store.ErrInstructionTooThin):
		writeErr(w, http.StatusBadRequest, "instruction_too_thin", err.Error(), nil)
	case errors.Is(err, store.ErrUnknownTool):
		writeErr(w, http.StatusBadRequest, "unknown_tool", err.Error(), nil)
	case errors.Is(err, store.ErrInvalidRole):
		writeErr(w, http.StatusBadRequest, "invalid_role", err.Error(), nil)
	case errors.Is(err, store.ErrPlayInvalid):
		writeErr(w, http.StatusBadRequest, "invalid_play", err.Error(), nil)
	case errors.Is(err, store.ErrMailRouteRefused):
		writeErr(w, http.StatusForbidden, "route_refused", err.Error(), nil)
	case errors.Is(err, store.ErrLoopEnded):
		writeErr(w, http.StatusConflict, "loop_ended", err.Error(), nil)
	case errors.Is(err, store.ErrMailNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error(), nil)
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error(), nil)
	}
}

func intParam(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return v
	}
	return def
}

func (s *Server) loopOr404(w http.ResponseWriter, r *http.Request) (*store.Loop, bool) {
	loop, err := s.st.GetLoop(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return nil, false
	}
	if loop == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no loop "+r.PathValue("id"), nil)
		return nil, false
	}
	return loop, true
}

func (s *Server) memberOr404(w http.ResponseWriter, ctx context.Context, loopID, role string) (*store.LoopMember, bool) {
	roster, err := s.st.ListLoopMembers(ctx, loopID)
	if err != nil {
		writeStoreErr(w, err)
		return nil, false
	}
	for _, m := range roster {
		if strings.EqualFold(m.Role, role) {
			return m, true
		}
	}
	writeErr(w, http.StatusNotFound, "not_found", "no member "+role+" in this loop", nil)
	return nil, false
}

// MountOptions selects which surfaces Mount puts on the listener.
type MountOptions struct {
	// Public is true for the remote listener. It omits the member mail API
	// (/api/v1/agent/*, member tokens): that surface exists for agents on
	// this machine and has no business being reachable from a public URL.
	Public bool
}

// Mount puts both loop surfaces on one listener alongside the existing control
// API. The prefixes are load bearing: /api/v1/agentd/... must keep reaching
// control, and it does because the character after "agent" is "d" rather than
// a slash. This lives here rather than inline in agentd so the routing it
// depends on is the routing under test.
func Mount(control http.Handler, agents *mailapi.Server, engineer *Server, opts MountOptions) *http.ServeMux {
	root := http.NewServeMux()
	if !opts.Public {
		// mailapi is local-only: /api/v1/agent/* is the agent-member
		// surface, gated by member tokens the engineer issues, and a
		// public listener must never see it.
		root.Handle(mailapi.Prefix+"/", agents.Handler())
	}
	root.Handle(ToolsPrefix, engineer.Handler())
	root.Handle(ToolsPrefix+"/", engineer.Handler())
	root.Handle(PromptsPrefix, engineer.Handler())
	root.Handle(PromptsPrefix+"/", engineer.Handler())
	root.Handle(Prefix, engineer.Handler())
	root.Handle(Prefix+"/", engineer.Handler())
	root.Handle("/", control)
	return root
}
