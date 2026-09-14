// handlers.go — the engineer routes.

package loopapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/engine"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

func (s *Server) handleTools(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tools": store.Tools()})
}

func (s *Server) handlePlays(w http.ResponseWriter, r *http.Request) {
	cat := s.catalogue(r.Context())
	out := []map[string]any{}
	for i := range cat.Plays {
		p := &cat.Plays[i]
		steps := make([]map[string]any, 0, len(p.Steps))
		roles := store.PlayRoles(p)
		for i := range p.Steps {
			steps = append(steps, map[string]any{
				"id": p.Steps[i].ID, "actor": p.Steps[i].Actor,
				"note": p.Steps[i].Note, "fanIn": p.Steps[i].FanIn,
			})
		}
		out = append(out, map[string]any{
			"name": p.Name, "title": p.Title, "purpose": p.Purpose, "entry": p.Entry,
			"maxRounds": p.EffectiveMaxRounds(), "hasCycle": p.HasCycle(),
			"roles": roles, "steps": steps,
		})
	}
	// A play whose merged form does not validate is absent from the
	// catalogue and named here, so an edit that broke a graph is visible
	// rather than a play that quietly disappeared.
	writeJSON(w, http.StatusOK, map[string]any{"plays": out, "dropped": cat.Dropped})
}

// catalogueBudget bounds the create-time model check. The catalogue is
// usually cached by the time create runs, because the picker just asked for
// it; this is the cold path, and a create that waits longer than this is
// worse than a create that skips the check.
const catalogueBudget = 20 * time.Second

type createRequest struct {
	// Title names the loop; Task is the instruction the orchestrator is sent.
	// One field used to be both, which is why nothing was ever instructed.
	Title               string             `json:"title"`
	Task                string             `json:"task"`
	CWD                 string             `json:"cwd"`
	Play                string             `json:"play"`
	Crew                []store.MemberSpec `json:"crew"`
	PollIntervalSeconds int                `json:"pollIntervalSeconds"`
}

// handoff is everything the engineer needs to start one agent, including the
// only readable copy of its token.
type handoff struct {
	Role    string `json:"role"`
	AgentID string `json:"agentId"`
	Tool    string `json:"tool,omitempty"`
	Model   string `json:"model,omitempty"`
	Token   string `json:"token,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
	// RunID is the run this member will be spawned as, when this deployment
	// spawns. Never serialized: it is the create handler's own bookkeeping,
	// and the board reads run ids from the crew.
	RunID string `json:"-"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON: "+err.Error(), nil)
		return
	}
	// The floor runs BEFORE anything is written, so a refused create leaves no
	// loop row, no member rows and no tokens behind.
	if err := store.ValidateInstruction(req.Title, req.Task); err != nil {
		writeStoreErr(w, err)
		return
	}
	cat := s.catalogue(r.Context())
	play, ok := cat.LookupPlay(req.Play)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_play",
			"no play "+req.Play+"; the catalogue is "+strings.Join(store.PlayNames(), ", "), nil)
		return
	}
	crew := req.Crew
	if len(crew) == 0 {
		crew = store.RoleSpecs(defaultCrewFor(play)...)
	}
	// The crew is checked against the play BEFORE anything else, and long
	// before anything is written: a play that cannot be staffed used to be
	// discovered after the loop, its members, their tokens and the task had
	// all been committed, leaving an active loop behind the 400.
	if err := play.ValidateRoster(store.CrewRoles(crew)); err != nil {
		writeStoreErr(w, err)
		return
	}
	// The shape floor in ValidateToolModel catches "minimax". It cannot catch
	// a well-shaped id for a model this install does not have, because the
	// registry does not know what this install has. When the catalogue can be
	// obtained, check against it; when it cannot, the shape floor stands
	// alone. Refusing here beats failing in a detached spawn, where the
	// member's row exists, the board shows it, and nothing is running.
	if err := s.refuseUnknownModels(r.Context(), crew); err != nil {
		writeErr(w, http.StatusBadRequest, "unknown_model", err.Error(), nil)
		return
	}
	// Allocate every run id BEFORE the transaction so each member row is born
	// knowing which run is its own. NewRunID is synchronous and needs no
	// process; the engine's own session id is discovered later and may never
	// arrive, which is why the run id is the link and not the session id.
	crew = append([]store.MemberSpec(nil), crew...)
	if s.launcher != nil {
		for i := range crew {
			if strings.ToUpper(strings.TrimSpace(crew[i].Role)) == store.RoleEngineer {
				continue
			}
			crew[i].RunID = spawner.NewRunID()
		}
	}
	loop := &store.Loop{
		Title: loopTitle(req.Title, req.Task), Task: strings.TrimSpace(req.Task),
		CWD: req.CWD, PollIntervalSeconds: req.PollIntervalSeconds,
		// Stamped with the daemon that is about to run it, so the next boot
		// can tell whether anyone still owns it.
		Generation: s.cfg.Generation,
	}
	// The task is the instruction, so it is DELIVERED and not merely stored:
	// it is posted from the engineer to the orchestrator ahead of the play's
	// entry brief, which orders it first in the orchestrator's inbox. What to
	// do, then how this play wants it done. The loop, its crew, the task and
	// the play's start are one transaction, so a refusal at any point leaves
	// nothing behind.
	tokens, err := s.st.StartLoop(r.Context(), loop, crew, play.Name, req.Task)
	if err != nil {
		writeCrewErr(w, err)
		return
	}
	fresh, err := s.st.GetLoop(r.Context(), loop.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	hands := s.handoffs(r.Context(), loop.ID, play, tokens)

	// Every row exists and the play has started, so the engineer can be sent
	// to the board now. The processes come up behind him.
	writeJSON(w, http.StatusOK, map[string]any{
		"loop":     fresh,
		"handoffs": hands,
		"spawning": s.launcher != nil,
	})
	s.spawnInBackground(loop.ID, loop.Title, loop.CWD, hands)
}

// spawnInBackground starts every member a handoff carries a run and a brief
// for, detached from the request that asked for it.
func (s *Server) spawnInBackground(loopID, title, cwd string, hands []handoff) {
	if s.launcher == nil {
		return
	}
	plans := make([]memberPlan, 0, len(hands))
	for _, h := range hands {
		if h.RunID == "" || h.Prompt == "" {
			continue // the engineer, an adopted member still running, or a brief we could not render
		}
		plans = append(plans, memberPlan{
			Role: h.Role, Tool: h.Tool, Model: h.Model, RunID: h.RunID, Prompt: h.Prompt, Token: h.Token,
		})
	}
	if len(plans) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), spawnBudget)
		defer cancel()
		s.spawnCrew(ctx, loopID, title, cwd, plans)
	}()
}

// writeCrewErr answers a refused crew. A crew over the member limit is the
// caller's mistake, so it is a 400 like every other malformed crew rather
// than the 500 an unrecognised store error gets.
func writeCrewErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrTooManyMembers) {
		writeErr(w, http.StatusBadRequest, "too_many_members", err.Error(), nil)
		return
	}
	writeStoreErr(w, err)
}

// defaultCrewFor staffs exactly the roles the play needs, plus the engineer.
func defaultCrewFor(p *store.Play) []string {
	roles := append([]string{}, store.PlayRoles(p)...)
	have := false
	for _, r := range roles {
		if r == store.RoleEngineer {
			have = true
		}
	}
	if !have {
		roles = append(roles, store.RoleEngineer)
	}
	return roles
}

// handoffs renders one prompt per member, and carries the plaintext token for
// the members whose token was just issued. This is the only moment a token is
// readable: it is stored as a hash and never returned again.
// catalogue is the merged view for this request. A read failure falls back to
// the shipped defaults rather than refusing: a machine whose override table is
// unreadable should still be able to run a loop with the prompts we shipped.
func (s *Server) catalogue(ctx context.Context) *store.PromptCatalogue {
	cat, err := s.st.PromptCatalogue(ctx)
	if err != nil {
		s.log.Warn("loopapi: merged catalogue unavailable, using shipped defaults", "err", err)
		return store.DefaultCatalogue()
	}
	return cat
}

func (s *Server) handoffs(ctx context.Context, loopID string, play *store.Play, tokens map[string]string) []handoff {
	cat := s.catalogue(ctx)
	roster, err := s.st.ListLoopMembers(ctx, loopID)
	if err != nil {
		return nil
	}
	out := []handoff{}
	for _, m := range roster {
		h := handoff{Role: m.Role, AgentID: m.ID, Tool: m.Tool, Model: m.Model,
			Token: tokens[m.Role], RunID: m.RunID}
		if h.Token != "" {
			if prompt, err := s.agents.RenderPromptWith(cat, s.cfg.BaseURL, play, m.Role, h.Token); err == nil {
				h.Prompt = prompt
			} else {
				s.log.Warn("loopapi: prompt render", "role", m.Role, "err", err)
			}
		}
		out = append(out, h)
	}
	return out
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	loops, err := s.st.ListLoops(r.Context(), r.URL.Query().Get("status"), intParam(r, "limit", 0))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	// Attention per loop, so the list says which of four open loops needs him
	// rather than four identical rows reading "active". Computed server-side
	// because only agentd knows who is holding a poll. Keyed by loop id so a
	// client that does not understand it can ignore it.
	attention := map[string]any{}
	for _, l := range loops {
		health, err := s.st.LoopHealth(r.Context(), l.ID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		stranded, failed := []string{}, []string{}
		for _, h := range health {
			if h.State == store.MemberStranded {
				stranded = append(stranded, h.Role)
			}
		}
		members, err := s.st.ListLoopMembers(r.Context(), l.ID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		for _, m := range members {
			if m.LastError != "" {
				failed = append(failed, m.Role)
			}
		}
		if len(stranded) == 0 && len(failed) == 0 {
			continue // nothing to say beats an empty object per row
		}
		attention[l.ID] = map[string]any{"stranded": stranded, "failed": failed}
	}
	writeJSON(w, http.StatusOK, map[string]any{"loops": loops, "attention": attention})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	loop, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	roster, err := s.st.ListLoopMembers(r.Context(), loop.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	notes, err := s.st.LoopNoteCounts(r.Context(), loop.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	counts, err := s.st.CountMailByStatus(r.Context(), loop.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	// Health, so the board can say whether anyone is reading each member's
	// mail rather than inferring "running" from a session id and saying it
	// forever.
	health, err := s.st.LoopHealth(r.Context(), loop.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	byMember := map[string]store.MemberHealth{}
	for _, h := range health {
		byMember[h.MemberID] = h
	}
	crew := make([]map[string]any, 0, len(roster))
	for _, m := range roster {
		row := map[string]any{
			"role": m.Role, "agentId": m.ID, "status": m.Status,
			"tool": m.Tool, "model": m.Model,
			"charsIn": m.CharsIn, "notes": notes[m.Role], "lastSeenAt": m.LastSeenAt,
			"runId": m.RunID, "sessionId": m.SessionID, "lastError": m.LastError,
		}
		if h, ok := byMember[m.ID]; ok {
			row["health"] = h.State
			row["listening"] = h.Listening
			row["listeningSince"] = h.ListeningSince
			row["held"] = h.Held
			row["pending"] = h.Pending
			row["oldestPendingAt"] = h.OldestPendingAt
			row["strandedSince"] = h.StrandedSince
			row["runState"] = h.RunState
		}
		crew = append(crew, row)
	}
	brief, _ := s.st.DeliveredStepBrief(r.Context(), loop)
	out := map[string]any{
		"loop": loop, "crew": crew, "messageCounts": counts,
		"stepBrief": brief,
	}
	if play, ok := store.LookupPlay(loop.Play); ok {
		out["maxRounds"] = play.EffectiveMaxRounds()
		if step := play.Step(loop.StepID); step != nil {
			out["stepNote"] = step.Note
			out["stepActor"] = step.Actor
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleEnd(w http.ResponseWriter, r *http.Request) {
	loop, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		Status        string `json:"status"`
		Reason        string `json:"reason"`
		DismissAgents bool   `json:"dismissAgents"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	status := req.Status
	if status != store.LoopKilled {
		status = store.LoopComplete
	}
	// The runs are read BEFORE the rows flip, because a dismissed member is
	// no longer listed as live afterwards. Ending a loop that only updated
	// rows left every member's process running with its PTY open, reading
	// "ended" on the board with nothing actually exited.
	runs := s.liveRuns(r.Context(), loop.ID)
	cancelled, err := s.st.EndLoop(r.Context(), loop.ID, status, req.Reason, req.DismissAgents)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	stopped := s.StopRuns(runs)
	writeJSON(w, http.StatusOK, map[string]any{
		"loopId": loop.ID, "status": status, "cancelledMessages": cancelled,
		"agents": agentsWord(req.DismissAgents), "stopped": stopped,
	})
}

// liveRuns lists the runs of a loop's members that may still have a process.
func (s *Server) liveRuns(ctx context.Context, loopID string) []string {
	runs, err := s.st.LiveMemberRunIDs(ctx, loopID)
	if err != nil {
		s.log.Warn("loopapi: list member runs to stop", "loop", loopID, "err", err)
		return nil
	}
	return runs
}

// stopBudget bounds stopping one batch of members. Stopping is detached from
// the request on purpose: a phone that closes the tab mid-request must not
// cancel the stop and leave half a crew running.
const stopBudget = 30 * time.Second

// StopRuns stops the given runs and returns how many stopped cleanly. It is
// the one path every loop action that retires a process goes through, so an
// end, a dismiss, a retire and a loop agentd caps all stop members the same
// way. A deployment with no launcher has no processes to stop.
func (s *Server) StopRuns(runIDs []string) int {
	if s.launcher == nil || len(runIDs) == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopBudget)
	defer cancel()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		stopped int
	)
	for _, runID := range runIDs {
		if runID == "" {
			continue
		}
		wg.Add(1)
		go func(runID string) {
			defer wg.Done()
			if err := s.launcher.Stop(ctx, runID); err != nil {
				s.log.Warn("loopapi: stop member", "run", runID, "err", err)
				return
			}
			mu.Lock()
			stopped++
			mu.Unlock()
		}(runID)
	}
	wg.Wait()
	return stopped
}

func agentsWord(dismissed bool) string {
	if dismissed {
		return "dismissed and their processes stopped"
	}
	return "processes stopped; members parked idle with live tokens, so the next loop can adopt them"
}

func (s *Server) handleDismiss(w http.ResponseWriter, r *http.Request) {
	loop, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	runs := s.liveRuns(r.Context(), loop.ID)
	n, err := s.st.DismissLoopMembers(r.Context(), loop.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	stopped := s.StopRuns(runs)
	writeJSON(w, http.StatusOK, map[string]any{"loopId": loop.ID, "dismissed": n, "stopped": stopped})
}

func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request) {
	previous, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		Title      string `json:"title"`
		Task       string `json:"task"`
		Play       string `json:"play"`
		CarryNotes bool   `json:"carryNotes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	// The same floor as create. The successor's orchestrator is briefed with
	// this task and reads it, not the title carried over from the parent, so a
	// two-word continuation reproduces exactly the failure the floor exists to
	// stop.
	if err := store.ValidateInstruction(orString(req.Title, previous.Title), req.Task); err != nil {
		writeStoreErr(w, err)
		return
	}
	playName := req.Play
	if playName == "" {
		playName = previous.Play
	}
	cat := s.catalogue(r.Context())
	play, ok := cat.LookupPlay(playName)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_play", "no play "+playName, nil)
		return
	}
	crew := store.RoleSpecs(defaultCrewFor(play)...)
	if s.launcher != nil {
		for i := range crew {
			if crew[i].Role != store.RoleEngineer {
				crew[i].RunID = spawner.NewRunID()
			}
		}
	}
	// A successor keeps its parent's name unless renamed. Losing it would
	// blank the board header and every member session name for round two.
	successor := &store.Loop{
		Title: loopTitle(orString(req.Title, previous.Title), req.Task),
		Task:  strings.TrimSpace(req.Task), CWD: previous.CWD, ParentLoopID: previous.ID,
		PollIntervalSeconds: previous.PollIntervalSeconds,
		// This life's, not the parent's: a successor is started by whichever
		// daemon is running now, and inheriting a dead generation would have
		// it reaped on the next restart for no reason.
		Generation: s.cfg.Generation,
	}
	// Ending the parent, staffing the successor, adopting the parent's
	// members, carrying notes, delivering the new task and starting the play
	// are one transaction. The new task used to be stored and never sent,
	// so the successor's orchestrator started round two without being told
	// what round two was.
	res, err := s.st.ContinueLoop(r.Context(), previous.ID, successor, crew, play.Name, req.Task, req.CarryNotes)
	if err != nil {
		writeCrewErr(w, err)
		return
	}
	tokens := res.Tokens
	// An adopted member whose process is gone (the parent was ended, which
	// stops its members) would leave round two with nobody to do the work.
	// It gets a fresh run and a fresh token and is started with the rest.
	if s.launcher != nil {
		s.restaffAdopted(r.Context(), successor.ID, res.Adopted, tokens)
	}
	fresh, _ := s.st.GetLoop(r.Context(), successor.ID)
	hands := s.handoffs(r.Context(), successor.ID, play, tokens)
	writeJSON(w, http.StatusOK, map[string]any{
		"loop": fresh, "continuedFrom": previous.ID,
		"keptAgents": res.Adopted, "notesCarried": res.NotesCarried,
		"handoffs": hands, "spawning": s.launcher != nil,
	})
	s.spawnInBackground(successor.ID, successor.Title, successor.CWD, hands)
}

// restaffAdopted gives every adopted member with no live process a new run
// and token, adding the token to tokens so its brief is rendered and it is
// spawned.
func (s *Server) restaffAdopted(ctx context.Context, loopID string, adopted []string, tokens map[string]string) {
	for _, role := range adopted {
		if role == store.RoleEngineer {
			continue
		}
		m, err := s.st.LoopMemberByRole(ctx, loopID, role)
		if err != nil || m == nil {
			continue
		}
		if m.RunID == "" {
			continue // it joined from somewhere else; there is no process here to replace
		}
		if live, err := s.st.RunIsLive(ctx, m.RunID); err != nil || live {
			continue // still running, and it keeps its token and its terminal
		}
		token, err := s.st.ReassignLoopMemberRun(ctx, m.ID, spawner.NewRunID())
		if err != nil {
			s.log.Warn("loopapi: restaff adopted member", "loop", loopID, "role", role, "err", err)
			continue
		}
		tokens[role] = token
	}
}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	loop, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	var spec store.MemberSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON: "+err.Error(), nil)
		return
	}
	member, token, err := s.st.AddLoopMember(r.Context(), loop.ID, spec)
	if err != nil {
		writeCrewErr(w, err)
		return
	}
	h := handoff{Role: member.Role, AgentID: member.ID, Tool: member.Tool, Model: member.Model, Token: token}
	cat := s.catalogue(r.Context())
	if play, ok := cat.LookupPlay(loop.Play); ok && token != "" {
		if prompt, err := s.agents.RenderPromptWith(cat, s.cfg.BaseURL, play, member.Role, token); err == nil {
			h.Prompt = prompt
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"handoff": h})
}

func (s *Server) handleRetire(w http.ResponseWriter, r *http.Request) {
	loop, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	if _, ok := s.memberOr404(w, r.Context(), loop.ID, r.PathValue("role")); !ok {
		return
	}
	// One transaction takes the role out of the play: dismissed, off the
	// step's awaiting list, and its pending and held mail cancelled.
	member, err := s.st.RetireLoopMember(r.Context(), loop.ID, r.PathValue("role"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if member == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no member "+r.PathValue("role")+" in this loop", nil)
		return
	}
	stopped := s.StopRuns([]string{member.RunID})
	writeJSON(w, http.StatusOK, map[string]any{"role": member.Role, "status": member.Status, "stopped": stopped})
}

func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request) {
	loop, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	member, ok := s.memberOr404(w, r.Context(), loop.ID, r.PathValue("role"))
	if !ok {
		return
	}
	token, err := s.st.RotateLoopMemberToken(r.Context(), member.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	h := handoff{Role: member.Role, AgentID: member.ID, Tool: member.Tool, Model: member.Model, Token: token}
	cat := s.catalogue(r.Context())
	if play, ok := cat.LookupPlay(loop.Play); ok {
		if prompt, err := s.agents.RenderPromptWith(cat, s.cfg.BaseURL, play, member.Role, token); err == nil {
			h.Prompt = prompt
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"handoff": h})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	loop, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	msgs, err := s.st.ListLoopMessages(r.Context(), loop.ID, intParam(r, "limit", 0))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (s *Server) handleNotes(w http.ResponseWriter, r *http.Request) {
	loop, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	notes, err := s.st.LoopNotes(r.Context(), loop.ID, r.URL.Query().Get("role"), intParam(r, "limit", 0))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}

// handleRefusals is the refusal history. Refusals are rows, not only events,
// so a stall diagnosed a day later reads the same as one watched live.
func (s *Server) handleRefusals(w http.ResponseWriter, r *http.Request) {
	loop, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	refusals, err := s.st.ListLoopRefusals(r.Context(), loop.ID, intParam(r, "limit", 0))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"refusals": refusals})
}

// handleSay posts as ENGINEER. It is an ordinary message into the inbox the
// recipient is already polling, not a push and not a side channel.
func (s *Server) handleSay(w http.ResponseWriter, r *http.Request) {
	loop, ok := s.loopOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		To      string `json:"to"`
		Body    string `json:"body"`
		Subject string `json:"subject"`
		Forward string `json:"forward"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON: "+err.Error(), nil)
		return
	}
	if req.To == "" {
		req.To = store.RoleOrchestrator
	}
	engineer, ok := s.memberOr404(w, r.Context(), loop.ID, store.RoleEngineer)
	if !ok {
		return
	}
	recipient, err := s.st.ResolveLoopRecipient(r.Context(), loop.ID, req.To)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if recipient == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no member "+req.To+" in this loop", nil)
		return
	}
	var msg *store.LoopMessage
	if req.Forward != "" {
		msg, err = s.st.ForwardMail(r.Context(), engineer, recipient,
			store.MailForward{OriginalID: req.Forward, Subject: req.Subject})
	} else {
		msg, err = s.st.PostMail(r.Context(), engineer, recipient,
			store.MailPost{Subject: req.Subject, Body: req.Body})
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": msg.ID, "to": msg.RecipientRole, "bodyChars": msg.BodyChars,
		"mirroredToOrchestrator": store.BaseRole(msg.RecipientRole) != store.RoleOrchestrator,
	})
}

// ModelLister reports the models a tool can run RIGHT NOW on this machine.
// Separate from the registry because for opencode the answer is a property of
// the install: which providers are authenticated and what the config enables.
type ModelLister interface {
	Models(ctx context.Context, tool, cwd string) ([]engine.Model, error)
}

// handleToolModels answers what one tool can run. Claude's answer is the
// registry's four aliases, which the CLI itself defines and which do not vary
// by machine. OpenCode's has to be asked.
//
// The response is a PROJECTION and that is a security property, not tidiness:
// opencode's own model listing carries a live provider credential on every
// entry. engine.Model is the boundary and nothing here widens it.
func (s *Server) handleToolModels(w http.ResponseWriter, r *http.Request) {
	tool := store.NormaliseTool(r.PathValue("id"))
	spec, ok := store.LookupTool(tool)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown_tool", "no tool "+tool, nil)
		return
	}
	if spec.Probed {
		out := make([]engine.Model, 0, len(spec.Models))
		for _, m := range spec.Models {
			out = append(out, engine.Model{ID: m, DisplayName: m, Provider: tool})
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": out, "probed": true, "note": spec.Note})
		return
	}
	if s.models == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"models": []engine.Model{}, "probed": false,
			"note": "this agentd cannot ask " + tool + " what it runs, so the model has to be typed",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	models, err := s.models.Models(ctx, tool, r.URL.Query().Get("cwd"))
	if err != nil {
		// Not an error to the caller: an unprobed tool with no live answer is
		// exactly the state the registry already describes, and the wizard
		// falls back to typing a model.
		s.log.Info("loopapi: model probe", "tool", tool, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"models": []engine.Model{}, "probed": false,
			"note": "could not ask " + tool + " what it runs: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "probed": true, "live": true})
}

// handlePromptsGet returns the merged catalogue and every stored override with
// its status, which is what an editing UI needs to show shipped-vs-mine.
func (s *Server) handlePromptsGet(w http.ResponseWriter, r *http.Request) {
	cat := s.catalogue(r.Context())
	defaults := store.DefaultCatalogue()
	writeJSON(w, http.StatusOK, map[string]any{
		"plays":     cat.Plays,
		"rules":     cat.Rules,
		"roles":     cat.Roles,
		"solo":      cat.Solo,
		"overrides": cat.Overrides,
		"dropped":   cat.Dropped,
		// The shipped text, so the UI can offer take-the-new-one on a stale
		// override without a second round trip.
		"defaultPlays": defaults.Plays,
		"defaultRules": defaults.Rules,
	})
}

// handlePromptsPut stores one leaf edit, or refuses it with the validator's
// own message. Validation happens HERE, on the merged result, so the daemon
// can never boot into a broken state: a broken state is never stored.
func (s *Server) handlePromptsPut(w http.ResponseWriter, r *http.Request) {
	var o store.PromptOverride
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&o); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON: "+err.Error(), nil)
		return
	}
	saved, err := s.st.PutPromptOverride(r.Context(), o)
	if err != nil {
		if errors.Is(err, store.ErrOverrideInvalid) || errors.Is(err, store.ErrPlayInvalid) {
			writeErr(w, http.StatusBadRequest, "invalid_override", err.Error(), nil)
			return
		}
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"override": saved})
}

// handlePromptsDelete removes one edit, restoring the shipped text.
func (s *Server) handlePromptsDelete(w http.ResponseWriter, r *http.Request) {
	ok, err := s.st.DeletePromptOverride(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such override", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// loopTitle names the loop. An omitted title falls back to the head of the
// instruction rather than refusing: the name is for the engineer's eye and a
// derived one is better than a form that will not submit.
func loopTitle(title, task string) string {
	if t := strings.Join(strings.Fields(title), " "); t != "" {
		return t
	}
	words := strings.Fields(task)
	if len(words) > 6 {
		words = words[:6]
	}
	return strings.Join(words, " ")
}

func orString(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// refuseUnknownModels checks each member's model against the live catalogue
// for its tool, and says nothing when there is no catalogue to check against.
//
// A lister error is NOT a refusal. The machine may have no opencode, or the
// probe may have timed out, and neither is evidence that the model is wrong.
func (s *Server) refuseUnknownModels(ctx context.Context, crew []store.MemberSpec) error {
	if s.models == nil {
		return nil
	}
	catalogues := map[string]map[string]bool{}
	for _, m := range crew {
		tool := store.NormaliseTool(m.Tool)
		model := strings.TrimSpace(m.Model)
		if model == "" || tool != store.ToolOpenCode {
			continue
		}
		known, seen := catalogues[tool]
		if !seen {
			lookCtx, cancel := context.WithTimeout(ctx, catalogueBudget)
			out, err := s.models.Models(lookCtx, tool, "")
			cancel()
			if err != nil || len(out) == 0 {
				catalogues[tool] = nil
				continue
			}
			known = map[string]bool{}
			for _, e := range out {
				known[strings.ToLower(e.ID)] = true
			}
			catalogues[tool] = known
		}
		if known == nil {
			continue
		}
		if !known[strings.ToLower(model)] {
			return fmt.Errorf("%s does not run %q on this machine; ask the model picker what it does run", tool, model)
		}
	}
	return nil
}
