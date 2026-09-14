// Package adminsock is the JSON-over-unix-socket control plane the CLI uses to
// talk to a running daemon: remote-access status and logout, minting pairing
// tokens, deciding access requests and revoking devices.
//
// There is no TCP exposure. Access control is the filesystem: the socket sits
// in a directory only the daemon's user can enter (0700) and is itself 0600,
// so no other local user can connect even for the instant between bind and
// chmod.
//
// Endpoints (JSON):
//
//	GET  /remote/status
//	POST /remote/logout
//	POST /pair/mint            -> {"token","expiresAt"}
//	GET  /pair/requests        -> {"requests":[PairRequest]}
//	POST /pair/requests/{id}/approve, /pair/requests/{id}/deny
//	                           -> {"request":PairRequest,"decision"}
//	POST /devices/{id}/revoke  -> {"revoked","connectionsClosed"}
//	POST /account/reload       -> the daemon's account reporting state
//	GET  /health
package adminsock

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SocketName is the socket's file name inside its directory.
const SocketName = "agentflow.sock"

// maxSocketPath is the longest socket path every supported OS accepts. The
// kernel's sun_path holds 108 bytes on Linux and 104 on macOS, NUL included;
// a longer path fails to bind with "invalid argument".
const maxSocketPath = 100

// Dir returns the socket directory for a data directory: <dataDir>/run, or,
// when that would make the socket path too long to bind (a deep home directory
// or AF_DATA_DIR), a short per-user directory under the system temp directory
// derived from the data directory, so the daemon and the CLI agree on it.
func Dir(dataDir string) string {
	d := filepath.Join(dataDir, "run")
	if len(filepath.Join(d, SocketName)) <= maxSocketPath {
		return d
	}
	sum := sha256.Sum256([]byte(dataDir))
	name := fmt.Sprintf("agentflow-%d-%x", os.Getuid(), sum[:6])
	base := os.TempDir()
	if len(filepath.Join(base, name, SocketName)) > maxSocketPath {
		base = "/tmp"
	}
	return filepath.Join(base, name)
}

// SocketPath returns the socket path for a data directory.
func SocketPath(dataDir string) string { return filepath.Join(Dir(dataDir), SocketName) }

// ErrNotFound is returned by Revoke for an unknown device id and by
// DecidePairRequest when no pending request matches.
var ErrNotFound = errors.New("not found")

// Config wires the endpoints to the daemon.
type Config struct {
	// Dir is the socket directory. It is created (or tightened) to 0700.
	Dir string
	// Status returns the remote-access status.
	Status func() any
	// Logout logs remote access out of Tailscale.
	Logout func(ctx context.Context) error
	// Revoke revokes a device and closes its live connections, returning how
	// many were closed. It returns ErrNotFound for an unknown id.
	Revoke func(ctx context.Context, id string) (closed int, err error)
	// MintPair mints a one-shot pairing token (expiry in unix ms).
	MintPair func(ctx context.Context) (token string, expiresAt int64, err error)
	// Health returns the daemon's health summary.
	Health func() any
	// PairRequests lists the access requests waiting for a decision.
	PairRequests func() []PairRequest
	// DecidePairRequest approves or denies a pending access request named by
	// its id or its four-digit code. It returns ErrNotFound when none matches.
	DecidePairRequest func(ctx context.Context, idOrCode string, approve bool) (PairRequest, error)
	// ReloadAccount re-reads the stored account after a sign-in or sign-out
	// and returns the resulting reporting state.
	ReloadAccount func() any
}

// PairRequest is an access request from a browser that has no pairing link.
type PairRequest struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MatchCode string `json:"matchCode"`
	ClientIP  string `json:"clientIp"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

// PairRequestsResponse is the body of GET /pair/requests.
type PairRequestsResponse struct {
	Requests []PairRequest `json:"requests"`
}

// DecisionResponse is the body of POST /pair/requests/{id}/approve and /deny.
type DecisionResponse struct {
	Request  PairRequest `json:"request"`
	Decision string      `json:"decision"`
}

// Server is the admin socket listener.
type Server struct {
	path string
	srv  *http.Server

	mu     sync.Mutex
	ln     net.Listener
	closed bool
}

// Start listens on cfg.Dir/agentflow.sock. The caller must already hold the
// daemon's instance lock: a socket file left by a crashed daemon is removed
// here, which is only safe when no other daemon can be using it.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Dir == "" {
		return nil, errors.New("adminsock: no socket directory")
	}
	if cfg.Status == nil || cfg.Logout == nil || cfg.Revoke == nil || cfg.MintPair == nil || cfg.Health == nil ||
		cfg.PairRequests == nil || cfg.DecidePairRequest == nil || cfg.ReloadAccount == nil {
		return nil, errors.New("adminsock: every Config func must be set")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("adminsock: %w", err)
	}
	if err := os.Chmod(cfg.Dir, 0o700); err != nil { //nolint:gosec // a directory needs the search bit
		return nil, fmt.Errorf("adminsock: %w", err)
	}
	// The directory may be the short fallback in a shared temp directory,
	// where another user could have created it first: only a real directory
	// this user owns, with no access for anyone else, may hold the socket.
	if err := checkPrivateDir(cfg.Dir); err != nil {
		return nil, fmt.Errorf("adminsock: %w", err)
	}
	path := filepath.Join(cfg.Dir, SocketName)
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("adminsock: %s exists and is not a socket; refusing to remove it", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("adminsock: remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("adminsock: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("adminsock: %w", err)
	}
	s := &Server{
		path: path,
		ln:   ln,
		srv: &http.Server{
			Handler:           handler(cfg),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       30 * time.Second,
		},
	}
	go func() { _ = s.srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	return s, nil
}

// Path returns the socket path.
func (s *Server) Path() string { return s.path }

// Close stops the listener and removes the socket file.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	err := s.srv.Close()
	_ = os.Remove(s.path)
	return err
}

func handler(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /remote/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, cfg.Status())
	})
	mux.HandleFunc("POST /remote/logout", func(w http.ResponseWriter, r *http.Request) {
		if err := cfg.Logout(r.Context()); err != nil {
			writeError(w, http.StatusConflict, "logout_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"loggedOut": true})
	})
	mux.HandleFunc("POST /pair/mint", func(w http.ResponseWriter, r *http.Request) {
		token, expiresAt, err := cfg.MintPair(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "mint_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, MintResponse{Token: token, ExpiresAt: expiresAt})
	})
	mux.HandleFunc("GET /pair/requests", func(w http.ResponseWriter, r *http.Request) {
		reqs := cfg.PairRequests()
		if reqs == nil {
			reqs = []PairRequest{}
		}
		writeJSON(w, http.StatusOK, PairRequestsResponse{Requests: reqs})
	})
	decide := func(approve bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			req, err := cfg.DecidePairRequest(r.Context(), id, approve)
			switch {
			case errors.Is(err, ErrNotFound):
				writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no pending access request %q", id))
			case err != nil:
				writeError(w, http.StatusInternalServerError, "decide_failed", err.Error())
			default:
				decision := "denied"
				if approve {
					decision = "approved"
				}
				writeJSON(w, http.StatusOK, DecisionResponse{Request: req, Decision: decision})
			}
		}
	}
	mux.HandleFunc("POST /pair/requests/{id}/approve", decide(true))
	mux.HandleFunc("POST /pair/requests/{id}/deny", decide(false))
	mux.HandleFunc("POST /devices/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		closed, err := cfg.Revoke(r.Context(), id)
		switch {
		case errors.Is(err, ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no device with id %q", id))
		case err != nil:
			writeError(w, http.StatusInternalServerError, "revoke_failed", err.Error())
		default:
			writeJSON(w, http.StatusOK, RevokeResponse{Revoked: id, ConnectionsClosed: closed})
		}
	})
	mux.HandleFunc("POST /account/reload", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, cfg.ReloadAccount())
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, cfg.Health())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such admin endpoint")
	})
	return mux
}

// MintResponse is the body of POST /pair/mint.
type MintResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"`
}

// RevokeResponse is the body of POST /devices/{id}/revoke.
type RevokeResponse struct {
	Revoked           string `json:"revoked"`
	ConnectionsClosed int    `json:"connectionsClosed"`
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, msg
	writeJSON(w, status, b)
}
