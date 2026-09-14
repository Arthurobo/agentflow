// Package mailapi is the agent-facing HTTP surface: the port an external
// agent joins a loop through. It is a different audience from agentapi and
// carries its OWN token class, X-Agent-Token, resolved against loop_members.
// A device token cannot act as a member here and a member token cannot reach
// the device surface, because the two live in different tables.
//
// The long poll is the load-bearing design constraint. store's pool is four
// connections and a poll is held for up to five minutes, so a handler that
// waits with a connection or a transaction in hand deadlocks the whole
// application on the fourth polling agent, dashboard included. Nothing here
// holds a connection while it waits: it does one cheap EXISTS, then parks on
// a channel the store closes when mail is committed, then loops.
package mailapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arthurobo/agentflow/internal/store"
)

// Defaults. MaxWait is the long-poll ceiling; PollTick is only a safety net
// behind the wake-up channel, not the mechanism.
const (
	DefaultMaxWait           = 300 * time.Second
	DefaultPollTick          = 1 * time.Second
	DefaultMaxBodyChars      = 500_000
	DefaultQuoteGuardMinimum = 400
)

// Config is the tuning, fixed at construction. It is deliberately NOT a set
// of public fields on Server: handlers read these on every request from many
// goroutines at once, so a settable knob is a data race waiting for the first
// person who adjusts one after the listener is up. The race detector found
// exactly that on the first run of this package.
type Config struct {
	MaxWait           time.Duration
	PollTick          time.Duration
	MaxBodyChars      int
	QuoteGuardMinimum int
	// RequireNotesBeforeReport refuses a worker report with no notes on
	// record. Its session can be retired the moment it delivers, so the
	// notes are all the role keeps. Defaults to true; set Relax to lift it.
	RelaxNotesBeforeReport bool
	// RetrySeconds is the pause the rendered wait command takes on a
	// response that is not inbox JSON.
	RetrySeconds int
}

func (c Config) withDefaults() Config {
	if c.MaxWait <= 0 {
		c.MaxWait = DefaultMaxWait
	}
	if c.PollTick <= 0 {
		c.PollTick = DefaultPollTick
	}
	if c.MaxBodyChars <= 0 {
		c.MaxBodyChars = DefaultMaxBodyChars
	}
	if c.QuoteGuardMinimum == 0 {
		c.QuoteGuardMinimum = DefaultQuoteGuardMinimum
	}
	if c.RetrySeconds <= 0 {
		c.RetrySeconds = 5
	}
	return c
}

// Server is the agent surface. Everything on it is immutable once built.
type Server struct {
	st  *store.Store
	log *slog.Logger
	cfg Config
}

// New builds the agent surface. A zero Config takes every default.
func New(st *store.Store, log *slog.Logger, cfg Config) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{st: st, log: log, cfg: cfg.withDefaults()}
}

// Prefix is the route prefix for every agent endpoint.
const Prefix = "/api/v1/agent"

// Handler returns the mux with every agent route mounted.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Prefix+"/whoami", s.auth(s.handleWhoami))
	mux.HandleFunc("GET "+Prefix+"/state", s.auth(s.handleState))
	mux.HandleFunc("GET "+Prefix+"/inbox", s.auth(s.handleInbox))
	mux.HandleFunc("POST "+Prefix+"/messages", s.auth(s.handlePostMessage))
	mux.HandleFunc("GET "+Prefix+"/messages/{id}", s.auth(s.handleGetMessage))
	mux.HandleFunc("POST "+Prefix+"/messages/{id}/ack", s.auth(s.handleAck))
	mux.HandleFunc("POST "+Prefix+"/messages/{id}/extend", s.auth(s.handleExtend))
	mux.HandleFunc("GET "+Prefix+"/notes", s.auth(s.handleGetNotes))
	mux.HandleFunc("POST "+Prefix+"/notes", s.auth(s.handlePostNote))
	mux.HandleFunc("GET "+Prefix+"/thread", s.auth(s.handleThread))
	mux.HandleFunc("GET "+Prefix+"/archive", s.auth(s.handleArchive))
	return mux
}

// --- auth -------------------------------------------------------------------------

type ctxKey string

const memberKey ctxKey = "mailapi.member"
const loopKey ctxKey = "mailapi.loop"

// auth resolves X-Agent-Token to a member and its loop. An empty token never
// authenticates: the engineer's row carries an empty token_hash on purpose,
// and hashing "" would otherwise let anyone present as the engineer.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(r.Header.Get("X-Agent-Token"))
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing X-Agent-Token", nil)
			return
		}
		member, loop, err := s.st.LoopMemberByToken(r.Context(), token)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error(), nil)
			return
		}
		if member == nil || loop == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "unknown agent token", nil)
			return
		}
		ctx := context.WithValue(r.Context(), memberKey, member)
		ctx = context.WithValue(ctx, loopKey, loop)
		next(w, r.WithContext(ctx))
	}
}

func memberFrom(ctx context.Context) *store.LoopMember {
	m, _ := ctx.Value(memberKey).(*store.LoopMember)
	return m
}

func loopFrom(ctx context.Context) *store.Loop {
	l, _ := ctx.Value(loopKey).(*store.Loop)
	return l
}

// requireWritable refuses a write from a dismissed member or into an ended
// loop, with the reason the agent needs to act on.
func (s *Server) requireWritable(w http.ResponseWriter, m *store.LoopMember, l *store.Loop) bool {
	if m.Status == store.LoopMemberDismissed {
		writeErr(w, http.StatusConflict, "member_dismissed",
			"You have been dismissed. Exit and stop polling.", nil)
		return false
	}
	if l.Ended() {
		writeErr(w, http.StatusConflict, "loop_ended",
			"Loop "+l.ID+" is "+l.Status+". Stop writing and wait; you are idle, not stopped.", nil)
		return false
	}
	return true
}

// --- responses --------------------------------------------------------------------

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string, details any) {
	writeJSON(w, status, map[string]any{"error": apiError{Code: code, Message: msg, Details: details}})
}

// writeStoreErr maps a delivery refusal to its status and carries the typed
// fields through as JSON. A refusal is data the UI renders, never a string it
// has to parse.
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
	case errors.Is(err, store.ErrMailRouteRefused):
		writeErr(w, http.StatusForbidden, "route_refused", err.Error(), nil)
	case errors.Is(err, store.ErrLoopEnded):
		writeErr(w, http.StatusConflict, "loop_ended", err.Error(), nil)
	case errors.Is(err, store.ErrMailSenderRetired):
		writeErr(w, http.StatusConflict, "member_dismissed", err.Error(), nil)
	case errors.Is(err, store.ErrMailNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, store.ErrInvalidRole):
		writeErr(w, http.StatusBadRequest, "invalid_role", err.Error(), nil)
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error(), nil)
	}
}

// signal is the stop/idle contract every polling agent reads. Only a dismissed
// member is told to stop; an ended loop parks it.
type signal struct {
	Stop       bool   `json:"stop"`
	Idle       bool   `json:"idle"`
	StopReason string `json:"stopReason,omitempty"`
	IdleReason string `json:"idleReason,omitempty"`
	LoopStatus string `json:"loopStatus,omitempty"`
}

func signalFor(m *store.LoopMember, l *store.Loop) signal {
	stop, idle, reason := store.LoopMemberSignal(m, l)
	out := signal{Stop: stop, Idle: idle, LoopStatus: l.Status}
	if stop {
		out.StopReason = reason
	} else if idle {
		out.IdleReason = reason
	}
	return out
}

func intParam(r *http.Request, name string, def int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

func boolParam(r *http.Request, name string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	switch raw {
	case "":
		return def
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return def
}

func runes(s string) int { return utf8.RuneCountInString(s) }
