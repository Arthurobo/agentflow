// policy_test.go — the two policies that belong at this layer, the structured
// refusals, and the step brief actually reaching an agent.
package mailapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/store"
)

func TestAgentRoutesRoundTrip(t *testing.T) {
	h := newHarness(t, nil, Config{})
	orch, inv := h.tokens["ORCHESTRATOR"], h.tokens["INVESTIGATION"]

	_, who := h.do(inv, "GET", "/whoami", nil)
	if who["role"] != "INVESTIGATION" || who["task"] != "Fix the flaky guest export" {
		t.Fatalf("whoami: %v", who)
	}
	if who["loopId"] != h.loop.ID || who["notesOnRecord"].(float64) != 0 {
		t.Fatalf("whoami: %v", who)
	}

	_, state := h.do(inv, "GET", "/state", nil)
	if state["loopId"] != h.loop.ID {
		t.Fatalf("state: %v", state)
	}
	if len(state["roster"].([]any)) != 5 {
		t.Fatalf("roster has %d members", len(state["roster"].([]any)))
	}

	if resp, note := h.do(inv, "POST", "/notes", map[string]any{"body": "the export is not the bug"}); resp.StatusCode != http.StatusOK || note["id"] == nil {
		t.Fatalf("note: %d %v", resp.StatusCode, note)
	}
	if resp, _ := h.do(inv, "POST", "/notes", map[string]any{"body": "  "}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an empty note must be refused, got %d", resp.StatusCode)
	}
	_, notes := h.do(inv, "GET", "/notes", nil)
	if len(notes["notes"].([]any)) != 1 {
		t.Fatalf("notes: %v", notes)
	}

	_, posted := h.do(orch, "POST", "/messages", map[string]any{
		"to": "INVESTIGATION", "subject": "round one", "body": "go and look",
	})
	id := posted["id"].(string)

	_, inbox := h.do(inv, "GET", "/inbox?wait=0", nil)
	msgs := inbox["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["body"] != "go and look" {
		t.Fatalf("inbox: %v", inbox)
	}

	if resp, _ := h.do(inv, "POST", "/messages/"+id+"/extend", map[string]any{"seconds": 3600}); resp.StatusCode != http.StatusOK {
		t.Fatalf("extend: %d", resp.StatusCode)
	}
	if resp, _ := h.do(inv, "POST", "/messages/"+id+"/ack", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("ack: %d", resp.StatusCode)
	}
	if resp, _ := h.do(inv, "POST", "/messages/"+id+"/ack", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a second ack must be a miss, got %d", resp.StatusCode)
	}
	if resp, _ := h.do(inv, "POST", "/messages/msg_nope/extend", map[string]any{"seconds": 60}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("extending an unknown message must 404, got %d", resp.StatusCode)
	}

	_, thread := h.do(inv, "GET", "/thread", nil)
	if len(thread["messages"].([]any)) != 1 {
		t.Fatalf("thread: %v", thread)
	}
	_, archive := h.do(orch, "GET", "/archive", nil)
	entries := archive["messages"].([]any)
	if len(entries) != 1 {
		t.Fatalf("archive: %v", archive)
	}
	if _, present := entries[0].(map[string]any)["body"]; present {
		t.Fatal("the archive index must carry no bodies")
	}
	if _, present := entries[0].(map[string]any)["bodyChars"]; !present {
		t.Fatal("the archive index must carry sizes")
	}
}

// A worker reporting with nothing on record is refused. Its session can be
// retired the moment it delivers, so the notes are all the role keeps.
func TestWorkerMustLeaveNotesBeforeReporting(t *testing.T) {
	h := newHarness(t, nil, Config{})
	orch, inv := h.tokens["ORCHESTRATOR"], h.tokens["INVESTIGATION"]

	resp, body := h.do(inv, "POST", "/messages", map[string]any{
		"to": "ORCHESTRATOR", "body": "here is what I found",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("reporting with no notes must be refused, got %d", resp.StatusCode)
	}
	e := errorOf(t, body)
	if e["code"] != "notes_required" {
		t.Fatalf("code = %v", e["code"])
	}
	details := e["details"].(map[string]any)
	if details["role"] != "INVESTIGATION" || details["notesOnRecord"].(float64) != 0 {
		t.Fatalf("the refusal must carry the fields: %v", details)
	}

	h.do(inv, "POST", "/notes", map[string]any{"body": "what I established"})
	if resp, _ := h.do(inv, "POST", "/messages", map[string]any{
		"to": "ORCHESTRATOR", "body": "here is what I found",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("with a note on record the report must go through, got %d", resp.StatusCode)
	}

	// The orchestrator is not held to it: routing, not note-taking, is its job.
	if resp, _ := h.do(orch, "POST", "/messages", map[string]any{
		"to": "REVIEW", "body": "have a look",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("the orchestrator must not need notes to route, got %d", resp.StatusCode)
	}
	h2 := newHarness(t, nil, Config{RelaxNotesBeforeReport: true})
	if resp, _ := h2.do(h2.tokens["REVIEW"], "POST", "/messages", map[string]any{
		"to": "ORCHESTRATOR", "body": "unnoted",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("the policy must be switchable off, got %d", resp.StatusCode)
	}
}

// Retyping a report is refused, and the refusal names the id to forward.
func TestRetypingAReportIsRefused(t *testing.T) {
	h := newHarness(t, nil, Config{})
	orch, inv, review := h.tokens["ORCHESTRATOR"], h.tokens["INVESTIGATION"], h.tokens["REVIEW"]

	report := "F1. the export never had the bug\n" + strings.Repeat("evidence line\n", 200)
	h.do(inv, "POST", "/notes", map[string]any{"body": "noted"})
	_, posted := h.do(inv, "POST", "/messages", map[string]any{"to": "ORCHESTRATOR", "body": report})
	reportID := posted["id"].(string)

	resp, body := h.do(orch, "POST", "/messages", map[string]any{
		"to": "REVIEW", "body": "Please review the following.\n\n" + report,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("retyping a report must be refused, got %d", resp.StatusCode)
	}
	e := errorOf(t, body)
	if e["code"] != "quote_refused" {
		t.Fatalf("code = %v", e["code"])
	}
	details := e["details"].(map[string]any)
	if details["messageId"] != reportID || details["forwardAs"] != reportID {
		t.Fatalf("the refusal must name the id to forward: %v", details)
	}
	if !strings.Contains(e["message"].(string), reportID) {
		t.Fatalf("the message must name the id too: %v", e["message"])
	}

	// Forwarding it is accepted and arrives byte for byte.
	if resp, _ := h.do(orch, "POST", "/messages", map[string]any{
		"to": "REVIEW", "subject": "verbatim", "forward": reportID,
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("forwarding must be accepted, got %d", resp.StatusCode)
	}
	_, inbox := h.do(review, "GET", "/inbox?wait=0", nil)
	got := inbox["messages"].([]any)[0].(map[string]any)
	if got["body"] != report {
		t.Fatal("the forwarded body must be byte identical")
	}
	// A short quote is not a retyped report.
	if resp, _ := h.do(orch, "POST", "/messages", map[string]any{
		"to": "REVIEW", "body": "F1. the export never had the bug",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("a short excerpt must be allowed, got %d", resp.StatusCode)
	}
}

// A refusal is data the UI renders, never a string it has to parse.
func TestRefusalsAreStructuredJSON(t *testing.T) {
	h := newHarness(t, nil, Config{})
	ctx := context.Background()
	orch, inv, review := h.tokens["ORCHESTRATOR"], h.tokens["INVESTIGATION"], h.tokens["REVIEW"]

	if _, err := h.st.StartPlay(ctx, h.loop.ID, "audit"); err != nil {
		t.Fatalf("start play: %v", err)
	}
	resp, body := h.do(orch, "POST", "/messages", map[string]any{
		"to": "REVIEW", "body": "skipping ahead",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("an out-of-step message must be refused, got %d", resp.StatusCode)
	}
	e := errorOf(t, body)
	if e["code"] != "step_violation" {
		t.Fatalf("code = %v", e["code"])
	}
	v := e["details"].(map[string]any)
	for _, field := range []string{"play", "stepId", "stepActor", "fromRole", "toRole", "reason"} {
		if v[field] == nil || v[field] == "" {
			t.Fatalf("the violation must carry %s: %v", field, v)
		}
	}
	if v["play"] != "audit" || v["stepId"] != "brief" {
		t.Fatalf("the violation must name the step it violated: %v", v)
	}
	if len(v["allowed"].([]any)) == 0 {
		t.Fatalf("the violation must say what the step would accept: %v", v)
	}

	// A worker addressing another worker is a route refusal, a different code.
	h.do(inv, "POST", "/notes", map[string]any{"body": "noted"})
	resp, body = h.do(inv, "POST", "/messages", map[string]any{"to": "REVIEW", "body": "psst"})
	if resp.StatusCode != http.StatusForbidden || errorOf(t, body)["code"] != "route_refused" {
		t.Fatalf("worker to worker must be a route refusal: %d %v", resp.StatusCode, body)
	}
	_ = review
}

// The brief is delivered, not merely exposed: an agent that boots after the
// step opened still finds it on its first poll.
func TestStepBriefIsDeliveredOnTheFirstPoll(t *testing.T) {
	h := newHarness(t, nil, Config{})
	ctx := context.Background()
	orch, inv := h.tokens["ORCHESTRATOR"], h.tokens["INVESTIGATION"]

	if _, err := h.st.StartPlay(ctx, h.loop.ID, "recon"); err != nil {
		t.Fatalf("start play: %v", err)
	}
	// The orchestrator holds the entry step, so the entry brief is waiting
	// for it before it has ever polled.
	_, first := h.do(orch, "GET", "/inbox?wait=0", nil)
	msgs := first["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("the entry brief must be waiting: %v", first)
	}
	m := msgs[0].(map[string]any)
	if m["fromRole"] != "ENGINE" || m["stepId"] != "brief" {
		t.Fatalf("the brief must come from the engine and name its step: %v", m)
	}
	play, _ := lookup(t, "recon")
	if m["body"] != play.Step("brief").Brief {
		t.Fatalf("the delivered brief must be the step's brief: %q", m["body"])
	}

	// Advancing the step delivers the next brief to whoever now holds it,
	// again without that agent having asked.
	h.do(orch, "POST", "/messages", map[string]any{"to": "INVESTIGATION", "body": "go and look"})
	_, atInv := h.do(inv, "GET", "/inbox?wait=0", nil)
	got := atInv["messages"].([]any)
	if len(got) != 2 {
		t.Fatalf("the investigator must have both the brief and the orchestrator's message: %v", atInv)
	}
	var engineBrief map[string]any
	for _, raw := range got {
		if raw.(map[string]any)["fromRole"] == "ENGINE" {
			engineBrief = raw.(map[string]any)
		}
	}
	if engineBrief == nil || engineBrief["stepId"] != "investigate" {
		t.Fatalf("the investigate brief must have been queued: %v", got)
	}
	if engineBrief["body"] != play.Step("investigate").Brief {
		t.Fatalf("wrong brief delivered: %q", engineBrief["body"])
	}
}

func lookup(t *testing.T, name string) (*store.Play, bool) {
	t.Helper()
	p, ok := store.LookupPlay(name)
	if !ok {
		t.Fatalf("play %s missing", name)
	}
	return p, ok
}
