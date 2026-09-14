package main

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arthurobo/agentflow/internal/engine/opencode"
	"github.com/arthurobo/agentflow/internal/spawner"
	"github.com/arthurobo/agentflow/internal/store"
)

// reporter guarantees that a member's work reaches the orchestrator.
//
// Until now a report arrived only if the member chose to POST it. Nothing
// captured a member's output and nothing noticed one that finished and went
// quiet, so a single skipped curl stopped the loop with no recovery path:
// a live OpenCode investigator wrote a complete PR count report into its own
// terminal, never sent it, and its orchestrator sat on an empty inbox while
// the loop stayed on its investigate step.
//
// The courier made the INBOUND direction certain — mail is pushed into a
// member's terminal rather than waited for. This is the outbound half. There
// are three ways the work can arrive, in order of preference:
//
//  1. The member posts it, because the brief says it is not done until it
//     has. This is the normal path and the other two should be rare.
//  2. It is told to. One nudge into its terminal naming the command.
//  3. agentd hands the captured output to the orchestrator as an ADVISORY
//     from the engine, tagged as a capture.
//
// Step 3 is deliberately not a post from the member. A capture has not been
// through whatever the member would have said about it, so it must not move
// the play or discharge the member's brief as if the member had answered:
// the orchestrator reads it and decides what to do. Only a member holding a
// step brief from the engine is chased at all (the store filters on that),
// so a member holding a note from the engineer is never put words in the
// mouth of.
type reporter struct {
	st      *store.Store
	submit  func(runID, text string) error
	control controlPrompt
	log     *slog.Logger

	mu sync.Mutex
	// acted remembers what has been done per BRIEF, not per member. A member
	// that is nudged, ignores the nudge and is then forwarded must not be
	// nudged again for the same brief; a member owing a SECOND brief later
	// is a new situation and gets the full ladder again.
	acted map[string]*reportAttempt
}

type reportAttempt struct {
	// at is when this brief was first seen owing a report, for forgetting.
	at       time.Time
	nudgedAt time.Time
	// done means nothing more will be done for this brief: the advisory was
	// sent, or it was refused for good.
	done bool
}

// reporterMemory is how long an attempt is remembered. Far longer than any
// sweep cycle and grace, so a brief is never chased twice, and bounded so a
// long-lived daemon does not keep one entry per brief forever.
const reporterMemory = 6 * time.Hour

// controlPrompt reaches an OpenCode member, which has no PTY this host
// writes to. A function rather than the spawner itself so the reporter is
// testable without a process; nil means this deployment cannot reach one.
type controlPrompt func(ctx context.Context, runID, text string) error

// reportGrace is how long a nudged member has to send its own report before
// agentd sends it for them.
//
// One sweep cycle plus room: long enough that a member which acted on the
// nudge wins the race and its own post lands first, short enough that a loop
// is not held open by a member that is never going to answer.
const reportGrace = 90 * time.Second

// reportNudge is what a member is told. It names the fact and the command,
// and nothing else.
const reportNudge = "You have finished but have not reported. " +
	"POST your findings to the ORCHESTRATOR now with the verdict command in your brief. " +
	"You are not done until that POST succeeds."

// autoForwardPrefix marks a report agentd captured rather than received. The
// orchestrator must never mistake a capture for the member speaking: a
// captured report has not been through whatever the member would have said
// about it, and reading it as a considered answer is how a wrong conclusion
// gets treated as a checked one.
func autoForwardSubject(role string) string {
	return "[auto-forwarded] " + role + " produced this but did not send it"
}

func autoForwardBody(role, report string) string {
	return "[auto-forwarded — " + role + " produced this but did not send it. " +
		"agentd captured it from the member's transcript after it finished and went quiet. " +
		"Treat it as raw output, not as a report the member stands behind. " +
		"The play has not moved: " + role + " still owes its report. Ask it to send one, " +
		"or decide the work is done some other way.]\n\n" + report
}

func newReporter(st *store.Store, submit func(runID, text string) error,
	control controlPrompt, log *slog.Logger) *reporter {
	return &reporter{st: st, submit: submit, control: control, log: log,
		acted: map[string]*reportAttempt{}}
}

// Handle runs the ladder for one member that owes a report.
func (r *reporter) Handle(ctx context.Context, u store.UnreportedMember) {
	if strings.TrimSpace(u.Report) == "" {
		// Nothing to forward and nothing to nudge about. Never post empty
		// output: an empty message to the orchestrator reads as an answer.
		return
	}
	key := u.MemberID + "@" + strconv.FormatInt(u.BriefAt, 10)
	r.mu.Lock()
	r.pruneLocked(time.Now())
	att, ok := r.acted[key]
	if !ok {
		att = &reportAttempt{at: time.Now()}
		r.acted[key] = att
	}
	nudged, done := att.nudgedAt, att.done
	r.mu.Unlock()

	if done {
		return
	}
	if nudged.IsZero() {
		// First remedy: ask. A member that reports its own findings has
		// said what it thinks of them, which a capture can never do.
		if r.nudge(ctx, u) {
			r.mu.Lock()
			att.nudgedAt = time.Now()
			r.mu.Unlock()
			r.log.Info("agentd: member finished without reporting; asked it to",
				"loop", u.LoopID, "role", u.Role, "run", u.RunID)
			return
		}
		// Could not reach it at all: a member with no process here, or a
		// terminal that is gone. There is nobody to ask, so the guarantee
		// falls straight to the forward rather than waiting out a grace
		// nobody is going to use.
		r.log.Info("agentd: member finished without reporting and cannot be asked",
			"loop", u.LoopID, "role", u.Role, "run", u.RunID)
	} else if time.Since(nudged) < reportGrace {
		return // it was asked; give it the chance to answer for itself
	}

	if err := r.forward(ctx, u); err != nil {
		if permanentRefusal(err) {
			// Retrying cannot help: the loop has ended, the member or the
			// orchestrator is gone, or the play refuses the route. Stop
			// here instead of trying again on every sweep.
			r.mu.Lock()
			att.done = true
			r.mu.Unlock()
			r.log.Info("agentd: auto-forward refused for good", "loop", u.LoopID, "role", u.Role, "err", err)
			return
		}
		r.log.Warn("agentd: auto-forward failed", "loop", u.LoopID, "role", u.Role, "err", err)
		return
	}
	r.mu.Lock()
	att.done = true
	r.mu.Unlock()
	r.log.Warn("agentd: passed a member's unsent output to the orchestrator",
		"loop", u.LoopID, "role", u.Role, "run", u.RunID, "chars", len(u.Report))
}

// permanentRefusal reports whether a forward failed for a reason that another
// attempt cannot change.
func permanentRefusal(err error) bool {
	var violation *store.StepViolation
	return errors.As(err, &violation) || errors.Is(err, store.ErrLoopEnded) ||
		errors.Is(err, store.ErrMailNotFound) || errors.Is(err, errMemberGone)
}

// errMemberGone is a forward for a member that no longer exists.
var errMemberGone = errors.New("reporter: the member is gone")

// nudge asks the member to report, and says whether it could be asked.
func (r *reporter) nudge(ctx context.Context, u store.UnreportedMember) bool {
	if u.RunID == "" || r.submit == nil {
		return false
	}
	if store.NormaliseTool(u.Tool) == store.ToolOpenCode {
		if r.control == nil {
			return false
		}
		if err := r.control(ctx, u.RunID, reportNudge); err != nil {
			r.log.Debug("agentd: report nudge opencode", "run", u.RunID, "err", err)
			return false
		}
		return true
	}
	if err := r.submit(u.RunID, reportNudge); err != nil {
		r.log.Debug("agentd: report nudge", "run", u.RunID, "err", err)
		return false
	}
	return true
}

// forward hands the member's captured output to the orchestrator as an
// advisory from the engine.
//
// It is not posted as the member. A post as the member went through the step
// engine, so a capture nobody had looked at advanced the play and discharged
// the member's brief exactly as a considered report would, and the next step
// was briefed on raw output. The advisory tells the orchestrator what was
// captured and leaves the play where it is; the orchestrator decides whether
// to ask the member again, act on the output, or retire the member.
func (r *reporter) forward(ctx context.Context, u store.UnreportedMember) error {
	member, err := r.st.LoopMemberByID(ctx, u.MemberID)
	if err != nil {
		return err
	}
	if member == nil || member.Status == store.LoopMemberDismissed {
		return errMemberGone
	}
	_, err = r.st.PostAdvisory(ctx, u.LoopID, store.RoleOrchestrator,
		autoForwardSubject(u.Role), autoForwardBody(u.Role, u.Report),
		map[string]any{"fromRole": u.Role, "memberId": u.MemberID, "chars": len(u.Report)})
	return err
}

// pruneLocked forgets attempts older than reporterMemory. The caller holds mu.
func (r *reporter) pruneLocked(now time.Time) {
	for k, a := range r.acted {
		if now.Sub(a.at) > reporterMemory {
			delete(r.acted, k)
		}
	}
}

// opencodePrompt reaches an OpenCode member through its control API, which is
// the only way in: agentd holds no PTY for one.
func opencodePrompt(sp *spawner.Spawner) controlPrompt {
	if sp == nil {
		return nil
	}
	return func(ctx context.Context, runID, text string) error {
		run, err := sp.Status(ctx, runID)
		if err != nil {
			return err
		}
		if run == nil || run.ControlBase == "" {
			return errors.New("reporter: run has no control API")
		}
		c := opencode.NewControl(run.ControlBase)
		if run.SessionID != "" {
			c.BindSession(run.SessionID)
		}
		return c.Prompt(ctx, text)
	}
}
