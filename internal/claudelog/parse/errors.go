// Package parse implements the tolerant, normalizing readers for all three
// Claude Code capture paths (JSONL transcripts, stream-json stdout protocol,
// and hook stdin payloads). Every reader emits the canonical eventmodel.Event
// shape and never panics on unknown fields: unknown top-level
// types fall back to event=meta with subtype "raw:<type>" and the raw record is
// retained verbatim.
//
// Layering: eventmodel -> parse -> tailer. The parse package imports only
// eventmodel.
package parse

import (
	"errors"
	"fmt"
)

// ErrLineTooLong is returned by the line readers when a single line exceeds the
// configured maximum. It is deliberately an error (not a silent truncate) so
// callers can surface the offending offset; the sweep harness counts these.
var ErrLineTooLong = errors.New("line exceeds maximum configured size")

// ErrCorruptLine is returned when a raw line is not valid JSON (or not a JSON
// object) and cannot be represented even as an opaque meta event. Tolerant
// parsing keeps the count of these at zero on the real corpus.
var ErrCorruptLine = errors.New("invalid JSON line")

// LineError annotates a line-level failure with the byte offset of the record
// so callers (sweep, tailer) can report exactly where parsing paused.
type LineError struct {
	// Offset is the byte offset at which the failing line starts.
	Offset int64
	Err    error
}

func (e *LineError) Error() string {
	return fmt.Sprintf("offset %d: %v", e.Offset, e.Err)
}

// Unwrap exposes the underlying error for errors.Is/As.
func (e *LineError) Unwrap() error { return e.Err }
