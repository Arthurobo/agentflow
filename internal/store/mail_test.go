// mail_test.go — loop, member, note and state contracts of migration 0016:
// hashed tokens, the active/idle/dismissed lifecycle, adoption into a
// successor loop with tokens preserved, role memory, and the state row a
// replacement orchestrator resumes from.
package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reading the database must never let anyone impersonate a member.
func TestMemberTokensAreStoredHashedNotPlaintext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mail.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()

	l := &Loop{Task: "hashing"}
	tokens, err := s.CreateLoop(ctx, l, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	token := tokens[RoleOrchestrator]
	if token == "" {
		t.Fatal("the orchestrator must be issued a token once, at creation")
	}
	if _, ok := tokens[RoleEngineer]; ok {
		t.Fatal("the engineer is an address, not a poller: it must hold no token")
	}

	member, loop, err := s.LoopMemberByToken(ctx, token)
	if err != nil || member == nil || loop == nil {
		t.Fatalf("the issued token must authenticate: %v", err)
	}
	if member.Role != RoleOrchestrator || member.TokenHash != HashToken(token) {
		t.Fatalf("token lookup returned %+v", member)
	}
	if m, _, _ := s.LoopMemberByToken(ctx, ""); m != nil {
		t.Fatal("an empty token must never match a member")
	}
	_ = s.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if strings.Contains(string(raw), token) {
			t.Fatalf("%s contains the plaintext token", e.Name())
		}
	}
}

// Rotation keeps the member id, so a fresh session inherits the notes and the
// thread, and kills the old credential at once.
func TestRotationIssuesANewTokenAndKillsTheOld(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	l := &Loop{Task: "rotation"}
	tokens, err := s.CreateLoop(ctx, l, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	old := tokens["REVIEW"]
	before, _, err := s.LoopMemberByToken(ctx, old)
	if err != nil || before == nil {
		t.Fatalf("seed lookup: %v", err)
	}

	fresh, err := s.RotateLoopMemberToken(ctx, before.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if fresh == old {
		t.Fatal("rotation must issue a different token")
	}
	if dead, _, _ := s.LoopMemberByToken(ctx, old); dead != nil {
		t.Fatal("the old token must stop working immediately")
	}
	after, _, err := s.LoopMemberByToken(ctx, fresh)
	if err != nil || after == nil {
		t.Fatalf("the new token must work: %v", err)
	}
	if after.ID != before.ID {
		t.Fatalf("rotation changed the member id: %s -> %s", before.ID, after.ID)
	}
	if _, err := s.RotateLoopMemberToken(ctx, "nobody"); err == nil {
		t.Fatal("rotating an unknown member must fail")
	}
}

// Ending a loop parks the agents; only an explicit dismiss retires them.
func TestEndingALoopParksMembersAndDismissRetiresThem(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	parked := &Loop{Task: "parking"}
	tokens, err := s.CreateLoop(ctx, parked, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.EndLoop(ctx, parked.ID, LoopComplete, "round one done", false); err != nil {
		t.Fatalf("end: %v", err)
	}
	member, loop, err := s.LoopMemberByToken(ctx, tokens["INVESTIGATION"])
	if err != nil || member == nil {
		t.Fatalf("a parked member keeps a live token: %v", err)
	}
	if member.Status != LoopMemberIdle {
		t.Fatalf("member status = %q, want idle", member.Status)
	}
	stop, idle, reason := LoopMemberSignal(member, loop)
	if stop || !idle || reason == "" {
		t.Fatalf("a parked member must be told to keep waiting: stop=%v idle=%v %q", stop, idle, reason)
	}

	if _, err := s.DismissLoopMembers(ctx, parked.ID); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	member, loop, err = s.LoopMemberByToken(ctx, tokens["INVESTIGATION"])
	if err != nil || member == nil {
		t.Fatalf("lookup after dismiss: %v", err)
	}
	stop, idle, _ = LoopMemberSignal(member, loop)
	if !stop || idle {
		t.Fatalf("a dismissed member must be told to exit: stop=%v idle=%v", stop, idle)
	}

	live := &Loop{Task: "still running"}
	if _, err := s.CreateLoop(ctx, live, nil); err != nil {
		t.Fatalf("create live: %v", err)
	}
	roster, err := s.ListLoopMembers(ctx, live.ID)
	if err != nil || len(roster) == 0 {
		t.Fatalf("roster: %v", err)
	}
	if stop, idle, _ := LoopMemberSignal(roster[0], live); stop || idle {
		t.Fatal("a member of a live loop keeps working")
	}
}

// Agents outlive loops: a successor adopts them with their ids and tokens, and
// leaves the dismissed behind.
func TestSuccessorLoopAdoptsMembersKeepingTheirTokens(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	first := &Loop{Task: "round one"}
	tokens, err := s.CreateLoop(ctx, first, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SetLoopMemberStatus(ctx, first.ID, "REVIEW", LoopMemberDismissed); err != nil {
		t.Fatalf("retire review: %v", err)
	}
	if _, err := s.EndLoop(ctx, first.ID, LoopComplete, "done", false); err != nil {
		t.Fatalf("end: %v", err)
	}
	before, _, err := s.LoopMemberByToken(ctx, tokens["INVESTIGATION"])
	if err != nil || before == nil {
		t.Fatalf("seed lookup: %v", err)
	}

	second := &Loop{Task: "round two", ParentLoopID: first.ID}
	freshTokens, err := s.CreateLoop(ctx, second, nil)
	if err != nil {
		t.Fatalf("create successor: %v", err)
	}
	adopted, err := s.AdoptLoopMembers(ctx, first.ID, second.ID)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if strings.Contains(strings.Join(adopted, ","), "REVIEW") {
		t.Fatalf("a dismissed member must not be adopted: %v", adopted)
	}

	after, loop, err := s.LoopMemberByToken(ctx, tokens["INVESTIGATION"])
	if err != nil || after == nil {
		t.Fatalf("the original token must still work after adoption: %v", err)
	}
	if after.ID != before.ID {
		t.Fatalf("adoption changed the member id: %s -> %s", before.ID, after.ID)
	}
	if after.LoopID != second.ID || loop.Task != "round two" {
		t.Fatalf("adopted member is in loop %q, want %q", after.LoopID, second.ID)
	}
	if after.Status != LoopMemberActive {
		t.Fatalf("an adopted member goes back to work, status = %q", after.Status)
	}

	roster, err := s.ListLoopMembers(ctx, second.ID)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	roles := map[string]int{}
	for _, m := range roster {
		roles[m.Role]++
	}
	for role, n := range roles {
		if n != 1 {
			t.Fatalf("role %s appears %d times in the successor roster", role, n)
		}
	}
	// The role left behind is staffed fresh, and that token is the one to
	// hand out.
	replacement, _, err := s.LoopMemberByToken(ctx, freshTokens["REVIEW"])
	if err != nil || replacement == nil || replacement.LoopID != second.ID {
		t.Fatalf("the replacement REVIEW must be usable: %v %+v", err, replacement)
	}
	if m, _, _ := s.LoopMemberByToken(ctx, freshTokens["INVESTIGATION"]); m != nil {
		t.Fatal("the superseded placeholder token must not survive adoption")
	}
}

// Notes are the role's memory and they outlive the session that wrote them.
func TestNotesAreRoleMemoryAndCarryForward(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	first := &Loop{Task: "notes"}
	if _, err := s.CreateLoop(ctx, first, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.AppendLoopNote(ctx, first.ID, "INVESTIGATION", "inv_1", "round one conclusion"); err != nil {
		t.Fatalf("note: %v", err)
	}
	if _, err := s.AppendLoopNote(ctx, first.ID, "REVIEW", "rev_1", "reviewer conclusion"); err != nil {
		t.Fatalf("note: %v", err)
	}

	mine, err := s.LoopNotes(ctx, first.ID, "INVESTIGATION", 0)
	if err != nil || len(mine) != 1 || mine[0].Body != "round one conclusion" {
		t.Fatalf("notes are per role: %v %+v", err, mine)
	}
	all, err := s.LoopNotes(ctx, first.ID, "", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("loop notes: %v (%d)", err, len(all))
	}
	if n, err := s.CountLoopNotes(ctx, first.ID, "INVESTIGATION"); err != nil || n != 1 {
		t.Fatalf("count: %d %v", n, err)
	}

	second := &Loop{Task: "round two", ParentLoopID: first.ID}
	if _, err := s.CreateLoop(ctx, second, nil); err != nil {
		t.Fatalf("create successor: %v", err)
	}
	carried, err := s.CopyLoopNotes(ctx, first.ID, second.ID)
	if err != nil || carried != 2 {
		t.Fatalf("carry over: %d %v", carried, err)
	}
	inherited, err := s.LoopNotes(ctx, second.ID, "INVESTIGATION", 0)
	if err != nil || len(inherited) != 1 {
		t.Fatalf("inherited notes: %v (%d)", err, len(inherited))
	}
	if !strings.Contains(inherited[0].Body, "round one conclusion") ||
		!strings.Contains(inherited[0].Body, first.ID) {
		t.Fatalf("a carried note must name where it came from: %q", inherited[0].Body)
	}
}

// The pipeline position lives in the row, so a replacement orchestrator
// resumes from it rather than from a context it no longer has.
func TestLoopStateSurvivesTheOrchestrator(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	l := &Loop{Task: "state"}
	tokens, err := s.CreateLoop(ctx, l, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if l.Round != 0 || l.State != "" {
		t.Fatalf("a new loop starts at round 0 with no state: %+v", l)
	}

	text := "investigation delivered, review pending"
	written, err := s.SetLoopState(ctx, l.ID, &text, nil)
	if err != nil || written.State != text {
		t.Fatalf("write state: %v %+v", err, written)
	}

	// The round is the engine's to count. A hand-written one is refused,
	// because a second writer turns "until review reports clean" back into
	// a hope.
	byHand := 3
	if _, err := s.SetLoopState(ctx, l.ID, &text, &byHand); !errors.Is(err, ErrRoundIsEngineOwned) {
		t.Fatalf("writing the round by hand must be refused: %v", err)
	}
	unchanged, _ := s.GetLoop(ctx, l.ID)
	if unchanged.Round != 0 {
		t.Fatalf("a refused round write must not land, got %d", unchanged.Round)
	}

	member, _, err := s.LoopMemberByToken(ctx, tokens[RoleOrchestrator])
	if err != nil || member == nil {
		t.Fatalf("lookup: %v", err)
	}
	replacement, err := s.RotateLoopMemberToken(ctx, member.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	_, resumed, err := s.LoopMemberByToken(ctx, replacement)
	if err != nil || resumed == nil {
		t.Fatalf("replacement lookup: %v", err)
	}
	if resumed.State != text {
		t.Fatalf("a replacement orchestrator must read the row: %+v", resumed)
	}
}

// Lineage bounds what a loop can see: its own history and the loops it came
// from, nothing else.
func TestLineageReachesAncestorsOnly(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	first := &Loop{Task: "one"}
	if _, err := s.CreateLoop(ctx, first, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	second := &Loop{Task: "two", ParentLoopID: first.ID}
	if _, err := s.CreateLoop(ctx, second, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	third := &Loop{Task: "three", ParentLoopID: second.ID}
	if _, err := s.CreateLoop(ctx, third, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	unrelated := &Loop{Task: "elsewhere"}
	if _, err := s.CreateLoop(ctx, unrelated, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	chain, err := s.LoopLineage(ctx, third.ID)
	if err != nil {
		t.Fatalf("lineage: %v", err)
	}
	want := []string{third.ID, second.ID, first.ID}
	if strings.Join(chain, ",") != strings.Join(want, ",") {
		t.Fatalf("lineage = %v, want %v", chain, want)
	}
	for _, id := range chain {
		if id == unrelated.ID {
			t.Fatal("an unrelated loop must never be in the lineage")
		}
	}

	// A cycle must terminate rather than spin.
	cyclic := &Loop{Task: "cycle"}
	if _, err := s.CreateLoop(ctx, cyclic, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loops SET parent_loop_id = ? WHERE id = ?`, cyclic.ID, cyclic.ID); err != nil {
		t.Fatalf("seed cycle: %v", err)
	}
	if chain, err := s.LoopLineage(ctx, cyclic.ID); err != nil || len(chain) != 1 {
		t.Fatalf("a self-parented loop must stop at itself: %v %v", chain, err)
	}
}

// Staffing up mid-loop and purging an ended one.
func TestAddMemberAndPurge(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	l := &Loop{Task: "staffing"}
	if _, err := s.CreateLoop(ctx, l, []string{RoleOrchestrator, RoleEngineer}); err != nil {
		t.Fatalf("create: %v", err)
	}
	member, token, err := s.AddLoopMember(ctx, l.ID, MemberSpec{Role: "research"})
	if err != nil || member == nil {
		t.Fatalf("add member: %v", err)
	}
	if member.Role != "RESEARCH" {
		t.Fatalf("roles are upper case addresses, got %q", member.Role)
	}
	if got, _, _ := s.LoopMemberByToken(ctx, token); got == nil || got.ID != member.ID {
		t.Fatal("a member added mid-loop must be usable at once")
	}
	if _, _, err := s.AddLoopMember(ctx, l.ID, MemberSpec{Role: "RESEARCH"}); err == nil {
		t.Fatal("one row per role per loop")
	}
	if resolved, _ := s.ResolveLoopRecipient(ctx, l.ID, "research"); resolved == nil || resolved.ID != member.ID {
		t.Fatal("a recipient resolves by role name")
	}
	if resolved, _ := s.ResolveLoopRecipient(ctx, l.ID, member.ID); resolved == nil {
		t.Fatal("a recipient resolves by member id")
	}
	if resolved, _ := s.ResolveLoopRecipient(ctx, l.ID, "NOBODY"); resolved != nil {
		t.Fatal("an unknown recipient must not resolve")
	}

	if _, err := s.AppendLoopNote(ctx, l.ID, "RESEARCH", member.ID, "kept"); err != nil {
		t.Fatalf("note: %v", err)
	}
	if _, err := s.EndLoop(ctx, l.ID, LoopKilled, "changed my mind", true); err != nil {
		t.Fatalf("end: %v", err)
	}
	if err := s.PurgeLoop(ctx, l.ID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if gone, err := s.GetLoop(ctx, l.ID); err != nil || gone != nil {
		t.Fatalf("purge must remove the loop: %v %v", gone, err)
	}
	if notes, err := s.LoopNotes(ctx, l.ID, "", 0); err != nil || len(notes) != 0 {
		t.Fatalf("purge must remove the notes: %v %d", err, len(notes))
	}
	if got, _, _ := s.LoopMemberByToken(ctx, token); got != nil {
		t.Fatal("purge must remove the members")
	}
}

// Loops listed newest-first and filtered by status.
func TestListLoops(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	live := &Loop{Task: "live"}
	if _, err := s.CreateLoop(ctx, live, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := &Loop{Task: "done"}
	if _, err := s.CreateLoop(ctx, done, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.EndLoop(ctx, done.ID, LoopComplete, "", false); err != nil {
		t.Fatalf("end: %v", err)
	}

	active, err := s.ListLoops(ctx, LoopActive, 0)
	if err != nil || len(active) != 1 || active[0].ID != live.ID {
		t.Fatalf("active filter: %v %+v", err, active)
	}
	all, err := s.ListLoops(ctx, "", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %v (%d)", err, len(all))
	}
	ended, err := s.GetLoop(ctx, done.ID)
	if err != nil || !ended.Ended() || ended.CompletedAt == 0 {
		t.Fatalf("an ended loop records when it ended: %+v %v", ended, err)
	}
}

// The engineer is an address, not a process: a UI listing the AGENTS has no
// reason to show him and no reason to ask which engine he runs on. But the
// surface resolves his member row to deliver anything he types, so a loop
// created without one would silently refuse to let him speak to it.
func TestEveryLoopGetsAnEngineerEvenWhenTheCallerOmitsHim(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "engineer.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	l := &Loop{Task: "no engineer sent", CWD: "/tmp"}
	tokens, err := s.CreateCrew(ctx, l, RoleSpecs("ORCHESTRATOR", "REVIEW"))
	if err != nil {
		t.Fatalf("crew: %v", err)
	}
	roster, err := s.ListLoopMembers(ctx, l.ID)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	var engineer *LoopMember
	for _, m := range roster {
		if m.Role == RoleEngineer {
			engineer = m
		}
	}
	if engineer == nil {
		t.Fatal("the loop has no ENGINEER, so nothing he types could be delivered")
	}
	// He is still not an agent: no token, no tool, no model.
	if tokens[RoleEngineer] != "" {
		t.Error("the engineer was issued a polling token; he is a participant, not an agent")
	}
	if engineer.Tool != "" || engineer.Model != "" {
		t.Errorf("the engineer runs on nothing, got tool=%q model=%q", engineer.Tool, engineer.Model)
	}

	// And sending him explicitly does not produce two of him.
	l2 := &Loop{Task: "engineer sent", CWD: "/tmp"}
	if _, err := s.CreateCrew(ctx, l2, RoleSpecs("ORCHESTRATOR", "ENGINEER")); err != nil {
		t.Fatalf("crew: %v", err)
	}
	roster2, _ := s.ListLoopMembers(ctx, l2.ID)
	n := 0
	for _, m := range roster2 {
		if m.Role == RoleEngineer {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want exactly one engineer, got %d", n)
	}
}
