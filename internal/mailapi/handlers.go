// handlers.go — the agent routes. The inbox is the one that matters: read its
// loop next to the package comment before changing it.

package mailapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

type inboxMessage struct {
	ID            string  `json:"id"`
	FromRole      string  `json:"fromRole"`
	FromAgentID   string  `json:"fromAgentId,omitempty"`
	Subject       string  `json:"subject,omitempty"`
	Body          *string `json:"body,omitempty"`
	BodyChars     int     `json:"bodyChars"`
	StepID        string  `json:"stepId,omitempty"`
	Outcome       string  `json:"outcome,omitempty"`
	Redelivered   bool    `json:"redelivered,omitempty"`
	ForwardedFrom string  `json:"forwardedFrom,omitempty"`
	LeaseSeconds  int     `json:"leaseSeconds,omitempty"`
	CreatedAt     int64   `json:"createdAt"`
}

// renderMessage omits the body entirely when bodies are not wanted. It never
// cuts one: you either have the whole message or a pointer to it.
func renderMessage(m *store.LoopMessage, bodies bool) inboxMessage {
	out := inboxMessage{
		ID: m.ID, FromRole: m.SenderRole, FromAgentID: m.SenderID,
		Subject: m.Subject, BodyChars: m.BodyChars, StepID: m.StepID,
		Outcome: m.Outcome, Redelivered: m.Redelivered,
		ForwardedFrom: m.ForwardedFrom, LeaseSeconds: m.LeaseSeconds,
		CreatedAt: m.CreatedAt,
	}
	if bodies {
		body := m.Body
		out.Body = &body
	}
	return out
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	member, loop := memberFrom(r.Context()), loopFrom(r.Context())
	brief, _ := s.st.DeliveredStepBrief(r.Context(), loop)
	notes, err := s.st.CountLoopNotes(r.Context(), loop.ID, member.Role)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agentId":             member.ID,
		"role":                member.Role,
		"loopId":              loop.ID,
		"task":                loop.Task,
		"cwd":                 loop.CWD,
		"round":               loop.Round,
		"state":               loop.State,
		"play":                loop.Play,
		"playStatus":          loop.PlayStatus,
		"stepId":              loop.StepID,
		"stepBrief":           brief,
		"stepAwaiting":        loop.StepAwaiting,
		"notesOnRecord":       notes,
		"charsIn":             member.CharsIn,
		"pollIntervalSeconds": member.PollIntervalSeconds,
		"signal":              signalFor(member, loop),
	})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	member, loop := memberFrom(r.Context()), loopFrom(r.Context())
	roster, err := s.st.ListLoopMembers(r.Context(), loop.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	counts, err := s.st.CountMailByStatus(r.Context(), loop.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	brief, _ := s.st.DeliveredStepBrief(r.Context(), loop)
	crew := make([]map[string]any, 0, len(roster))
	for _, m := range roster {
		crew = append(crew, map[string]any{
			"role": m.Role, "agentId": m.ID, "status": m.Status,
			"charsIn": m.CharsIn, "lastSeenAt": m.LastSeenAt,
		})
	}
	out := map[string]any{
		"loopId": loop.ID, "task": loop.Task, "round": loop.Round, "state": loop.State,
		"play": loop.Play, "playStatus": loop.PlayStatus, "stepId": loop.StepID,
		"stepBrief": brief, "stepAwaiting": loop.StepAwaiting,
		"roster": crew, "messageCounts": counts,
		"signal": signalFor(member, loop),
	}
	if play, ok := store.LookupPlay(loop.Play); ok {
		steps := make([]map[string]any, 0, len(play.Steps))
		for i := range play.Steps {
			steps = append(steps, map[string]any{
				"id": play.Steps[i].ID, "actor": play.Steps[i].Actor,
				"note": play.Steps[i].Note, "fanIn": play.Steps[i].FanIn,
			})
		}
		out["playSteps"] = steps
		out["maxRounds"] = play.EffectiveMaxRounds()
	}
	writeJSON(w, http.StatusOK, out)
}

// handleInbox is the long poll. It holds NO database connection while it
// waits: one cheap EXISTS, then park on the store's wake-up channel, then
// loop. Four connections in the pool and five-minute polls means a handler
// that waits with a connection in hand deadlocks everything on the fourth
// agent, so this shape is the requirement, not a preference.
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	member, loop := memberFrom(r.Context()), loopFrom(r.Context())
	out, ok := s.waitInbox(w, r, member, loop)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// waitInbox is the long poll itself, extracted so a report can re-enter the
// queue in the same call that posted it. Returns false when it has already
// written a response or the client went away.
//
// It holds NO database connection while it waits: one cheap EXISTS, then park
// on the store's wake-up channel, then loop. Four connections in the pool and
// five-minute polls means a handler that waits with a connection in hand
// deadlocks everything on the fourth agent, so this shape is the requirement,
// not a preference.
func (s *Server) waitInbox(
	w http.ResponseWriter, r *http.Request, member *store.LoopMember, loop *store.Loop,
) (map[string]any, bool) {
	ctx := r.Context()

	// Presence. Entered here and released on EVERY exit including a dropped
	// connection, which is what makes "listening" mean a line is actually
	// held rather than a column somebody forgot to clear.
	leave := s.st.EnterPolling(member.ID)
	defer leave()
	// Reading the inbox is the one kind of contact that answers "is anyone
	// reading this mailbox". Stamped on the way IN and again on the way out,
	// so a five minute park does not look like five minutes of silence, and a
	// zero-wait claim counts because it did read.
	s.markPolled(ctx, member.ID)
	defer s.markPolled(context.WithoutCancel(ctx), member.ID)
	// Polling again is the member answering for itself, so any earlier
	// verdict that nobody was reading its mail is now wrong.
	if member.StrandedSince != 0 {
		if err := s.st.ClearStranded(ctx, member.ID); err != nil {
			s.log.Warn("mailapi: clear stranded", "member", member.ID, "err", err)
		}
	}

	wait := time.Duration(intParam(r, "wait", 0)) * time.Second
	if wait > s.cfg.MaxWait {
		wait = s.cfg.MaxWait
	}
	if wait < 0 {
		wait = 0
	}
	bodies := boolParam(r, "bodies", true)
	autoAck := boolParam(r, "auto_ack", false)
	limit := intParam(r, "limit", 20)
	deadline := time.Now().Add(wait)

	for {
		sig := signalFor(member, loop)
		halted := sig.Stop || loop.Ended()

		out := []inboxMessage{}
		if !halted {
			has, err := s.st.HasClaimableMail(ctx, member, nil)
			if err != nil {
				writeStoreErr(w, err)
				return nil, false
			}
			if has {
				claimed, err := s.st.ClaimInbox(ctx, member, store.ClaimOptions{
					Limit: limit, AutoAck: autoAck,
				})
				if err != nil {
					writeStoreErr(w, err)
					return nil, false
				}
				for _, m := range claimed {
					out = append(out, renderMessage(m, bodies))
				}
			}
		}

		if len(out) > 0 || halted || !time.Now().Before(deadline) {
			return map[string]any{
				"agentId":             member.ID,
				"role":                member.Role,
				"loopId":              loop.ID,
				"pollIntervalSeconds": member.PollIntervalSeconds,
				"messages":            out,
				"signal":              sig,
				"stop":                sig.Stop,
				"idle":                sig.Idle,
			}, true
		}

		// Park with no connection held. The channel is closed by the store
		// the moment a row is committed for this member; the tick is only a
		// safety net for a wake-up that raced the registration.
		woke, release := s.st.WaitForMail(member.ID)
		timer := time.NewTimer(s.cfg.PollTick)
		select {
		case <-woke:
		case <-timer.C:
		case <-ctx.Done():
			release()
			timer.Stop()
			return nil, false
		}
		release()
		timer.Stop()

		// Re-read identity occasionally so a dismissal or an ending loop
		// reaches a parked agent rather than waiting out the whole poll.
		fresh, freshLoop, err := s.st.LoopMemberByToken(ctx, r.Header.Get("X-Agent-Token"))
		if err == nil && fresh != nil && freshLoop != nil {
			member, loop = fresh, freshLoop
		}
	}
}

type postMessageRequest struct {
	To           string `json:"to"`
	Body         string `json:"body"`
	Forward      string `json:"forward"`
	Subject      string `json:"subject"`
	Outcome      string `json:"outcome"`
	LeaseSeconds int    `json:"leaseSeconds"`
}

// notesRequired is the refusal a worker gets for reporting with nothing on
// record, carried as fields rather than prose.
type notesRequired struct {
	Role          string `json:"role"`
	LoopID        string `json:"loopId"`
	NotesOnRecord int    `json:"notesOnRecord"`
}

// quoteRefused names the message the body retyped, and the id to forward.
type quoteRefused struct {
	MessageID  string `json:"messageId"`
	SenderRole string `json:"senderRole"`
	BodyChars  int    `json:"bodyChars"`
	ForwardAs  string `json:"forwardAs"`
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	member, loop := memberFrom(r.Context()), loopFrom(r.Context())
	if !s.requireWritable(w, member, loop) {
		return
	}
	var req postMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON: "+err.Error(), nil)
		return
	}
	if (req.Body == "") == (req.Forward == "") {
		writeErr(w, http.StatusBadRequest, "bad_request", "send exactly one of body or forward", nil)
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

	if req.Forward != "" {
		msg, err := s.st.ForwardMail(r.Context(), member, recipient, store.MailForward{
			OriginalID: req.Forward, Subject: req.Subject,
			LeaseSeconds: req.LeaseSeconds, Outcome: req.Outcome,
		})
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		s.replyWithInbox(w, r, member, loop, msg)
		return
	}

	if runes(req.Body) > s.cfg.MaxBodyChars {
		writeErr(w, http.StatusRequestEntityTooLarge, "body_too_large",
			"body is longer than the cap; split the work, never the message", nil)
		return
	}
	if err := s.guardNotes(r, member, recipient, loop); err != nil {
		writeGuard(w, err)
		return
	}
	if err := s.guardQuote(r, member, loop, req.Body); err != nil {
		writeGuard(w, err)
		return
	}

	msg, err := s.st.PostMail(r.Context(), member, recipient, store.MailPost{
		Subject: req.Subject, Body: req.Body,
		LeaseSeconds: req.LeaseSeconds, Outcome: req.Outcome,
		IdempotencyKey: idempotencyKey(r),
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	s.replyWithInbox(w, r, member, loop, msg)
}

// replyWithInbox answers a post, and when the caller asked to wait, re-enters
// its own queue in the same call.
//
// This is what makes re-entry unforgettable. Waiting again used to be a
// separate command issued at the exact moment a model feels finished, which is
// the moment it stops following instructions; folding it into the report means
// there is no second step to skip. The post is committed before the wait
// begins, so a connection dying during the wait never loses the report.
func (s *Server) replyWithInbox(
	w http.ResponseWriter, r *http.Request,
	member *store.LoopMember, loop *store.Loop, msg *store.LoopMessage,
) {
	posted := postedMessage(msg)
	if r.URL.Query().Get("wait") == "" {
		writeJSON(w, http.StatusOK, posted)
		return
	}
	// Re-read: the post may have advanced the step and queued this member's
	// next brief, and waiting on a stale roster row would miss a dismissal.
	fresh, freshLoop, err := s.st.LoopMemberByToken(r.Context(), r.Header.Get("X-Agent-Token"))
	if err == nil && fresh != nil && freshLoop != nil {
		member, loop = fresh, freshLoop
	}
	inbox, ok := s.waitInbox(w, r, member, loop)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"posted": posted, "inbox": inbox})
}

func (s *Server) markPolled(ctx context.Context, memberID string) {
	if err := s.st.MarkPolled(ctx, memberID, time.Now().UnixMilli()); err != nil {
		s.log.Warn("mailapi: mark polled", "member", memberID, "err", err)
	}
}

// idempotencyKey reads the client's retry key. Bounded, because it is stored.
func idempotencyKey(r *http.Request) string {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > 200 {
		return key[:200]
	}
	return key
}

func postedMessage(m *store.LoopMessage) map[string]any {
	return map[string]any{
		"id": m.ID, "loopId": m.LoopID, "to": m.RecipientRole, "toAgentId": m.RecipientID,
		"bodyChars": m.BodyChars, "forwardedFrom": m.ForwardedFrom,
		"outcome": m.Outcome, "stepId": m.StepID, "createdAt": m.CreatedAt,
	}
}

// guardErr carries a policy refusal with its typed detail.
type guardErr struct {
	code    string
	message string
	details any
}

func (e *guardErr) Error() string { return e.message }

func writeGuard(w http.ResponseWriter, err error) {
	var g *guardErr
	if errors.As(err, &g) {
		writeErr(w, http.StatusConflict, g.code, g.message, g.details)
		return
	}
	writeStoreErr(w, err)
}

// guardNotes refuses a worker report with nothing on record. A worker's
// session can be retired the moment it delivers, so the notes are all the role
// keeps.
func (s *Server) guardNotes(r *http.Request, member, recipient *store.LoopMember, loop *store.Loop) error {
	if s.cfg.RelaxNotesBeforeReport {
		return nil
	}
	if store.BaseRole(member.Role) == store.RoleOrchestrator || store.BaseRole(member.Role) == store.RoleEngineer {
		return nil
	}
	if store.BaseRole(recipient.Role) != store.RoleOrchestrator {
		return nil
	}
	n, err := s.st.CountLoopNotes(r.Context(), loop.ID, member.Role)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return &guardErr{
		code: "notes_required",
		message: "Reporting with no notes on record is refused. Write what you established as a note first: " +
			"your session can be retired the moment you deliver, and the notes are all your role keeps.",
		details: notesRequired{Role: member.Role, LoopID: loop.ID, NotesOnRecord: 0},
	}
}

// guardQuote refuses a body that contains an earlier message in full, naming
// the id to forward instead. Verbatim stays verbatim and briefs stay short.
func (s *Server) guardQuote(r *http.Request, member *store.LoopMember, loop *store.Loop, body string) error {
	if s.cfg.QuoteGuardMinimum <= 0 {
		return nil
	}
	lineage, err := s.st.LoopLineage(r.Context(), loop.ID)
	if err != nil {
		return err
	}
	quoted, err := s.st.FindQuotedMail(r.Context(), lineage, body, s.cfg.QuoteGuardMinimum)
	if err != nil {
		return err
	}
	if quoted == nil {
		return nil
	}
	return &guardErr{
		code: "quote_refused",
		message: "This body contains message " + quoted.ID + " in full. Forward it by id instead: " +
			"the body is copied server side, byte for byte, and retyping a report is how evidence gets lost.",
		details: quoteRefused{
			MessageID: quoted.ID, SenderRole: quoted.SenderRole,
			BodyChars: quoted.BodyChars, ForwardAs: quoted.ID,
		},
	}
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	loop := loopFrom(r.Context())
	lineage, err := s.st.LoopLineage(r.Context(), loop.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	msg, err := s.st.FindLoopMessage(r.Context(), lineage, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if msg == nil {
		writeErr(w, http.StatusNotFound, "not_found",
			"no message "+r.PathValue("id")+" in this loop or the loops it came from", nil)
		return
	}
	writeJSON(w, http.StatusOK, renderMessage(msg, true))
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	member, loop := memberFrom(r.Context()), loopFrom(r.Context())
	if !s.requireWritable(w, member, loop) {
		return
	}
	ok, err := s.st.AckMail(r.Context(), member.ID, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found",
			"no unacked message "+r.PathValue("id")+" addressed to you", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": r.PathValue("id")})
}

func (s *Server) handleExtend(w http.ResponseWriter, r *http.Request) {
	member, loop := memberFrom(r.Context()), loopFrom(r.Context())
	if !s.requireWritable(w, member, loop) {
		return
	}
	var req struct {
		Seconds int `json:"seconds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ok, err := s.st.ExtendMailLease(r.Context(), member.ID, r.PathValue("id"), req.Seconds)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found",
			"no message "+r.PathValue("id")+" held by you to extend", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": r.PathValue("id")})
}

func (s *Server) handleGetNotes(w http.ResponseWriter, r *http.Request) {
	member, loop := memberFrom(r.Context()), loopFrom(r.Context())
	role := r.URL.Query().Get("role")
	if role == "" {
		role = member.Role
	}
	notes, err := s.st.LoopNotes(r.Context(), loop.ID, role, intParam(r, "limit", 0))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"role": role, "notes": notes})
}

func (s *Server) handlePostNote(w http.ResponseWriter, r *http.Request) {
	member, loop := memberFrom(r.Context()), loopFrom(r.Context())
	if !s.requireWritable(w, member, loop) {
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Body) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "a note needs a body", nil)
		return
	}
	note, err := s.st.AppendLoopNote(r.Context(), loop.ID, member.Role, member.ID, req.Body)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, note)
}

func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())
	msgs, err := s.st.MailThread(r.Context(), member.ID, intParam(r, "limit", 0))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	bodies := boolParam(r, "bodies", true)
	out := make([]inboxMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, renderMessage(m, bodies))
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out})
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	loop := loopFrom(r.Context())
	lineage, err := s.st.LoopLineage(r.Context(), loop.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	index, err := s.st.MailArchive(r.Context(), lineage, intParam(r, "limit", 0))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lineage": lineage, "messages": index})
}
