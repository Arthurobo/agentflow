// spawn.go — starting the crew. Create returns as soon as the ROWS exist and
// the processes come up behind it, one at a time.
//
// Two orderings here are load bearing and neither is a preference.
//
// Rows before processes. The engine this replaced spawned first and inserted
// afterwards, and its own comment records what that cost: "a live,
// fully-permissioned claude member that no loop action could reach for over
// two minutes". A member that exists as a process and not as a row cannot be
// stopped, addressed, or shown.
//
// One at a time. Claude session-id discovery picks the newest transcript in
// the cwd modified after the process started, minus a snapshot of the ids
// already claimed. Four TUIs launched into one directory within milliseconds
// each take that snapshot before any of the others has written anything, so
// they can all converge on the same file and end up bound to one session.
// Each spawn therefore waits for the one before it and adds the id it found to
// the next one's exclusions.

package loopapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

// maxPromptBytes bounds what may ride argv. The measured single-argument
// ceiling on this machine is 131071 bytes and an orchestrator brief is about
// 8 KB, so this is not a limit anyone reaches; it exists so that if a prompt
// ever does grow past it we refuse the member instead of handing execve a
// truncated brief. BYTES, not runes: the kernel counts bytes.
const maxPromptBytes = 120000

// maxTitleTask bounds the name half of a member's session name. The sessions
// list shows one line per row, and a title long enough to wrap pushes the role
// off the end, which is the thing the name exists to show.
const maxTitleTask = 42

// memberTitle names a member session so the sessions list is readable with
// several loops running. Every member used to be called by its role alone, so
// three loops produced nine rows reading ORCHESTRATOR, INVESTIGATION, REVIEW
// three times over with nothing to tell them apart.
//
// The fan-out suffix survives on purpose: INVESTIGATION#2 is a different agent
// from INVESTIGATION and the list has to say so.
func memberTitle(title, role string) string {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return role
	}
	if len(title) > maxTitleTask {
		// Cut on a word boundary where there is one close enough, so the name
		// ends in a word rather than mid-syllable.
		cut := title[:maxTitleTask]
		if i := strings.LastIndex(cut, " "); i > maxTitleTask/2 {
			cut = cut[:i]
		}
		title = strings.TrimRight(cut, " ,.:;") + "…"
	}
	return title + " -> " + role
}

// LaunchSpec is one member's process, everything the engine needs and nothing
// about the loop.
type LaunchSpec struct {
	Role  string
	Tool  string
	Model string
	Cwd   string
	Title string
	// Prompt is the member's whole brief, including the commands it reaches
	// the loop with. It rides argv, which any local user can read, so the
	// launcher must take Token out of it before it gets there.
	Prompt string
	// Token is the member's credential as it appears in Prompt.
	Token string
	// ExcludeSessionIDs are engine session ids this spawn must never claim.
	ExcludeSessionIDs []string
}

// LaunchResult is what came back from starting one member.
type LaunchResult struct {
	// SessionID is the engine's own session id, when it reported one.
	SessionID string
	// Identified says the engine told us who it is. False means the process
	// started and never said, which for claude means discovery timed out. An
	// unidentified member is worse than a missing one: it is alive, it is
	// holding a transcript nobody has claimed, and the NEXT member's discovery
	// can bind to it.
	Identified bool
}

// Launcher starts loop members. The spawner implements it through an adapter
// in cmd/agentd; this package never sees a process.
type Launcher interface {
	Launch(ctx context.Context, runID string, spec LaunchSpec) (LaunchResult, error)
	// Stop terminates a run. TTY members do not end on a closed stdin, so this
	// must be the real termination path.
	Stop(ctx context.Context, runID string) error
}

// memberPlan is one member the background spawn will start.
type memberPlan struct {
	Role   string
	Tool   string
	Model  string
	RunID  string
	Prompt string
	Token  string
}

// spawnOrder puts the orchestrator LAST. It is the member that reads the
// roster and starts talking, so it should not begin before the members it will
// address exist as processes.
func spawnOrder(plans []memberPlan) []memberPlan {
	out := make([]memberPlan, 0, len(plans))
	var orchestrators []memberPlan
	for _, p := range plans {
		if store.BaseRole(p.Role) == store.RoleOrchestrator {
			orchestrators = append(orchestrators, p)
			continue
		}
		out = append(out, p)
	}
	return append(out, orchestrators...)
}

// spawnCrew starts every member sequentially. It runs detached from the
// request that created the loop, so it takes its own context.
//
// One member failing never fails the loop. Losing three working agents because
// a fourth named a binary that is not installed is worse than a loop with a
// gap in it, and the gap is recoverable: the row is there, the token is there,
// and the engineer can rotate it and start that one by hand.
func (s *Server) spawnCrew(ctx context.Context, loopID, title, cwd string, plans []memberPlan) {
	if s.launcher == nil {
		return
	}
	// Seed exclusions with every session id any run already owns, so a member
	// cannot adopt a transcript that belongs to something else on this machine.
	exclude := []string{}
	if claimed, err := s.st.ListClaimedSessionIDs(ctx); err == nil {
		for sid := range claimed {
			if sid != "" {
				exclude = append(exclude, sid)
			}
		}
	} else {
		s.log.Warn("loopapi: claimed session ids", "loop", loopID, "err", err)
	}

	for _, p := range spawnOrder(plans) {
		// The loop can end while the crew is still coming up, and the spawn
		// runs for minutes. Every member launched after the end would be a
		// live process nothing will ever stop.
		if s.loopEnded(ctx, loopID) {
			s.log.Info("loopapi: loop ended while its crew was starting; not launching the rest",
				"loop", loopID, "role", p.Role)
			return
		}
		if n := len(p.Prompt); n > maxPromptBytes {
			s.failMember(ctx, loopID, p.Role,
				fmt.Sprintf("brief is %d bytes, over the %d argv budget", n, maxPromptBytes))
			continue
		}
		res, err := s.launcher.Launch(ctx, p.RunID, LaunchSpec{
			Role: p.Role, Tool: p.Tool, Model: p.Model, Cwd: cwd,
			Title: memberTitle(title, p.Role), Prompt: p.Prompt, Token: p.Token,
			ExcludeSessionIDs: append([]string(nil), exclude...),
		})
		if err != nil {
			s.failMember(ctx, loopID, p.Role, err.Error())
			continue
		}
		// Ended while this one was starting: the end could not have seen a
		// run that did not exist yet, so this is the only place it gets
		// stopped.
		if s.loopEnded(ctx, loopID) {
			s.StopRuns([]string{p.RunID})
			return
		}
		if !res.Identified {
			// Alive and anonymous. Stop it before the next member's discovery
			// can bind to the transcript it is writing.
			if err := s.launcher.Stop(ctx, p.RunID); err != nil {
				s.log.Warn("loopapi: stop unidentified member", "loop", loopID, "role", p.Role, "err", err)
			}
			s.failMember(ctx, loopID, p.Role,
				"started but never reported a session id, so it was stopped rather than left to collide with the next member")
			continue
		}
		if res.SessionID != "" {
			exclude = append(exclude, res.SessionID)
		}
		if err := s.st.MarkLoopMemberStarted(ctx, loopID, p.Role, p.RunID, res.SessionID); err != nil {
			s.log.Warn("loopapi: mark member started", "loop", loopID, "role", p.Role, "err", err)
		}
	}
}

// loopEnded reports whether the loop is gone or has ended. A read error
// counts as ended: launching a process nothing can account for is the worse
// mistake.
func (s *Server) loopEnded(ctx context.Context, loopID string) bool {
	l, err := s.st.GetLoop(ctx, loopID)
	if err != nil {
		s.log.Warn("loopapi: re-read loop before launch", "loop", loopID, "err", err)
		return true
	}
	return l == nil || l.Ended()
}

func (s *Server) failMember(ctx context.Context, loopID, role, reason string) {
	s.log.Warn("loopapi: member did not start", "loop", loopID, "role", role, "reason", reason)
	if err := s.st.MarkLoopMemberFailed(ctx, loopID, role, reason); err != nil {
		s.log.Warn("loopapi: record member failure", "loop", loopID, "role", role, "err", err)
	}
}

// spawnBudget bounds one whole crew. Claude's own discovery budget is 90s per
// member, so a four-member crew needs room for four of them in series.
const spawnBudget = 8 * time.Minute
