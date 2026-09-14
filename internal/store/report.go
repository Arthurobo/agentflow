// report.go — what a member actually produced, read back from the events it
// emitted.
//
// A loop only moves when a member POSTs its report, and posting is voluntary:
// nothing captures a member's output and nothing notices a member that
// finished and stayed quiet. A live OpenCode investigator wrote a complete
// "PR Count Report" into its own terminal and never sent it, and the loop sat
// on its investigate step indefinitely with the orchestrator's inbox empty.
//
// The work is not lost, though. It is in the events table for BOTH engines,
// which is the fact this file is built on.

package store

import (
	"context"
	"database/sql"
	"strings"
)

// whitespaceChars is what SQLite must strip to decide a message is blank.
//
// TRIM(x) alone strips SPACES and nothing else, so a message of "  \n  " is
// not empty to it. An assistant message that is only a newline is real and
// common, and forwarding one posts a blank report to the orchestrator.
const whitespaceChars = "' ' || char(9) || char(10) || char(13)"

// MemberOutput is what a member has emitted since a moment: its last complete
// answer, and when it last did anything at all.
type MemberOutput struct {
	// Report is the last NON-EMPTY assistant message at or after `since`.
	// Empty when the member has produced no answer since then.
	Report string
	// ReportAt is when that answer landed.
	ReportAt int64
	// LastEventAt is the newest event of any kind for the session, which is
	// how quiet the member has gone. A member mid-turn is emitting tool
	// calls and results continuously; one that has finished emits nothing.
	LastEventAt int64
}

// LastReportSince reads a member's most recent answer out of the transcript.
//
// It works for both engines, which is worth stating because it is not what
// the corpus gap would suggest. OpenCode sessions are absent from the
// `sessions` index, so they do not appear in the sessions list without the
// managed-row merge; their EVENTS are present all the same, stamped
// source='opencode' by the live SSE consumer. Verified against the live
// database: the OpenCode investigator whose report went missing has that
// report in events, 2,827 characters of it, as an assistant_message.
//
// Ordered by ts, NOT by seq. Every OpenCode event row carries seq = 0 —
// the sequence number is a Claude transcript concept — so ordering by seq
// returns an arbitrary row for exactly the engine this exists to rescue.
func (s *Store) LastReportSince(ctx context.Context, sessionID string, since int64) (MemberOutput, error) {
	var out MemberOutput
	if strings.TrimSpace(sessionID) == "" {
		return out, nil
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ts), 0) FROM events WHERE session_id = ?`,
		sessionID).Scan(&out.LastEventAt)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT content, ts FROM events
		 WHERE session_id = ? AND event = 'assistant_message'
		   AND ts >= ? AND TRIM(COALESCE(content, ''), `+whitespaceChars+`) != ''
		 ORDER BY ts DESC, seq DESC LIMIT 1`, sessionID, since)
	err = row.Scan(&out.Report, &out.ReportAt)
	if err == sql.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	return out, nil
}
