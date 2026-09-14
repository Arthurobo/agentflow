package parse

import (
	"io"

	gojson "github.com/goccy/go-json"

	"github.com/arthurobo/agentflow/internal/claudelog/eventmodel"
)

// Options configure a reader. They are passed by value and shared across the
// JSONL / stream / hook readers.
type Options struct {
	// MaxLineSize caps a single raw line in bytes (default DefaultMaxLineSize).
	MaxLineSize int
	// MachineID is stamped on every emitted event.
	MachineID string
	// Source overrides the source used for emitted events. The default is the
	// reader's natural source (tailer/stream-json/hook).
	Source eventmodel.Source
	// RetainRaw keeps the exact line bytes on Parsed.Raw / Event.Raw (default
	// true). Set false for bulk scans that only need normalized output — the
	// reader then reuses its internal buffer (big allocation win; measured in
	// ) and Raw is dropped.
	RetainRaw bool
}

// JSONLParser reads a JSONL transcript (session or subagent .jsonl) line by
// line, normalizing each record and stamping per-file sequence numbers and byte
// offsets. The Source defaults to backfill for bulk reads; callers that are
// tailing a live file should pass Source=tailer.
type JSONLParser struct {
	rd     *LineReader
	opts   Options
	source eventmodel.Source
	lineNo int64
}

// NewJSONLParser wraps r (an os.File, io.Reader, or in-memory bytes.Reader).
func NewJSONLParser(r io.Reader, opts Options) *JSONLParser {
	src := opts.Source
	if src == "" {
		src = eventmodel.SourceBackfill
	}
	jp := &JSONLParser{
		rd:     NewLineReader(r, opts.MaxLineSize),
		opts:   opts,
		source: src,
	}
	if !opts.RetainRaw {
		jp.rd.EnableBufferReuse()
	}
	return jp
}

// Next returns the next normalized record, or io.EOF when the transcript is
// exhausted. On a corrupt/oversized line it returns the LineError; the caller
// may continue with subsequent lines if it wants to (reads are independent).
func (jp *JSONLParser) Next() (*Parsed, error) {
	raw, off, err := jp.rd.ReadLine()
	if err == io.EOF {
		return nil, io.EOF
	}
	if err != nil {
		return nil, err
	}
	p, perr := Normalize(raw, jp.source)
	if perr != nil {
		return nil, &LineError{Offset: off, Err: perr}
	}
	jp.lineNo++
	p.RawLen = len(p.Raw)
	if !jp.opts.RetainRaw {
		p.Raw = nil
	}
	for _, ev := range p.Events {
		ev.Seq = jp.lineNo
		ev.FileOffset = off
		ev.MachineID = jp.opts.MachineID
		if !jp.opts.RetainRaw {
			ev.Raw = nil
		}
	}
	return p, nil
}

// LineNo reports the 1-based sequence number of the most recently returned
// record.
func (jp *JSONLParser) LineNo() int64 { return jp.lineNo }

// StreamParser reads the stream-json protocol (NDJSON on stdout of
// `claude --output-format stream-json`, ). It carries run state so
// `system/init` is treated as repeatable
// and its cwd/model/session_id are stamped on records that omit them.
type StreamParser struct {
	rd         *LineReader
	opts       Options
	source     eventmodel.Source
	seq        int64
	stateCwd   string
	stateSess  string
	stateModel string
}

// NewStreamParser wraps r with a stream-json event stream.
func NewStreamParser(r io.Reader, opts Options) *StreamParser {
	sp := &StreamParser{
		rd:     NewLineReader(r, opts.MaxLineSize),
		opts:   opts,
		source: eventmodel.SourceStream,
	}
	if !opts.RetainRaw {
		sp.rd.EnableBufferReuse()
	}
	return sp
}

// Next returns the next normalized record or io.EOF. It never blocks past the
// next line so callers are free to reserve goroutines around it.
func (sp *StreamParser) Next() (*Parsed, error) {
	raw, off, err := sp.rd.ReadLine()
	if err == io.EOF {
		return nil, io.EOF
	}
	if err != nil {
		return nil, err
	}
	p, perr := Normalize(raw, sp.source)
	if perr != nil {
		return nil, &LineError{Offset: off, Err: perr}
	}

	// system/init is repeatable and carries run-level fields we re-capture even
	// though the normalized event keeps them minimal.
	if len(p.Events) > 0 && p.Events[0].Type == eventmodel.EventSystemMeta &&
		p.Events[0].Subtype == eventmodel.SubtypeInit {
		var m map[string]any
		if err := gojson.Unmarshal(raw, &m); err == nil {
			if s, ok := m["cwd"].(string); ok {
				sp.stateCwd = s
			}
			if s, ok := m["session_id"].(string); ok {
				sp.stateSess = s
			}
			if s, ok := m["model"].(string); ok {
				sp.stateModel = s
			}
		}
	}

	sp.seq++
	for _, ev := range p.Events {
		ev.Seq = sp.seq
		ev.FileOffset = off
		ev.MachineID = sp.opts.MachineID
		ev.Source = sp.source
		// Stamp run-level state where the record itself is silent (stream
		// records other than init rarely carry cwd; model lives on assistant).
		if ev.CWD == "" && sp.stateCwd != "" {
			ev.CWD = sp.stateCwd
			ev.Project = eventmodel.ProjectFromCWD(sp.stateCwd)
		}
		if ev.SessionID == "" && sp.stateSess != "" {
			ev.SessionID = sp.stateSess
		}
		if ev.Model == "" && ev.Type == eventmodel.EventAssistantMessage && sp.stateModel != "" {
			ev.Model = sp.stateModel
		}
	}
	return p, nil
}
