// events_test.go — tool and model as validated data, the engineer mirror, and
// loop activity on the existing event pipe. The refusal events matter most: a
// step violation the UI can render is enforcement made visible, where a silent
// 409 is enforcement nobody sees.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// loggedEvent is one loop_events row as the tests read it back.
type loggedEvent struct {
	Seq     int64
	Kind    string
	LoopID  string
	Content string
	TS      int64
	Detail  map[string]any
}

// loopEvents reads every loop event, oldest first.
func loopEvents(t *testing.T, s *Store) []loggedEvent {
	t.Helper()
	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT seq, loop_id, kind, content, detail, created_at FROM loop_events ORDER BY seq`)
	if err != nil {
		t.Fatalf("loop_events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := []loggedEvent{}
	for rows.Next() {
		var e loggedEvent
		var detail string
		if err := rows.Scan(&e.Seq, &e.LoopID, &e.Kind, &e.Content, &detail, &e.TS); err != nil {
			t.Fatalf("loop_events scan: %v", err)
		}
		_ = json.Unmarshal([]byte(detail), &e.Detail)
		out = append(out, e)
	}
	return out
}

func kinds(events []loggedEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func hasKind(events []loggedEvent, kind string) *loggedEvent {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

// An unknown tool or model pairing is refused at creation, not discovered at
// launch. The opencode side is unprobed and says so rather than pretending.
func TestToolAndModelAreValidatedAtCreation(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	if err := ValidateToolModel(ToolClaude, "opus"); err != nil {
		t.Fatalf("a known pairing must be accepted: %v", err)
	}
	if err := ValidateToolModel(ToolClaude, ""); err != nil {
		t.Fatalf("claude may take its own default: %v", err)
	}
	if err := ValidateToolModel(ToolClaude, "gpt-4"); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("claude does not run gpt-4: %v", err)
	}
	if err := ValidateToolModel("emacs", "opus"); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("an unknown tool must be refused: %v", err)
	}
	// Unprobed still means the SET is unknown, but the SHAPE is not. A bare
	// id is not an error to opencode: it resolves to nothing and the member
	// runs on the default, which is how "minimax" reached a spawn.
	if err := ValidateToolModel(ToolOpenCode, "minimax"); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("a bare id must be refused at creation: %v", err)
	}
	if err := ValidateToolModel(ToolOpenCode, "anthropic/claude-sonnet-4-5"); err != nil {
		t.Fatalf("a well-shaped model must be accepted by an unprobed tool: %v", err)
	}
	// An openrouter route has slashes in the model half; only the first is
	// structural, so this must not be mistaken for a malformed ref.
	if err := ValidateToolModel(ToolOpenCode, "openrouter/anthropic/claude-3"); err != nil {
		t.Fatalf("a routed model must be accepted: %v", err)
	}
	// The empty model is opencode's own default, which is a real answer. It
	// was refused as a registry policy, and that policy forced a guess.
	if err := ValidateToolModel(ToolOpenCode, ""); err != nil {
		t.Fatalf("an empty model must mean opencode's own default: %v", err)
	}
	spec, ok := LookupTool(ToolOpenCode)
	if !ok || spec.Probed || spec.Models != nil {
		t.Fatalf("the registry must not list guesses: %+v", spec)
	}
	if !spec.DefaultModelAllowed {
		t.Fatalf("opencode must permit its own default: %+v", spec)
	}
	if !strings.Contains(spec.Note, "this machine") {
		t.Fatalf("the note must say where the list comes from: %q", spec.Note)
	}

	loop := &Loop{Task: "tooling"}
	if _, err := s.CreateCrew(ctx, loop, []MemberSpec{
		{Role: RoleOrchestrator, Tool: ToolClaude, Model: "opus"},
		{Role: "REVIEW", Tool: ToolClaude, Model: "not-a-model"},
	}); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("a bad pairing must fail the whole creation: %v", err)
	}
	if got, _ := s.GetLoop(ctx, loop.ID); got != nil {
		t.Fatal("a refused creation must leave no loop behind")
	}

	good := &Loop{Task: "tooling"}
	if _, err := s.CreateCrew(ctx, good, []MemberSpec{
		{Role: RoleOrchestrator, Tool: ToolClaude, Model: "opus"},
		{Role: RoleEngineer},
		{Role: "REVIEW", Tool: ToolOpenCode, Model: "anthropic/some-opencode-model"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	roster, _ := s.ListLoopMembers(ctx, good.ID)
	byRole := map[string]*LoopMember{}
	for _, m := range roster {
		byRole[m.Role] = m
	}
	if byRole[RoleOrchestrator].Tool != ToolClaude || byRole[RoleOrchestrator].Model != "opus" {
		t.Fatalf("tool and model must round-trip: %+v", byRole[RoleOrchestrator])
	}
	if byRole["REVIEW"].Tool != ToolOpenCode {
		t.Fatalf("review tool = %q", byRole["REVIEW"].Tool)
	}
	// The engineer is an address, not a process, so it runs on nothing.
	if byRole[RoleEngineer].Tool != "" || byRole[RoleEngineer].Model != "" {
		t.Fatalf("the engineer must run on no tool: %+v", byRole[RoleEngineer])
	}
}

// The engineer is a participant. A worker may narrate to it and it may
// instruct a worker, and BOTH directions are mirrored to the orchestrator, so
// reaching past the orchestrator never means going behind it.
func TestEngineerTrafficIsMirroredToTheOrchestrator(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "mirroring")
	orch, inv, engineer := roles[RoleOrchestrator], roles["INVESTIGATION"], roles[RoleEngineer]

	instruction := mustPost(t, s, engineer, inv, "stop what you are doing and check the export")
	report := mustPost(t, s, inv, engineer, "still digging, nothing yet")
	direct := mustPost(t, s, engineer, orch, "how is it going")

	transcript, err := s.ListLoopMessages(ctx, loop.ID, 0)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	mirrors := map[string]*LoopMessage{}
	for _, m := range transcript {
		if m.MirrorOf != "" {
			mirrors[m.MirrorOf] = m
		}
	}
	if len(mirrors) != 2 {
		t.Fatalf("both engineer-to-worker and worker-to-engineer must mirror, got %d", len(mirrors))
	}
	for _, original := range []*LoopMessage{instruction, report} {
		mirror := mirrors[original.ID]
		if mirror == nil {
			t.Fatalf("no mirror for %s (%s to %s)", original.ID, original.SenderRole, original.RecipientRole)
		}
		if mirror.RecipientRole != RoleOrchestrator {
			t.Fatalf("a mirror goes to the orchestrator, got %s", mirror.RecipientRole)
		}
		if mirror.Body != original.Body {
			t.Fatal("a mirror carries the whole body, not a summary of it")
		}
		if !strings.HasPrefix(mirror.Subject, "mirror:") {
			t.Fatalf("a mirror must be distinguishable from a report: %q", mirror.Subject)
		}
	}
	if mirrors[direct.ID] != nil {
		t.Fatal("the orchestrator was already a party; that must not be mirrored to itself")
	}

	// A mirror is for SIGHT, not for action. The orchestrator is the most
	// context-expensive member in the loop and pays its whole context on
	// every wake, so a mirror must never be one of the things that wakes it.
	claimed, err := s.ClaimInbox(ctx, orch, ClaimOptions{Limit: 50})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, m := range claimed {
		if m.MirrorOf != "" {
			t.Fatalf("a mirror must never be deliverable: %s woke the orchestrator", m.ID)
		}
	}
	// Exactly one thing here WAS addressed to the orchestrator, the
	// engineer's direct question, and that one still wakes it.
	if len(claimed) != 1 || claimed[0].ID != direct.ID {
		t.Fatalf("the inbox must hold the one message addressed to it and nothing else, got %d", len(claimed))
	}
	for _, m := range mirrors {
		if m.Status != MailMirrored {
			t.Fatalf("a mirror must carry the undeliverable status, got %q", m.Status)
		}
	}

	// It is still fully visible to anyone who looks: the transcript, the
	// orchestrator's own thread, and the archive index.
	thread, err := s.MailThread(ctx, orch.ID, 0)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	inThread := 0
	for _, m := range thread {
		if m.MirrorOf != "" {
			inThread++
			if m.Body == "" {
				t.Fatal("a mirror in the thread must carry its whole body")
			}
		}
	}
	if inThread != 2 {
		t.Fatalf("the orchestrator's thread must show both mirrors, got %d", inThread)
	}
	index, err := s.MailArchive(ctx, []string{loop.ID}, 0)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	archived := 0
	for _, e := range index {
		if e.Status == MailMirrored {
			archived++
		}
	}
	if archived != 2 {
		t.Fatalf("the archive must index both mirrors, got %d", archived)
	}

	// Worker to worker is still refused: the engineer is an address, not a
	// hole in the routing rule.
	if _, err := s.PostMail(ctx, inv, roles["REVIEW"], MailPost{Body: "psst"}); !errors.Is(err, ErrMailRouteRefused) {
		t.Fatalf("worker to worker must still be refused: %v", err)
	}
}

// Loop activity is recorded in the same transaction as the row it describes.
func TestLoopActivityIsPublishedOnTheEventPipe(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "events")
	orch, inv := roles[RoleOrchestrator], roles["INVESTIGATION"]

	if created := hasKind(loopEvents(t, s), EvLoopCreated); created == nil {
		t.Fatalf("creating a loop must publish an event, got %v", kinds(loopEvents(t, s)))
	} else if created.LoopID != loop.ID || created.TS == 0 {
		t.Fatalf("event payload: %+v", created)
	}

	if _, err := s.StartPlay(ctx, loop.ID, "recon"); err != nil {
		t.Fatalf("start play: %v", err)
	}
	if hasKind(loopEvents(t, s), EvPlayStarted) == nil {
		t.Fatalf("starting a play must publish an event, got %v", kinds(loopEvents(t, s)))
	}

	posted := mustPost(t, s, orch, inv, "go and look")
	events := loopEvents(t, s)
	mail := hasKind(events, EvMailPosted)
	if mail == nil {
		t.Fatalf("a message must publish an event, got %v", kinds(events))
	}
	detail := mail.Detail
	if detail["messageId"] != posted.ID || detail["toRole"] != "INVESTIGATION" {
		t.Fatalf("the event must carry the message: %v", detail)
	}
	if hasKind(events, EvStepAdvanced) == nil {
		t.Fatalf("advancing a step must publish an event, got %v", kinds(events))
	}

	if _, err := s.AckMail(ctx, inv.ID, posted.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if hasKind(loopEvents(t, s), EvMailAcked) == nil {
		t.Fatal("an ack must publish an event")
	}

	if _, err := s.EndLoop(ctx, loop.ID, LoopComplete, "done", false); err != nil {
		t.Fatalf("end: %v", err)
	}
	if ended := hasKind(loopEvents(t, s), EvLoopEnded); ended == nil {
		t.Fatal("ending a loop must publish an event")
	}
}

// A refusal is the event that matters most, and it has to survive the
// transaction that rolled the message back.
func TestRefusalsArePublishedEvenThoughTheMessageIsNot(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "refusals")
	orch, inv, review := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["REVIEW"]
	if _, err := s.StartPlay(ctx, loop.ID, "audit"); err != nil {
		t.Fatalf("start play: %v", err)
	}

	before, err := s.ListLoopMessages(ctx, loop.ID, 0)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if _, err := s.PostMail(ctx, orch, review, MailPost{Body: "skipping ahead"}); !errors.Is(err, ErrMailStepViolation) {
		t.Fatalf("expected a step violation: %v", err)
	}
	after, _ := s.ListLoopMessages(ctx, loop.ID, 0)
	if len(after) != len(before) {
		t.Fatal("a refused message must not be delivered")
	}
	refused := hasKind(loopEvents(t, s), EvStepRefused)
	if refused == nil {
		t.Fatalf("the refusal must be published, got %v", kinds(loopEvents(t, s)))
	}
	detail := refused.Detail
	if detail["play"] != "audit" || detail["stepId"] != "brief" {
		t.Fatalf("the refusal event must carry the violation's fields: %v", detail)
	}
	if detail["toRole"] != "REVIEW" {
		t.Fatalf("the refusal event must name both parties: %v", detail)
	}

	// A routing refusal writes no row at all and still surfaces.
	if _, err := s.PostMail(ctx, inv, review, MailPost{Body: "psst"}); !errors.Is(err, ErrMailRouteRefused) {
		t.Fatalf("expected a route refusal: %v", err)
	}
	if hasKind(loopEvents(t, s), EvRouteRefused) == nil {
		t.Fatalf("a route refusal must be published, got %v", kinds(loopEvents(t, s)))
	}
}

// Every loop event carries the sentence the board shows, the loop it belongs
// to and when it happened.
func TestLoopEventsCarryTheirSentence(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	loop, _ := mustLoop(t, s, "event shape")

	created := hasKind(loopEvents(t, s), EvLoopCreated)
	if created == nil {
		t.Fatal("no loop event reached loop_events")
	}
	if created.LoopID != loop.ID || created.Content == "" || created.TS == 0 {
		t.Fatalf("the row must carry its loop, its sentence and its time: %+v", created)
	}
}

// Refusals are rows, not only events: a stall diagnosed a day later has to
// read the same as one watched live, and that is only true if the refusal was
// written down.
func TestRefusalsAreDurableRowsNotOnlyEvents(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	loop, roles := mustLoop(t, s, "durable refusals")
	orch, inv, review := roles[RoleOrchestrator], roles["INVESTIGATION"], roles["REVIEW"]
	if _, err := s.StartPlay(ctx, loop.ID, "audit"); err != nil {
		t.Fatalf("start play: %v", err)
	}

	if _, err := s.PostMail(ctx, orch, review, MailPost{Body: "skipping ahead"}); !errors.Is(err, ErrMailStepViolation) {
		t.Fatalf("expected a step violation: %v", err)
	}
	if _, err := s.PostMail(ctx, inv, review, MailPost{Body: "psst"}); !errors.Is(err, ErrMailRouteRefused) {
		t.Fatalf("expected a route refusal: %v", err)
	}

	refusals, err := s.ListLoopRefusals(ctx, loop.ID, 0)
	if err != nil {
		t.Fatalf("refusals: %v", err)
	}
	if len(refusals) != 2 {
		t.Fatalf("both refusals must be kept as rows, got %d", len(refusals))
	}
	step := refusals[0]
	if step.Kind != EvStepRefused {
		t.Fatalf("first refusal kind = %q", step.Kind)
	}
	if step.Play != "audit" || step.StepID != "brief" {
		t.Fatalf("the row must carry the step it violated: %+v", step)
	}
	if step.FromRole != RoleOrchestrator || step.ToRole != "REVIEW" {
		t.Fatalf("the row must name both parties: %+v", step)
	}
	if step.Detail["reason"] == nil || step.Detail["allowed"] == nil {
		t.Fatalf("the row must carry the whole typed detail, so the card drawn from history is the card drawn live: %v", step.Detail)
	}
	if step.Content == "" {
		t.Fatal("the row must carry the rendered sentence")
	}
	if refusals[1].Kind != EvRouteRefused {
		t.Fatalf("second refusal kind = %q", refusals[1].Kind)
	}
	// A loop with no refusals has none, rather than everyone else's.
	other, _ := mustLoop(t, s, "quiet")
	if got, _ := s.ListLoopRefusals(ctx, other.ID, 0); len(got) != 0 {
		t.Fatalf("refusals are per loop, got %d", len(got))
	}
}
