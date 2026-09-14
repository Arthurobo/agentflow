// overrides_test.go — the engineer's edits. The guarantees here are about what
// CANNOT happen: a stored override that breaks the graph, a copied play, a
// weakened binding rule, and an edit that vanishes without explanation.
package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func overrideStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ovr.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Validate on WRITE is the whole reason the daemon can never boot broken: a
// brief that would break the graph is refused at the moment it is typed, with
// the validator's own message.
func TestAnOverrideThatBreaksThePlayIsRefusedOnWrite(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	// A step with no brief is a gap, and the engineer gets the VALIDATOR's own
	// description of what he broke rather than a sentence of ours.
	_, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindPlay, Play: "deep", Step: "review_work", Field: FieldBrief, Value: "   ",
	})
	if err == nil {
		t.Fatal("an emptied brief must be refused")
	}
	if !strings.Contains(err.Error(), "no brief") {
		t.Errorf("the refusal must carry the validator's message, got %v", err)
	}
	// A field Validate says nothing about still gets the plain refusal.
	if _, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindPlay, Play: "deep", Field: FieldPurpose, Value: "  ",
	}); err == nil || !strings.Contains(err.Error(), "delete the override instead") {
		t.Errorf("an emptied purpose must say what to do instead, got %v", err)
	}
	got, err := s.ListPromptOverrides(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("nothing may be stored on a refusal, got %d: %v", len(got), err)
	}
}

func TestAnOverrideOnNothingIsRefused(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	for _, bad := range []PromptOverride{
		{Kind: OverrideKindPlay, Play: "deep", Step: "no-such-step", Field: FieldBrief, Value: "x"},
		{Kind: OverrideKindPlay, Play: "no-such-play", Step: "brief", Field: FieldBrief, Value: "x"},
		{Kind: OverrideKindRule, Rule: "no-such-rule", Field: FieldText, Value: "x"},
		{Kind: OverrideKindPlay, Play: "deep", Step: "brief", Field: "colour", Value: "x"},
	} {
		if _, err := s.PutPromptOverride(ctx, bad); err == nil {
			t.Errorf("%+v must be refused: there is nothing there to override", bad)
		}
	}
}

// A leaf edit touches one field. Everything else in that play, and every other
// play, still gets whatever we ship next.
func TestAnOverrideIsALeafAndNeverACopiedPlay(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	if _, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindPlay, Play: "deep", Step: "review_work", Field: FieldBrief,
		Value: "Read the diff. Say clean or changes.",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	cat, err := s.PromptCatalogue(ctx)
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	deep, _ := cat.LookupPlay("deep")
	shipped, _ := LookupPlay("deep")
	if deep.Step("review_work").Brief != "Read the diff. Say clean or changes." {
		t.Error("the edit did not apply")
	}
	// every OTHER step of the same play is untouched
	for _, s := range shipped.Steps {
		if s.ID == "review_work" {
			continue
		}
		if deep.Step(s.ID).Brief != s.Brief {
			t.Errorf("step %s changed; an override is a leaf, not a fork of the play", s.ID)
		}
	}
	// and every other play too
	if len(cat.Plays) != len(shipped2(t)) {
		t.Error("a play went missing")
	}
}

func shipped2(t *testing.T) []string { t.Helper(); return PlayNames() }

// Stale is not revoked: it is his text and it keeps applying, and the UI can
// offer keep-mine or take-the-new-one.
func TestADefaultsUpgradeMarksAnOverrideStaleWithoutRevokingIt(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	if _, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindRule, Rule: "house-style", Field: FieldText, Value: "Tabs, and no jokes.",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// simulate the shipped text moving underneath it
	stored, _ := s.ListPromptOverrides(ctx)
	stored[0].BaseDigest = Digest("something the previous version said")
	cat := buildCatalogue(stored)

	var o PromptOverride
	for _, x := range cat.Overrides {
		if x.Rule == "house-style" {
			o = x
		}
	}
	if o.Status != OverrideStale {
		t.Fatalf("want stale, got %q", o.Status)
	}
	if r, _ := cat.LookupRule("house-style"); r.Text != "Tabs, and no jokes." {
		t.Errorf("a stale override must still APPLY; got %q", r.Text)
	}
	if o.Base == "" {
		t.Error("a stale override must carry the new default so the UI can offer take-the-new-one")
	}
}

// Orphaned is not applied and not deleted, so his writing is retrievable.
func TestARenamedAnchorOrphansAnOverrideAndKeepsIt(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	if _, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindPlay, Play: "deep", Step: "rework", Field: FieldBrief, Value: "Send it back.",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	stored, _ := s.ListPromptOverrides(ctx)
	stored[0].Step = "a_step_that_was_renamed"
	cat := buildCatalogue(stored)

	if len(cat.Overrides) != 1 || cat.Overrides[0].Status != OverrideOrphaned {
		t.Fatalf("want one orphaned override, got %+v", cat.Overrides)
	}
	if cat.Overrides[0].Value != "Send it back." {
		t.Error("an orphaned override must keep its text; it is the engineer's writing")
	}
	deep, _ := cat.LookupPlay("deep")
	shipped, _ := LookupPlay("deep")
	if deep.Step("rework").Brief != shipped.Step("rework").Brief {
		t.Error("an orphaned override must not be applied anywhere")
	}
}

// BINDING. He can tighten a rule from his phone and cannot weaken it.
func TestABindingRuleCanOnlyBeAppendedTo(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	shipped, _ := LookupRule("engineer-drives-git")
	if _, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindRule, Rule: "engineer-drives-git", Field: FieldText,
		Value: "Also never touch the release branch.",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	cat, _ := s.PromptCatalogue(ctx)
	got, _ := cat.LookupRule("engineer-drives-git")
	if !strings.Contains(got.Text, shipped.Text) {
		t.Fatal("the shipped text must survive an override of a binding rule")
	}
	if !strings.Contains(got.Text, "release branch") {
		t.Error("the engineer's addition must be there; he can tighten it")
	}
}

func TestABindingRuleCannotBeReplacedFromAPhone(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	shipped, _ := LookupRule("no-deploys")
	if _, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindRule, Rule: "no-deploys", Field: FieldText,
		Value: "Deploys are fine, go ahead.",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	cat, _ := s.PromptCatalogue(ctx)
	got, _ := cat.LookupRule("no-deploys")
	if !strings.Contains(got.Text, shipped.Text) {
		t.Fatal("a binding rule cannot be weakened; its shipped text is a floor")
	}
	// An advisory rule IS replaceable, and that is the difference.
	if _, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindRule, Rule: "no-scope-expansion", Field: FieldText, Value: "Use judgement.",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	cat, _ = s.PromptCatalogue(ctx)
	adv, _ := cat.LookupRule("no-scope-expansion")
	if adv.Text != "Use judgement." {
		t.Errorf("an advisory rule must be replaceable, got %q", adv.Text)
	}
}

// The second tier: a defaults upgrade that invalidates an existing override
// costs ONE play, never the catalogue, and never silently falls back to the
// default for that play.
func TestOneBrokenMergeDropsOnePlayAndRecordsWhy(t *testing.T) {
	// Written directly rather than through Put, because Put is what stops
	// this ever being stored: this is the shape a defaults upgrade leaves.
	broken := []PromptOverride{{
		Kind: OverrideKindPlay, Play: "deep", Step: "review_work", Field: FieldBrief, Value: "",
	}}
	cat := buildCatalogue(broken)
	if _, ok := cat.LookupPlay("deep"); ok {
		t.Error("a play whose merged form does not validate must be dropped")
	}
	msg, dropped := cat.Dropped["deep"]
	if !dropped || !strings.Contains(msg, "no brief") {
		t.Errorf("the reason must be recorded, got %q", msg)
	}
	// the other six still work
	if len(cat.Plays) != len(PlayNames())-1 {
		t.Errorf("one bad merge cost %d plays; it must cost exactly one", len(PlayNames())-len(cat.Plays))
	}
	for _, name := range []string{"recon", "audit", "patch", "build", "sweep", "second-opinion"} {
		if _, ok := cat.LookupPlay(name); !ok {
			t.Errorf("%s was lost to an unrelated play's bad merge", name)
		}
	}
}

// Two overrides can never target one leaf, which is what makes the merge one
// deterministic pass with no precedence rule to get wrong.
func TestOneLeafHoldsOneOverride(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	leaf := PromptOverride{Kind: OverrideKindPlay, Play: "recon", Step: "brief", Field: FieldBrief}
	leaf.Value = "first"
	if _, err := s.PutPromptOverride(ctx, leaf); err != nil {
		t.Fatalf("first: %v", err)
	}
	leaf.Value = "second"
	if _, err := s.PutPromptOverride(ctx, leaf); err != nil {
		t.Fatalf("second: %v", err)
	}
	got, _ := s.ListPromptOverrides(ctx)
	if len(got) != 1 {
		t.Fatalf("one leaf, one override; got %d", len(got))
	}
	if got[0].Value != "second" {
		t.Errorf("the later edit wins, got %q", got[0].Value)
	}
}

func TestDeletingAnOverrideRestoresTheShippedText(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	saved, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindPlay, Play: "recon", Step: "brief", Field: FieldBrief, Value: "mine",
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if ok, err := s.DeletePromptOverride(ctx, saved.ID); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	cat, _ := s.PromptCatalogue(ctx)
	got, _ := cat.LookupPlay("recon")
	shipped, _ := LookupPlay("recon")
	if got.Step("brief").Brief != shipped.Step("brief").Brief {
		t.Error("deleting an override must restore the shipped text")
	}
}

// There is no device column and no user column, and that absence is the
// decision: a brief queued by the engine at 3am has no creating device.
func TestOverridesAreMachineScoped(t *testing.T) {
	s := overrideStore(t)
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('prompt_overrides')`)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		if col == "device_id" || col == "user_id" || col == "device" {
			t.Errorf("prompt_overrides has a %q column; overrides are per machine on purpose", col)
		}
	}
}

// The board resolves the current step's brief from the LIVE definition, so
// after an edit it showed the new brief next to a message row containing the
// old one with nothing saying they differ. The row is what the agent actually
// read, and that is what the board has to show.
func TestTheBoardShowsTheBriefThatWasActuallySent(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	loop := &Loop{Task: "provenance", CWD: "/tmp"}
	if _, err := s.CreateCrew(ctx, loop, RoleSpecs("ORCHESTRATOR", "INVESTIGATION")); err != nil {
		t.Fatalf("crew: %v", err)
	}
	if _, err := s.StartPlay(ctx, loop.ID, "recon"); err != nil {
		t.Fatalf("play: %v", err)
	}
	fresh, err := s.GetLoop(ctx, loop.ID)
	if err != nil || fresh == nil {
		t.Fatalf("loop: %v", err)
	}

	// Make the QUEUED text differ from the definition, which is exactly what
	// an edit after the step opened leaves behind. Reading the definition and
	// reading the row now give different answers, so only one of them can pass.
	const asSent = "THE TEXT THE MEMBER ACTUALLY RECEIVED"
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET body = ? WHERE loop_id = ? AND step_id = ? AND sender_role = ?`,
		asSent, loop.ID, fresh.StepID, RoleEngine); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, ok := s.DeliveredStepBrief(ctx, fresh)
	if !ok {
		t.Fatal("no brief resolved at all")
	}
	if got != asSent {
		shipped, _ := LookupPlay("recon")
		if got == shipped.Step(fresh.StepID).Brief {
			t.Fatal("the board is reading the play definition, not the message the member was sent")
		}
		t.Fatalf("want the queued text, got %q", got)
	}

	// A brief queued for a DIFFERENT step is not this step's brief.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE loop_messages SET step_id = 'some_other_step' WHERE loop_id = ?`, loop.ID); err != nil {
		t.Fatalf("move: %v", err)
	}
	shipped, _ := LookupPlay("recon")
	after, ok := s.DeliveredStepBrief(ctx, fresh)
	if !ok || after != shipped.Step(fresh.StepID).Brief {
		t.Errorf("with no row for THIS step it must fall back to the definition, got %q", after)
	}

	// And a loop with no rows at all still resolves.
	none := &Loop{ID: "loop_absent", Play: "recon", StepID: "brief", PlayStatus: PlayRunning}
	if got, ok := s.DeliveredStepBrief(ctx, none); !ok || got != shipped.Step("brief").Brief {
		t.Errorf("with no rows it must fall back to the definition, got %q", got)
	}
}

// applyValue is the whole of the binding contract, and Put can no longer reach
// its blank branch, so it is held to its contract directly.
func TestApplyValueIsAppendOnlyForBindingRules(t *testing.T) {
	const base = "The engineer drives ALL git."
	if got := applyValue(base, "Also never touch main.", true); got != base+"\nAlso never touch main." {
		t.Errorf("binding must append, got %q", got)
	}
	if got := applyValue(base, "Push freely.", true); !strings.Contains(got, base) {
		t.Errorf("binding must keep its shipped text, got %q", got)
	}
	if got := applyValue(base, "   ", true); got != base {
		t.Errorf("a blank override must leave a binding rule exactly as shipped, got %q", got)
	}
	if got := applyValue(base, "Use judgement.", false); got != "Use judgement." {
		t.Errorf("an advisory rule is replaced, got %q", got)
	}
}

// The solo block's prose is a leaf like any other. WHICH rules it references
// is structure, and structure is not editable from a phone.
func TestTheSoloIntroIsEditableAndReachesASoloPrompt(t *testing.T) {
	s := overrideStore(t)
	ctx := context.Background()
	if _, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindSolo, Field: FieldIntro,
		Value: "You are working on the payments service. Ask before touching migrations.",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	cat, err := s.PromptCatalogue(ctx)
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	got, err := cat.SoloPrompt(PromptModeStanding)
	if err != nil {
		t.Fatalf("solo: %v", err)
	}
	if !strings.Contains(got, "Ask before touching migrations") {
		t.Error("the edited intro did not reach the prompt")
	}
	// The rules it names are still the shipped set: prose is editable,
	// structure is not.
	if !strings.Contains(got, "[reporting-contract-solo]") {
		t.Error("editing the intro must not change which rules the block references")
	}
	// Nothing else may be overridden on the solo block.
	if _, err := s.PutPromptOverride(ctx, PromptOverride{
		Kind: OverrideKindSolo, Field: "rules", Value: "engineer-drives-git",
	}); err == nil {
		t.Error("which rules the solo block references is structure and must not be editable")
	}
}
