package parse

import (
	"bufio"
	"io"
)

// DefaultMaxLineSize is 16 MiB. Corpus lines reach ~2.1 MB
// so this gives an order-of-magnitude headroom while bounding memory for truly
// pathological input. Configurable via SetMaxLineSize / the parser options.
const DefaultMaxLineSize = 16 * 1024 * 1024

// readerBufSize is the internal bufio.Reader buffer. Tuned to cover the common
// case (most JSONL lines are well under 64 KiB) with a single ReadSlice call.
const readerBufSize = 64 * 1024

// LineReader reads newline-delimited records with a configurable maximum line
// size and tracks the byte offset of each line start. It is the primitive used
// by the JSONL reader, the stream-json reader and the tailer — bufio.Reader
// with a growable slice, never bufio.Scanner (whose default token limit
// truncates the ~2.1 MB corpus lines).
type LineReader struct {
	r           *bufio.Reader
	maxLine     int
	lineOffset  int64 // byte offset where the next line will start
	byteCount   int64 // total bytes consumed from the underlying reader
	eofReported bool  // underlying stream exhausted and final line emitted
	reuse       bool  // reuse the line buffer across calls (caller must not retain)
	line        []byte
}

// NewLineReader wraps r with the given maximum line size (<=0 means the
// package default).
func NewLineReader(r io.Reader, maxLine int) *LineReader {
	if maxLine <= 0 {
		maxLine = DefaultMaxLineSize
	}
	return &LineReader{
		r:       bufio.NewReaderSize(r, readerBufSize),
		maxLine: maxLine,
	}
}

// EnableBufferReuse switches the reader into zero-alloc mode: the returned
// line slice is valid only until the next ReadLine call. Only safe for
// callers that do not retain raw bytes (e.g. sweep passes RetainRaw=false).
func (lr *LineReader) EnableBufferReuse() {
	lr.reuse = true
}

// ReadLine returns the next raw line without a trailing newline, the byte
// offset where the line starts, and io.EOF when the underlying stream is
// exhausted. A final unterminated line (file does not end with '\n') is
// returned normally; the following call returns io.EOF.
func (lr *LineReader) ReadLine() (line []byte, off int64, err error) {
	if lr.eofReported {
		return nil, lr.lineOffset, io.EOF
	}
	off = lr.lineOffset
	buf := lr.line[:0]
	for {
		frag, rerr := lr.r.ReadSlice('\n')
		buf = append(buf, frag...)
		if rerr == bufio.ErrBufferFull {
			if lr.maxLine > 0 && len(buf) > lr.maxLine {
				return buf, off, &LineError{Offset: off, Err: ErrLineTooLong}
			}
			// Not a complete line yet; keep growing across the buffer boundary.
			continue
		}
		if rerr == io.EOF && len(buf) == 0 {
			lr.eofReported = true
			return nil, off, io.EOF
		}
		if rerr == io.EOF {
			// Final unterminated line: return it now, report EOF next call.
			lr.eofReported = true
		}
		// rerr is nil (complete line) or io.EOF (final unterminated line).
		n := len(buf)
		if n > 0 && buf[n-1] == '\n' {
			n--
		}
		if lr.maxLine > 0 && n > lr.maxLine {
			return buf, off, &LineError{Offset: off, Err: ErrLineTooLong}
		}
		consumed := len(buf)
		if len(buf) > 0 && buf[len(buf)-1] == '\n' {
			if len(buf) > 1 && buf[len(buf)-2] == '\r' {
				consumed = len(buf) - 2
			} else {
				consumed = len(buf) - 1
			}
		}
		lr.byteCount += int64(len(buf))
		lr.lineOffset += int64(len(buf))
		if lr.reuse {
			lr.line = buf
			return buf[:consumed], off, nil
		}
		lr.line = buf
		out := make([]byte, consumed)
		copy(out, buf[:consumed])
		return out, off, nil
	}
}
