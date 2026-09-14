package agentapi

// approvals endpoints: the blocking hook entrypoint, the
// human decide route, the inbox list, and standing policies.
import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/arthurobo/agentflow/internal/approvals"
	"github.com/arthurobo/agentflow/internal/store"
)

func (s *Server) approvalsDown(w http.ResponseWriter) bool {
	if s.Approvals == nil {
		writeError(w, http.StatusServiceUnavailable, "no_approvals", "approval engine not configured")
		return true
	}
	return false
}

// handleApprovalRequest is called by the PreToolUse hook and BLOCKS until a
// decision (engine wait; deny on timeout). Fail-closed: any decode error
// denies rather than allowing.
//
// The route is mounted on the local listener only and requires the
// X-AgentFlow-Hook-Secret header to match the 0600 secret file in the data
// directory. Nothing installs the hook yet; without the secret every caller
// gets 401.
func (s *Server) handleApprovalRequest(w http.ResponseWriter, r *http.Request) {
	if s.approvalsDown(w) {
		return
	}
	if !s.checkHookSecret(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid hook secret")
		return
	}
	var payload map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"routed": false, "allowed": false, "reason": "bad_payload",
		})
		return
	}
	p := approvals.Params{
		SessionID: strFrom(payload["session_id"]),
		ToolUseID: strFrom(payload["tool_use_id"]),
		ToolName:  strFrom(payload["tool_name"]),
		CWD:       strFrom(payload["cwd"]),
	}
	// render tool_input (object or string) as text for policy matching + the
	// bounded audit preview
	if ti, ok := payload["tool_input"]; ok {
		if tiStr, ok := ti.(string); ok {
			p.ToolInput = tiStr
		} else if b, err := json.Marshal(ti); err == nil {
			p.ToolInput = string(b)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.Approvals.Wait()+5*time.Second)
	defer cancel()

	d, err := s.Approvals.Resolve(ctx, p)
	if err != nil {
		// engine-level failure: deny-by-default, never allow
		writeJSON(w, http.StatusOK, map[string]any{
			"routed": true, "allowed": false, "reason": "engine_error",
		})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// checkHookSecret compares the X-AgentFlow-Hook-Secret header against the
// 0600 file in dataDir, in constant time. A missing secret means the hook has
// not been wired up, and nothing matches it.
func (s *Server) checkHookSecret(r *http.Request) bool {
	if s.hookSecret == nil {
		return false
	}
	got := []byte(r.Header.Get("X-AgentFlow-Hook-Secret"))
	if len(got) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(got, s.hookSecret) == 1
}

// handleApprovalDecide records a human decision; every blocked hook call for
// that approval (including collapsed asks) unblocks simultaneously.
func (s *Server) handleApprovalDecide(w http.ResponseWriter, r *http.Request) {
	if s.approvalsDown(w) {
		return
	}
	id := r.PathValue("id")
	var body struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil ||
		(body.Decision != "allow" && body.Decision != "deny") {
		writeError(w, http.StatusBadRequest, "bad_request", "decision must be allow|deny")
		return
	}
	dev := deviceFrom(r.Context())
	by := "device:" + dev.ID
	if err := s.Approvals.Decide(r.Context(), id, body.Decision == "allow", by); err != nil {
		if errors.Is(err, approvals.ErrNotPending) {
			writeError(w, http.StatusConflict, "not_pending", "approval was already decided or has expired")
			return
		}
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "decision": body.Decision, "decidedBy": by,
	})
}

// handleApprovalList serves the inbox (state filter) and doubles as the audit
// trail view (no filter = everything, bounded).
func (s *Server) handleApprovalList(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	mission := r.URL.Query().Get("mission")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.st.ListApprovals(r.Context(), state, mission, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

func (s *Server) handlePoliciesList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListPolicies(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

func (s *Server) handlePoliciesUpsert(w http.ResponseWriter, r *http.Request) {
	var p store.ApprovalPolicy
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	switch p.Action {
	case "allow", "ask", "deny":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "action must be allow|ask|deny")
		return
	}
	if err := s.st.UpsertPolicy(r.Context(), &p); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "action": p.Action})
}

func (s *Server) handlePoliciesDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "bad policy id")
		return
	}
	if err := s.st.DeletePolicy(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func strFrom(m any) string {
	s, _ := m.(string)
	return s
}
