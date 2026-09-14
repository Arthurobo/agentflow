// Package approvals implements the remote-approval engine:
// PreToolUse allowlist-misses are routed to a human (or resolved by standing
// policy), deny-by-default on timeout, with a per-(run, tool) collapse +
// cooldown so a burst of identical asks produces ONE notification. Every
// request/decision is recorded in the approvals table (= the audit trail).
package approvals

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

// MaxInputPreview bounds tool_input stored in the audit trail (blob policy: no
// secrets verbatim; the full input is hashed separately).
const MaxInputPreview = 2000

// defaultCooldown is used when no policy sets cooldown for an ask.
const defaultCooldown = 15 * time.Second

// DefaultWait is the approval wait (no answer by the deadline = deny)
// and must stay below the hook timeout (600s default).
const DefaultWait = 90 * time.Second

// Decision is the hook-facing outcome.
type Decision struct {
	Routed  bool   `json:"routed"` // false = this session does not route approvals
	Allowed bool   `json:"allowed"`
	ID      string `json:"id,omitempty"`
	Reason  string `json:"reason,omitempty"` // policy:<id> | cooldown | timeout | device:<id> | hook
}

// Params is one PreToolUse (hook payload subset).
type Params struct {
	SessionID string
	RunID     string
	ToolUseID string
	ToolName  string
	ToolInput string // pre-rendered (JSON) input text
	Project   string
	CWD       string
	MissionID string
}

// Option configures the Service.
type Option func(*Service)

// WithOnOpen sets the callback fired exactly when a NEW pending approval opens
// (i.e. when a push/notification should fire — never for collapsed/cooled-down
// asks, so bursts stay quiet).
func WithOnOpen(fn func(a *store.Approval)) Option {
	return func(s *Service) { s.onOpen = fn }
}

// WithWait overrides the human-answer deadline.
func WithWait(w time.Duration) Option {
	return func(s *Service) { s.wait = w }
}

// Wait returns the configured human-answer deadline.
func (s *Service) Wait() time.Duration { return s.wait }

// ErrNotPending is returned by Decide when the approval is no longer pending
// (already decided, expired, or unknown). The row is left as it was.
var ErrNotPending = errors.New("approvals: approval is not pending")

// pendingDecision is the broadcast primitive for a blocking approval: the
// resolution closes done once, waking EVERY waiter (collapsed asks included)
// at once; the outcome is read back from the approvals row (single source of
// truth). waiters counts the goroutines holding it, so the entry is dropped
// only when the last of them leaves — one waiter giving up must not take the
// wake-up away from the others.
type pendingDecision struct {
	done    chan struct{}
	closed  bool
	waiters int
}

// Service resolves approval requests: policy evaluation, human routing with
// collapse + cooldown, deny-by-default timeout.
type Service struct {
	st   *store.Store
	log  *slog.Logger
	wait time.Duration

	onOpen func(*store.Approval)

	mu     sync.Mutex
	dec    map[string]*pendingDecision // approval id -> broadcast
	open   map[string]string           // "run|tool" -> open pending approval id (collapse gate)
	recent map[string]time.Time        // "run|tool" -> last decision time (cooldown)
}

// New builds the approval engine over the store.
func New(st *store.Store, log *slog.Logger, opts ...Option) *Service {
	s := &Service{st: st, log: log, wait: DefaultWait, dec: map[string]*pendingDecision{}, open: map[string]string{}, recent: map[string]time.Time{}}
	if s.log == nil {
		s.log = slog.Default()
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Routed reports whether this session routes its PreToolUse calls to approval.
// Only approvals-enabled runs (spawner Options.ApprovalsEnabled) route — a run
// configured with full allowlists never pings anyone.
func (s *Service) Routed(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	m, err := s.st.GetManagedSessionBySessionID(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return m != nil && m.ApprovalsEnabled, nil
}

// Resolve evaluates + answers one PreToolUse. deny-by-default on every path
// that has no explicit human/policy allow.
func (s *Service) Resolve(ctx context.Context, p Params) (Decision, error) {
	routed, err := s.Routed(ctx, p.SessionID)
	if err != nil {
		return Decision{}, err
	}
	if !routed {
		return Decision{Routed: false}, nil // fast no-op; hook exits 0
	}
	// project/cwd defaults come from the managed run when known
	if m, err := s.st.GetManagedSessionBySessionID(ctx, p.SessionID); err == nil && m != nil {
		if p.Project == "" {
			p.Project = m.Project
		}
		if p.CWD == "" {
			p.CWD = m.CWD
		}
		if p.RunID == "" {
			p.RunID = m.ID
		}
	}

	policy, err := s.matchPolicy(ctx, p.Project, p.ToolName, matchText(p.ToolName, p.ToolInput))
	if err != nil {
		return Decision{}, err
	}
	action := "ask"
	var pRule *store.ApprovalPolicy
	if policy != nil {
		action = policy.Action
		pRule = policy
	}

	id := newID()
	now := time.Now().UnixMilli()
	a := &store.Approval{
		ID: id, SessionID: p.SessionID, RunID: p.RunID, MissionID: p.MissionID,
		ToolUseID: p.ToolUseID, ToolName: p.ToolName,
		ToolInput: preview(p.ToolInput), InputHash: hashOf(p.ToolInput),
		Project: p.Project, CWD: p.CWD, Routed: true,
		State:     store.ApprovalPending,
		CreatedAt: now, ExpiresAt: now + s.wait.Milliseconds(),
	}

	switch action {
	case "allow":
		a.State = store.ApprovalApproved
		a.DecisionBy = ruleLabel(pRule)
		_ = s.st.UpsertApproval(ctx, a)
		s.log.Info("approval: policy allow", "tool", p.ToolName, "rule", ruleLabel(pRule))
		return Decision{Routed: true, Allowed: true, ID: id, Reason: ruleLabel(pRule)}, nil

	case "deny":
		a.State = store.ApprovalDenied
		a.DecisionBy = ruleLabel(pRule)
		_ = s.st.UpsertApproval(ctx, a)
		s.log.Info("approval: policy deny", "tool", p.ToolName, "rule", ruleLabel(pRule))
		return Decision{Routed: true, Allowed: false, ID: id, Reason: ruleLabel(pRule)}, nil
	}

	// ---- ask: human routing with collapse + cooldown (anti-spam) ----
	key := p.RunID + "|" + p.ToolName

	// The open/collapse decision must be atomic within the process: without
	// this gate, concurrent asks for the same (run, tool) can all pass the
	// pending check before the first insert commits (N opens for one push).
	s.mu.Lock()
	if parentID, ok := s.open[key]; ok {
		parent := parentID
		pd := s.registerLocked(parent)
		s.mu.Unlock()
		defer s.release(parent, pd)
		a.State = store.ApprovalCollided
		a.Collided = true
		_ = s.st.UpsertApproval(ctx, a)
		o := s.waitOn(ctx, parent, pd, 0)
		return Decision{Routed: true, Allowed: o == outcomeAllowed, ID: parent, Reason: "pending"}, nil
	}
	if prev, _ := s.st.PendingApprovalForRunTool(ctx, p.RunID, p.ToolName); prev != nil {
		s.open[key] = prev.ID
		parent := prev.ID
		pd := s.registerLocked(parent)
		s.mu.Unlock()
		defer s.release(parent, pd)
		a.State = store.ApprovalCollided
		a.Collided = true
		_ = s.st.UpsertApproval(ctx, a)
		s.log.Info("approval: collapsed onto pending", "tool", p.ToolName, "parent", prev.ID)
		o := s.waitOn(ctx, parent, pd, prev.ExpiresAt)
		s.mu.Lock()
		if s.open[key] == parent {
			delete(s.open, key)
		}
		s.mu.Unlock()
		return Decision{Routed: true, Allowed: o == outcomeAllowed, ID: parent, Reason: "pending"}, nil
	}

	cooldown := defaultCooldown
	if pRule != nil && pRule.CooldownSec > 0 {
		cooldown = time.Duration(pRule.CooldownSec) * time.Second
	}
	if last, inCool := s.recent[key]; inCool && time.Since(last) < cooldown {
		s.mu.Unlock()
		a.State = store.ApprovalDenied
		a.DecisionBy = "cooldown"
		_ = s.st.UpsertApproval(ctx, a)
		s.log.Info("approval: cooldown auto-deny", "tool", p.ToolName, "run", p.RunID)
		return Decision{Routed: true, Allowed: false, ID: id, Reason: "cooldown"}, nil
	}

	// Open a fresh pending. The broadcast is registered BEFORE the row is
	// written and onOpen fires: a decision that lands immediately (a fast
	// phone, or onOpen deciding synchronously) must find someone to wake.
	s.open[key] = id
	pd := s.registerLocked(id)
	s.mu.Unlock()
	defer s.release(id, pd)

	if err := s.st.UpsertApproval(ctx, a); err != nil {
		s.mu.Lock()
		if s.open[key] == id {
			delete(s.open, key)
		}
		s.mu.Unlock()
		return Decision{}, err
	}
	if s.onOpen != nil {
		s.onOpen(a)
	}
	s.log.Info("approval: opened for decision", "id", id, "tool", p.ToolName, "run", p.RunID)
	o := s.waitOn(ctx, id, pd, a.ExpiresAt)
	s.mu.Lock()
	if s.open[key] == id {
		delete(s.open, key)
	}
	s.recent[key] = time.Now()
	s.mu.Unlock()
	if o == outcomeTimeout {
		// deny-by-default: the deadline passed (or the hook went away) with no
		// answer; record the timeout as the audit line. The request context
		// may already be cancelled — that is exactly the disconnected-hook
		// case — so the write must not inherit its cancellation, or the row
		// would stay pending forever.
		wctx := context.WithoutCancel(ctx)
		if ok, _ := s.st.ResolvePendingApproval(wctx, id, store.ApprovalExpired, "timeout"); ok {
			// Collapsed asks still waiting on this approval read the expiry
			// now instead of each sitting out its own deadline.
			s.broadcast(id)
		}
		return Decision{Routed: true, Allowed: false, ID: id, Reason: "timeout"}, nil
	}
	return Decision{Routed: true, Allowed: o == outcomeAllowed, ID: id, Reason: "decision"}, nil
}

// outcome is the tri-state a blocked approval resolves to: a real human
// DECISION (allow/deny) must never be conflated with the timeout path — the
// timeout is the only branch that writes "expired", so the audit trail keeps
// the human's decision verbatim.
type outcome int

const (
	outcomeTimeout outcome = iota
	outcomeDenied
	outcomeAllowed
)

// Decide records a human/policy decision and releases blocking hooks. It
// returns ErrNotPending, and changes nothing, when the approval has already
// been decided or has expired.
func (s *Service) Decide(ctx context.Context, id string, allow bool, by string) error {
	state := store.ApprovalDenied
	if allow {
		state = store.ApprovalApproved
	}
	ok, err := s.st.ResolvePendingApproval(ctx, id, state, by)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotPending
	}
	s.broadcast(id)
	s.log.Info("approval: decided", "id", id, "allow", allow, "by", by)
	return nil
}

// registerLocked joins (or creates) the broadcast for id. s.mu must be held.
// Every registration is paired with a release.
func (s *Service) registerLocked(id string) *pendingDecision {
	pd, ok := s.dec[id]
	if !ok {
		pd = &pendingDecision{done: make(chan struct{})}
		s.dec[id] = pd
	}
	pd.waiters++
	return pd
}

// release drops one waiter; the entry goes away with the last one.
func (s *Service) release(id string, pd *pendingDecision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pd.waiters--
	if pd.waiters <= 0 && s.dec[id] == pd {
		delete(s.dec, id)
	}
}

// broadcast wakes every waiter on id (at most once per broadcast entry).
func (s *Service) broadcast(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pd, ok := s.dec[id]; ok && !pd.closed {
		pd.closed = true
		close(pd.done)
	}
}

// waitOn blocks until the approval resolves (broadcast) or the deadline
// (expiresAt; 0 = service default). Outcome is read back from the row. The
// caller has already registered pd, so a resolution that happened before
// this call is caught by the row check below and one that happens after it
// closes pd.done.
func (s *Service) waitOn(ctx context.Context, id string, pd *pendingDecision, expiresAt int64) outcome {
	if row, err := s.st.GetApproval(context.WithoutCancel(ctx), id); err == nil && row != nil && row.State != store.ApprovalPending {
		return outcomeFromRow(row)
	}
	d := s.wait
	if expiresAt > 0 {
		d = time.Until(time.UnixMilli(expiresAt))
		if d <= 0 {
			return outcomeTimeout
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return outcomeTimeout
	case <-pd.done:
	case <-timer.C:
		return outcomeTimeout
	}
	row, err := s.st.GetApproval(context.Background(), id)
	if err != nil || row == nil {
		return outcomeDenied // deny-by-default: unknown state is a deny
	}
	return outcomeFromRow(row)
}

// outcomeFromRow maps a resolved row to what the hook is told. Anything but
// an explicit approval is a deny.
func outcomeFromRow(row *store.Approval) outcome {
	switch row.State {
	case store.ApprovalApproved:
		return outcomeAllowed
	case store.ApprovalExpired:
		return outcomeTimeout
	default:
		return outcomeDenied
	}
}

// matchPolicy returns the most-specific matching policy (nil = default ask).
// Specificity: more non-empty constraints => more specific; ties -> latest id.
func (s *Service) matchPolicy(ctx context.Context, project, tool, input string) (*store.ApprovalPolicy, error) {
	pols, err := s.st.ListPolicies(ctx)
	if err != nil {
		return nil, err
	}
	var best *store.ApprovalPolicy
	bestScore := -1
	for _, p := range pols {
		if p.Project != "" && p.Project != project {
			continue
		}
		if p.Tool != "" && p.Tool != tool {
			continue
		}
		if p.Pattern != "" {
			re, err := regexp.Compile(p.Pattern)
			if err != nil {
				s.log.Warn("approval: bad policy pattern", "id", p.ID, "pattern", p.Pattern, "err", err)
				continue
			}
			if !re.MatchString(input) {
				continue
			}
		}
		score := 0
		if p.Project != "" {
			score++
		}
		if p.Tool != "" {
			score++
		}
		if p.Pattern != "" {
			score++
		}
		if score > bestScore {
			best = p
			bestScore = score
		}
	}
	return best, nil
}

// matchText extracts the semantic text a policy pattern should match: for
// Bash the command string, for file tools the path (tool_input arrives as
// rendered JSON, so patterns like `^ls…` / `^/workspace/…` match the meaning).
func matchText(tool, input string) string {
	if input == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(input), &m) != nil {
		return input
	}
	for _, key := range []string{"command", "file_path", "filePath", "path"} {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return input
}

func ruleLabel(p *store.ApprovalPolicy) string {
	if p == nil {
		return "policy:default"
	}
	return fmt.Sprintf("policy:%d", p.ID)
}

func preview(input string) string {
	if len(input) <= MaxInputPreview {
		return input
	}
	return input[:MaxInputPreview] + "…[truncated]"
}

func hashOf(input string) string {
	if input == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(input))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ap-%d", time.Now().UnixNano())
	}
	return "ap-" + hex.EncodeToString(b[:])
}
