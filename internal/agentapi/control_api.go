package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/spawner"
)

// controlClientFor resolves the Control implementation for an already-spawned
// run. The lookup follows the persistence contract (Plan / migration
// 0015): the engine id lives on the row, the control endpoint lives on the
// row, and the in-memory Proc may have richer state we can fall back to.
//
// When the engine supports session auto-binding (opencode.Control implements
// engine.SessionBinder), the stored session id is bound here so per-session
// methods like SetModel / Abort know which session to address — without
// this, every control call after a restart would need the spawner's
// PostStart hook to have run.
func (s *Server) controlClientFor(ctx context.Context, runID string) (engine.Control, engine.Caps, string, error) {
	if s.Engines == nil {
		return nil, engine.Caps{}, "", errors.New("engine registry not configured")
	}
	sess, err := s.spawner.Status(ctx, runID)
	if err != nil {
		return nil, engine.Caps{}, "", err
	}
	if sess == nil {
		return nil, engine.Caps{}, "", errRunNotFound
	}
	eng, err := s.Engines.Get(sess.Engine)
	if err != nil {
		return nil, engine.Caps{}, sess.Engine, err
	}
	c := eng.Control(sess.ControlBase)
	if c == nil {
		return nil, engine.Caps{}, sess.Engine, errEngineUnsupported
	}
	if binder, ok := c.(engine.SessionBinder); ok && sess.SessionID != "" {
		binder.BindSession(sess.SessionID)
	}
	// A PTY-driven control (Claude: every slash command is reachable by
	// typing, and nothing else is) gets the run's live PTY master. The
	// SPAWNER's master, not the tty hub's session: the hub only holds a
	// session while a WebSocket viewer is attached, so binding from there
	// would make the control sheet work only while someone happened to be
	// watching the terminal. The spawner's master is valid for the whole
	// process lifetime.
	if binder, ok := c.(engine.PTYBinder); ok {
		if m, live := s.spawner.PTYMaster(runID); live && m != nil {
			binder.BindPTY(&ptyWriter{w: m}, sess.CWD)
		}
	}
	return c, c.Capabilities(), sess.Engine, nil
}

// ptyWriter adapts a run's PTY master to engine.PTYWriter. Submit reuses the
// tty hub's pacing (text, pause, then a separate CR) so a slash command
// injected from the control sheet reaches the TUI exactly the way a line
// sent from the composer does.
type ptyWriter struct{ w io.Writer }

func (p *ptyWriter) Write(b []byte) error {
	_, err := p.w.Write(b)
	return err
}

func (p *ptyWriter) Submit(text string) error {
	// Not bracketed: an injected command is a single line with no embedded
	// newlines, and a TUI that has not enabled mode 2004 would render the
	// markers as literal text.
	return ttySubmit(p.w, text, false)
}

// errRunNotFound is the canonical error for missing runs returned by the
// control endpoints. Handlers translate to a 404.
var (
	errRunNotFound       = errors.New("control: run not found")
	errEngineUnsupported = errors.New("control: engine does not expose a control layer")
)

// --- handlers ----------------------------------------------------------------

// handleListEngines returns the registered engine ids + display names. The
// frontend uses it to populate the engine selector on RunForm and the engine
// filter on the sessions list.
func (s *Server) handleListEngines(w http.ResponseWriter, r *http.Request) {
	if s.Engines == nil {
		writeJSON(w, http.StatusOK, map[string]any{"engines": []map[string]any{}})
		return
	}
	out := make([]map[string]any, 0, len(s.Engines.IDs()))
	for _, id := range s.Engines.IDs() {
		e, err := s.Engines.Get(id)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id":      e.ID(),
			"version": e.LookupVersion(r.Context()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"engines": out})
}

// handleControlCaps returns the engine's Capabilities mask for the run.
// The frontend uses it to grey out pickers the engine cannot serve
// (Plan — "Caps is load-bearing").
func (s *Server) handleControlCaps(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"engine": engineID,
		"caps":   caps,
	})
}

// handleControlState returns the engine's CURRENTLY ACTIVE model/agent/
// provider (Plan P3 — the "truth" the header chips always reconcile to).
func (s *Server) handleControlState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, _, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	state, err := c.State(ctx)
	if err != nil {
		if errors.Is(err, engine.ErrUnsupported) {
			writeJSON(w, http.StatusOK, map[string]any{
				"engine": engineID,
				"state":  engine.SessionState{},
				"note":   "engine does not expose live state",
			})
			return
		}
		writeError(w, http.StatusBadGateway, "control_state_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": engineID, "state": state})
}

// handleControlModels lists the engine's available models. Claude always
// returns 501 (not enumerable); OpenCode serves GET /model.
func (s *Server) handleControlModels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.ListModels {
		writeJSON(w, http.StatusOK, map[string]any{
			"engine": engineID,
			"models": []engine.Model{},
			"note":   "engine does not enumerate models",
		})
		return
	}
	// 12s, not 5s, and a wait rather than a single ask. This is the same race
	// the model prober hit: an engine can answer health several seconds
	// before its catalogue is populated, and the control sheet is most often
	// opened on a member spawned moments ago. An engine without the race does
	// not implement ModelWaiter and pays none of this.
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	var models []engine.Model
	if waiter, ok := c.(engine.ModelWaiter); ok {
		models, err = waiter.WaitModels(ctx, 12*time.Second)
	} else {
		models, err = c.Models(ctx)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "control_models_failed", err.Error())
		return
	}
	if models == nil {
		models = []engine.Model{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": engineID, "models": models})
}

// handleControlSetModel applies a model selection optimistically, then
// re-reads state and returns it so the picker reconciles (Plan P3).
func (s *Server) handleControlSetModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil ||
		strings.TrimSpace(body.Model) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "model required")
		return
	}
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.SetModel {
		writeError(w, http.StatusBadRequest, "control_unsupported",
			"engine "+engineID+" does not support live model switching")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := c.SetModel(ctx, body.Model); err != nil {
		writeError(w, http.StatusBadGateway, "control_set_model_failed", err.Error())
		return
	}
	// re-read so the caller sees the truth — never optimistic.
	state, _ := c.State(ctx)
	// also persist on the row so /run and the sessions list show it.
	if cur, _ := s.spawner.Status(r.Context(), id); cur != nil {
		cur.Model = body.Model
		_ = s.persistSessionModel(cur)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"engine":  engineID,
		"applied": body.Model,
		"state":   state,
	})
}

// handleControlAgents lists the engine's agents (OpenCode only; Claude
// returns the empty list).
func (s *Server) handleControlAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.ListAgents {
		writeJSON(w, http.StatusOK, map[string]any{
			"engine": engineID,
			"agents": []engine.Agent{},
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	agents, err := c.Agents(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "control_agents_failed", err.Error())
		return
	}
	if agents == nil {
		agents = []engine.Agent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": engineID, "agents": agents})
}

// handleControlSetAgent applies an agent selection (OpenCode only).
func (s *Server) handleControlSetAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil ||
		strings.TrimSpace(body.Agent) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "agent required")
		return
	}
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.SetAgent {
		writeError(w, http.StatusBadRequest, "control_unsupported",
			"engine "+engineID+" does not support live agent switching")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := c.SetAgent(ctx, body.Agent); err != nil {
		writeError(w, http.StatusBadGateway, "control_set_agent_failed", err.Error())
		return
	}
	state, _ := c.State(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"engine":  engineID,
		"applied": body.Agent,
		"state":   state,
	})
}

// handleControlProviders lists the engine's LLM providers (OpenCode only).
func (s *Server) handleControlProviders(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.ListProviders {
		writeJSON(w, http.StatusOK, map[string]any{
			"engine": engineID, "providers": []engine.Provider{},
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	providers, err := c.Providers(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "control_providers_failed", err.Error())
		return
	}
	if providers == nil {
		providers = []engine.Provider{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": engineID, "providers": providers})
}

// handleControlSetProvider applies a provider switch (OpenCode: implicit
// through model id like "anthropic/claude-sonnet-4-5"; Claude: unsupported).
func (s *Server) handleControlSetProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil ||
		strings.TrimSpace(body.Provider) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "provider required")
		return
	}
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.SetProvider {
		writeError(w, http.StatusBadRequest, "control_unsupported",
			"engine "+engineID+" does not support live provider switching")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := c.SetProvider(ctx, body.Provider); err != nil {
		writeError(w, http.StatusBadGateway, "control_set_provider_failed", err.Error())
		return
	}
	state, _ := c.State(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"engine": engineID, "applied": body.Provider, "state": state,
	})
}

// handleControlMCP lists MCP servers (OpenCode only). Read-only for the
// slice — enable/disable lives behind its own surface later.
func (s *Server) handleControlMCP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.ListMCPServers {
		writeJSON(w, http.StatusOK, map[string]any{
			"engine": engineID, "servers": []engine.MCPServer{},
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	servers, err := c.MCPServers(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "control_mcp_failed", err.Error())
		return
	}
	if servers == nil {
		servers = []engine.MCPServer{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": engineID, "servers": servers})
}

// handleControlSkills lists the engine's skill files (SKILL.md under
// .claude/skills or the engine's own equivalent). OpenCode-only for now.
func (s *Server) handleControlSkills(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.ListSkills {
		writeJSON(w, http.StatusOK, map[string]any{
			"engine": engineID, "skills": []engine.Skill{},
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	skills, err := c.Skills(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "control_skills_failed", err.Error())
		return
	}
	if skills == nil {
		skills = []engine.Skill{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": engineID, "skills": skills})
}

// handleControlCommands lists the engine's slash commands. Plan P4: the
// slash-command autocomplete in the composer reads this list.
func (s *Server) handleControlCommands(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.ListCommands {
		writeJSON(w, http.StatusOK, map[string]any{
			"engine": engineID, "commands": []engine.Command{},
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	commands, err := c.Commands(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "control_commands_failed", err.Error())
		return
	}
	if commands == nil {
		commands = []engine.Command{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": engineID, "commands": commands})
}

// handleControlRunCommand invokes a slash command by name + args. Plan P4:
// "the same UI, injection underneath" — Claude injects the literal name
// into the PTY; OpenCode serves it natively.
func (s *Server) handleControlRunCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
		Args string `json:"args"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil ||
		strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name required")
		return
	}
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.RunCommand {
		writeError(w, http.StatusBadRequest, "control_unsupported",
			"engine "+engineID+" does not support running slash commands natively")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := c.RunCommand(ctx, body.Name, body.Args); err != nil {
		writeError(w, http.StatusBadGateway, "control_command_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"engine": engineID, "applied": body.Name, "args": body.Args,
	})
}

// handleControlPermissions returns the engine's pending permission requests.
// Plan P6: the frontend renders these as native Allow/Allow-always/Deny
// buttons instead of forcing the phone user to type a number into a TUI
// menu.
func (s *Server) handleControlPermissions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.ReplyPermission {
		writeJSON(w, http.StatusOK, map[string]any{
			"engine": engineID, "permissions": []engine.PermissionRequest{},
			"note": "engine uses the approval hook for permissions",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	perms, err := c.Permissions(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "control_permissions_failed", err.Error())
		return
	}
	if perms == nil {
		perms = []engine.PermissionRequest{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": engineID, "permissions": perms})
}

// handleControlReplyPermission routes an Allow/Allow-always/Deny choice
// back through the engine's native API. The scope (session|always) maps
// onto the OpenCode "allow" vs "always" response value.
func (s *Server) handleControlReplyPermission(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		PermissionID string `json:"permissionId"`
		Allow        bool   `json:"allow"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil ||
		strings.TrimSpace(body.PermissionID) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "permissionId required")
		return
	}
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.ReplyPermission {
		writeError(w, http.StatusBadRequest, "control_unsupported",
			"engine "+engineID+" routes permissions through the approval hook")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := c.ReplyPermission(ctx, body.PermissionID, body.Allow, body.Scope); err != nil {
		writeError(w, http.StatusBadGateway, "control_reply_permission_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"engine": engineID, "permissionId": body.PermissionID,
		"applied": body.Allow, "scope": body.Scope,
	})
}

// handleControlQuestions returns the engine's pending question prompts.
// Plan P6: native question UI replaces the "type a number" TUI flow.
func (s *Server) handleControlQuestions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.ReplyQuestion {
		writeJSON(w, http.StatusOK, map[string]any{
			"engine": engineID, "questions": []engine.Question{},
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	qs, err := c.Questions(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "control_questions_failed", err.Error())
		return
	}
	if qs == nil {
		qs = []engine.Question{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": engineID, "questions": qs})
}

// handleControlReplyQuestion submits an answer to a question prompt.
func (s *Server) handleControlReplyQuestion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		QuestionID string `json:"questionId"`
		Answer     string `json:"answer"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil ||
		strings.TrimSpace(body.QuestionID) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "questionId required")
		return
	}
	c, caps, engineID, err := s.controlClientFor(r.Context(), id)
	if err != nil {
		s.writeControlErr(w, err, engineID)
		return
	}
	if !caps.ReplyQuestion {
		writeError(w, http.StatusBadRequest, "control_unsupported",
			"engine "+engineID+" does not support native question replies")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := c.ReplyQuestion(ctx, body.QuestionID, body.Answer); err != nil {
		writeError(w, http.StatusBadGateway, "control_reply_question_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"engine": engineID, "questionId": body.QuestionID, "applied": body.Answer,
	})
}

// writeControlErr maps internal control errors to the right HTTP status.
func (s *Server) writeControlErr(w http.ResponseWriter, err error, engineID string) {
	switch {
	case errors.Is(err, errRunNotFound):
		writeError(w, http.StatusNotFound, "not_found", "run not found")
	case errors.Is(err, errEngineUnsupported):
		writeError(w, http.StatusBadRequest, "engine_unsupported",
			"engine "+engineID+" does not expose a control layer")
	default:
		writeError(w, http.StatusInternalServerError, "control_error", err.Error())
	}
}

// persistSessionModel writes the applied model to the managed row.
//
// A narrow UPDATE, not an upsert of the whole row. The upsert it used to do
// rebuilt the row from an in-memory Session that carries no Pgid,
// ProcStartTicks, Generation or StopReason, so every successful model change
// wiped the kill identity of a live process.
func (s *Server) persistSessionModel(sess *spawner.Session) error {
	if sess == nil {
		return nil
	}
	return s.st.SetManagedSessionModel(context.Background(), sess.ID, sess.Model)
}
