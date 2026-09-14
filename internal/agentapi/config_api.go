package agentapi

import (
	"context"
	"net/http"
	"time"

	"github.com/arthurobo/agentflow/internal/claudeconfig"
	"github.com/arthurobo/agentflow/internal/engine"
)

// handleSessionConfig returns the engine-aware "what rules are actually in
// effect" payload (Plan P8). For Claude runs we parse ~/.claude +
// cwd-scoped CLAUDE.md + settings.json locally; for OpenCode runs we
// delegate to the engine's /config API.
//
// The response shape is intentionally engine-agnostic at the top level:
// files (CLAUDE.md + settings paths), settings (model/permission/tools),
// hooks, agents, skills. The engine-specific extras (OpenCode's full
// config blob) live under `engineExtras`.
func (s *Server) handleSessionConfig(w http.ResponseWriter, r *http.Request) {
	sess, err := s.spawner.Status(r.Context(), r.PathValue("id"))
	if err != nil || sess == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	summary := claudeconfig.LoadForSession(sess.CWD)

	// Best-effort engine-side extras (OpenCode: /config). When the engine
	// has no control channel we just return the Claude-style summary.
	var engineExtras map[string]any
	if s.Engines != nil {
		eng, err := s.Engines.Get(sess.Engine)
		if err == nil && eng != nil && sess.ControlBase != "" {
			c := eng.Control(sess.ControlBase)
			if binder, ok := c.(engine.SessionBinder); ok && sess.SessionID != "" {
				binder.BindSession(sess.SessionID)
			}
			if c != nil && c.Capabilities().ListModels {
				ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
				defer cancel()
				// we treat Models as a cheap liveness probe; the actual
				// config comes via a dedicated fetch in a later slice
				if _, err := c.Models(ctx); err == nil {
					engineExtras = map[string]any{
						"engine":      sess.Engine,
						"controlBase": sess.ControlBase,
						"reachable":   true,
					}
				} else {
					engineExtras = map[string]any{
						"engine":      sess.Engine,
						"controlBase": sess.ControlBase,
						"reachable":   false,
					}
				}
			}
		}
	}
	// A nil map marshals as `null`, and a client that reasonably typed this
	// as "an object, maybe absent" rejected it. Omit the key instead: absent
	// and empty both read as "this engine has no extras", and null is the
	// only one of the three that needs special handling at every reader.
	out := map[string]any{
		"engine":  sess.Engine,
		"cwd":     sess.CWD,
		"summary": summary,
	}
	if len(engineExtras) > 0 {
		out["engineExtras"] = engineExtras
	}
	writeJSON(w, http.StatusOK, out)
}
