// overrides.go — the engineer's edits, and the merge that applies them.
//
// Two rules govern everything here.
//
// Validate on WRITE, not on load. Play.Validate is pure and descriptive and
// was called from nothing in production; the override API applies a proposed
// override onto the defaults, validates the MERGED play, and refuses to store
// on failure with the validator's own message. The daemon can then never boot
// into a broken state, because a broken state was never stored.
//
// Load time is the second tier, for the one case write time cannot cover: a
// defaults upgrade invalidating an override that was valid when written. Each
// play is merged and validated INDEPENDENTLY, a failing one is dropped from
// the catalogue with its error recorded, and the other six keep working. There
// is deliberately no whole-catalogue fallback: that turns one typo into the
// silent loss of every customisation.

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrOverrideInvalid refuses an override that does not name a real leaf or
// whose merged result does not validate.
var ErrOverrideInvalid = errors.New("store: invalid prompt override")

// Override kinds and the fields each one may target.
const (
	OverrideKindPlay = "play"
	OverrideKindRule = "rule"
	// OverrideKindSolo is the block a session with no loop reads. Its prose
	// is one leaf; WHICH rules it references is structure and is not editable
	// from a phone.
	OverrideKindSolo = "solo"

	FieldBrief   = "brief"
	FieldNote    = "note"
	FieldPurpose = "purpose"
	FieldTitle   = "title"
	FieldText    = "text"
	FieldIntro   = "intro"
)

// Override statuses, computed on read and never stored: they are a function of
// the override and the CURRENT defaults, so storing one would go stale.
const (
	// OverrideApplied: the default this was written against is unchanged.
	OverrideApplied = "applied"
	// OverrideStale: the default changed underneath it. Still applied,
	// because it is his text and we do not revoke it, but the UI can offer
	// keep-mine or take-the-new-one.
	OverrideStale = "stale"
	// OverrideOrphaned: the anchor is gone, a step renamed or removed. NOT
	// applied and NOT deleted, so his writing is retrievable.
	OverrideOrphaned = "orphaned"
)

// PromptOverride is one leaf edit.
type PromptOverride struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Play       string `json:"play,omitempty"`
	Step       string `json:"step,omitempty"`
	Rule       string `json:"rule,omitempty"`
	Field      string `json:"field"`
	Value      string `json:"value"`
	BaseDigest string `json:"baseDigest,omitempty"`
	CreatedAt  int64  `json:"createdAt,omitempty"`
	UpdatedAt  int64  `json:"updatedAt,omitempty"`
	// Status is computed against the current defaults on read.
	Status string `json:"status,omitempty"`
	// Base is the current default value of the anchor, empty when orphaned.
	Base string `json:"base,omitempty"`
}

// Digest is the anchor fingerprint: sha256 of the default value an override
// was written against.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// defaultLeaf returns the shipped value of the leaf an override targets.
func defaultLeaf(o PromptOverride) (string, bool) {
	switch o.Kind {
	case OverrideKindSolo:
		if o.Field != FieldIntro {
			return "", false
		}
		return soloBlock.Intro, true
	case OverrideKindRule:
		r, ok := LookupRule(o.Rule)
		if !ok || o.Field != FieldText {
			return "", false
		}
		return r.Text, true
	case OverrideKindPlay:
		p, ok := lookupDefaultPlay(o.Play)
		if !ok {
			return "", false
		}
		if o.Step == "" {
			switch o.Field {
			case FieldPurpose:
				return p.Purpose, true
			case FieldTitle:
				return p.Title, true
			}
			return "", false
		}
		s := p.Step(o.Step)
		if s == nil {
			return "", false
		}
		switch o.Field {
		case FieldBrief:
			return s.Brief, true
		case FieldNote:
			return s.Note, true
		}
	}
	return "", false
}

func lookupDefaultPlay(name string) (*Play, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for i := range plays {
		if plays[i].Name == name {
			cp := plays[i]
			return &cp, true
		}
	}
	return nil, false
}

// applyValue is the whole of the binding contract. A binding rule's shipped
// text is fixed and an override may only ADD to it, so the engineer can
// tighten a rule or name a project script from his phone and cannot weaken it
// from there. Everything else is a replacement.
func applyValue(base, value string, binding bool) string {
	if !binding {
		return value
	}
	if strings.TrimSpace(value) == "" {
		return base
	}
	return base + "\n" + strings.TrimSpace(value)
}

// PromptCatalogue is the merged view: what this machine actually uses.
type PromptCatalogue struct {
	Plays []Play
	// Rules by group name, in dictionary order.
	Rules map[string][]Rule
	Roles map[string]string
	Solo  SoloBlock
	// Overrides carries every stored override with its computed status.
	Overrides []PromptOverride
	// Dropped names the plays whose merged form failed to validate, with the
	// validator's message. They are absent from Plays and the others still
	// work.
	Dropped map[string]string
}

// LookupPlay finds one play in the merged catalogue.
func (c *PromptCatalogue) LookupPlay(name string) (*Play, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for i := range c.Plays {
		if c.Plays[i].Name == name {
			return &c.Plays[i], true
		}
	}
	return nil, false
}

// RulesFor returns the merged rules that apply to a role, shared first.
func (c *PromptCatalogue) RulesFor(role string) []Rule {
	out := append([]Rule{}, c.Rules[RulesShared]...)
	switch {
	case isOrchestratorRole(role):
		return append(out, c.Rules[RulesOrchestrator]...)
	case isEngineerRole(role):
		return out
	default:
		return append(out, c.Rules[RulesWorker]...)
	}
}

// LookupRule finds one rule anywhere in the merged dictionary.
func (c *PromptCatalogue) LookupRule(name string) (Rule, bool) {
	for _, group := range []string{RulesShared, RulesWorker, RulesOrchestrator, RulesSolo} {
		for _, r := range c.Rules[group] {
			if r.Name == name {
				return r, true
			}
		}
	}
	return Rule{}, false
}

// DefaultCatalogue is the shipped catalogue with no overrides applied. It is
// what the pure render path uses and what a test compares against.
func DefaultCatalogue() *PromptCatalogue {
	return buildCatalogue(nil)
}

// buildCatalogue merges a set of overrides onto the defaults. Each play is
// validated independently so one bad merge costs one play.
func buildCatalogue(overrides []PromptOverride) *PromptCatalogue {
	c := &PromptCatalogue{
		Rules:   map[string][]Rule{},
		Roles:   map[string]string{},
		Solo:    soloBlock,
		Dropped: map[string]string{},
	}
	for k, v := range roleJobs {
		c.Roles[k] = v
	}
	for _, group := range []struct {
		name string
		src  []Rule
	}{
		{RulesShared, sharedRules}, {RulesWorker, workerRules},
		{RulesOrchestrator, orchestratorRules}, {RulesSolo, soloRules},
	} {
		c.Rules[group.name] = append([]Rule{}, group.src...)
	}

	byLeaf := map[string]PromptOverride{}
	for _, o := range overrides {
		base, ok := defaultLeaf(o)
		o.Base = base
		switch {
		case !ok:
			o.Status = OverrideOrphaned
		case o.BaseDigest != "" && o.BaseDigest != Digest(base):
			o.Status = OverrideStale
		default:
			o.Status = OverrideApplied
		}
		c.Overrides = append(c.Overrides, o)
		if o.Status != OverrideOrphaned {
			byLeaf[leafKey(o)] = o
		}
	}

	if o, ok := byLeaf[leafKey(PromptOverride{Kind: OverrideKindSolo, Field: FieldIntro})]; ok {
		c.Solo.Intro = o.Value
	}

	// rules
	for group, rules := range c.Rules {
		for i := range rules {
			o, ok := byLeaf[leafKey(PromptOverride{
				Kind: OverrideKindRule, Rule: rules[i].Name, Field: FieldText,
			})]
			if !ok {
				continue
			}
			rules[i].Text = applyValue(rules[i].Text, o.Value, rules[i].Binding)
		}
		c.Rules[group] = rules
	}

	// plays, each independently
	for i := range plays {
		p := clonePlay(&plays[i])
		applyPlayOverrides(p, byLeaf)
		if err := p.Validate(); err != nil {
			// Dropped, never silently replaced with the default: an engineer
			// whose edit vanished with no explanation will simply make it
			// again.
			c.Dropped[p.Name] = err.Error()
			continue
		}
		c.Plays = append(c.Plays, *p)
	}
	return c
}

func applyPlayOverrides(p *Play, byLeaf map[string]PromptOverride) {
	if o, ok := byLeaf[leafKey(PromptOverride{Kind: OverrideKindPlay, Play: p.Name, Field: FieldPurpose})]; ok {
		p.Purpose = o.Value
	}
	if o, ok := byLeaf[leafKey(PromptOverride{Kind: OverrideKindPlay, Play: p.Name, Field: FieldTitle})]; ok {
		p.Title = o.Value
	}
	for j := range p.Steps {
		s := &p.Steps[j]
		if o, ok := byLeaf[leafKey(PromptOverride{
			Kind: OverrideKindPlay, Play: p.Name, Step: s.ID, Field: FieldBrief,
		})]; ok {
			s.Brief = o.Value
		}
		if o, ok := byLeaf[leafKey(PromptOverride{
			Kind: OverrideKindPlay, Play: p.Name, Step: s.ID, Field: FieldNote,
		})]; ok {
			s.Note = o.Value
		}
	}
}

func clonePlay(src *Play) *Play {
	out := *src
	out.Steps = make([]PlayStep, len(src.Steps))
	copy(out.Steps, src.Steps)
	for i := range out.Steps {
		out.Steps[i].Next = append([]PlayEdge(nil), src.Steps[i].Next...)
	}
	return &out
}

func leafKey(o PromptOverride) string {
	return strings.Join([]string{o.Kind, strings.ToLower(o.Play), o.Step, o.Rule, o.Field}, "\x00")
}

const promptOverrideColumns = `id, kind, play, step, rule, field, value, base_digest, created_at, updated_at`

// ListPromptOverrides returns every stored override with its status computed
// against the current defaults.
func (s *Store) ListPromptOverrides(ctx context.Context) ([]PromptOverride, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+promptOverrideColumns+` FROM prompt_overrides ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []PromptOverride{}
	for rows.Next() {
		var o PromptOverride
		if err := rows.Scan(&o.ID, &o.Kind, &o.Play, &o.Step, &o.Rule, &o.Field,
			&o.Value, &o.BaseDigest, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PromptCatalogue returns the merged catalogue this machine uses.
func (s *Store) PromptCatalogue(ctx context.Context) (*PromptCatalogue, error) {
	overrides, err := s.ListPromptOverrides(ctx)
	if err != nil {
		return nil, err
	}
	return buildCatalogue(overrides), nil
}

// PutPromptOverride stores one leaf edit, or refuses it.
//
// The refusal is the point: the proposed override is applied onto the defaults
// and the MERGED play is validated before anything is written, so a brief that
// would break the graph is rejected at the moment it is typed, with the
// validator's own message, rather than at the next boot.
func (s *Store) PutPromptOverride(ctx context.Context, o PromptOverride) (*PromptOverride, error) {
	o.Kind = strings.ToLower(strings.TrimSpace(o.Kind))
	o.Play = strings.ToLower(strings.TrimSpace(o.Play))
	o.Step = strings.TrimSpace(o.Step)
	o.Rule = strings.TrimSpace(o.Rule)
	o.Field = strings.ToLower(strings.TrimSpace(o.Field))
	// Trimmed before it is validated OR stored: leading and trailing space is
	// not meaningful prose, and an untrimmed value made a whitespace-only
	// brief validate, because Validate asks whether a brief is empty and "   "
	// is not.
	o.Value = strings.TrimSpace(o.Value)

	base, ok := defaultLeaf(o)
	if !ok {
		return nil, fmt.Errorf("%w: nothing to override at %s", ErrOverrideInvalid, describeLeaf(o))
	}
	// The digest is taken from the CURRENT default at write time: an override
	// is by definition written against what the engineer was shown.
	o.BaseDigest = Digest(base)

	// The merged validation runs FIRST, and the order matters. A blank-value
	// guard ahead of it would catch the only override that can break a graph
	// today, an emptied brief, and the engineer would get our sentence instead
	// of the validator's own description of what he broke.
	if o.Kind == OverrideKindPlay {
		existing, err := s.ListPromptOverrides(ctx)
		if err != nil {
			return nil, err
		}
		merged := buildCatalogue(replaceLeaf(existing, o))
		if msg, dropped := merged.Dropped[o.Play]; dropped {
			return nil, fmt.Errorf("%w: %s", ErrOverrideInvalid, msg)
		}
	}
	if o.Value == "" {
		return nil, fmt.Errorf("%w: an empty override would delete the shipped text; delete the override instead",
			ErrOverrideInvalid)
	}

	now := time.Now().UnixMilli()
	if o.ID == "" {
		o.ID = newMailID("ovr")
	}
	o.UpdatedAt = now
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO prompt_overrides (`+promptOverrideColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (kind, play, step, rule, field) DO UPDATE SET
				value = excluded.value, base_digest = excluded.base_digest,
				updated_at = excluded.updated_at`,
			o.ID, o.Kind, o.Play, o.Step, o.Rule, o.Field, o.Value, o.BaseDigest, now, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	o.CreatedAt = now
	o.Base = base
	o.Status = OverrideApplied
	return &o, nil
}

// replaceLeaf returns the override set with o taking the place of any override
// on the same leaf, which is what the UNIQUE constraint would do on write.
func replaceLeaf(existing []PromptOverride, o PromptOverride) []PromptOverride {
	out := make([]PromptOverride, 0, len(existing)+1)
	key := leafKey(o)
	for _, e := range existing {
		if leafKey(e) != key {
			out = append(out, e)
		}
	}
	return append(out, o)
}

func describeLeaf(o PromptOverride) string {
	if o.Kind == OverrideKindRule {
		return fmt.Sprintf("rule %q field %q", o.Rule, o.Field)
	}
	if o.Step == "" {
		return fmt.Sprintf("play %q field %q", o.Play, o.Field)
	}
	return fmt.Sprintf("play %q step %q field %q", o.Play, o.Step, o.Field)
}

// DeletePromptOverride removes one edit, restoring the shipped text.
func (s *Store) DeletePromptOverride(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM prompt_overrides WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
