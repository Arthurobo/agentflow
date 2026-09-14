package parse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLineReaderBasic(t *testing.T) {
	in := "{\"a\":1}\n{\"b\":2}\n"
	lr := NewLineReader(strings.NewReader(in), 0)
	line, off, err := lr.ReadLine()
	if err != nil || string(line) != `{"a":1}` || off != 0 {
		t.Fatalf("line1=%q off=%d err=%v", line, off, err)
	}
	line, off, err = lr.ReadLine()
	if err != nil || string(line) != `{"b":2}` || off != 8 {
		t.Fatalf("line2=%q off=%d err=%v", line, off, err)
	}
	if _, _, err := lr.ReadLine(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestLineReaderHugeLine(t *testing.T) {
	// ~2.1 MB line like the corpus max (K5).
	big := `{"type":"user","content":"` + strings.Repeat("x", 2*1024*1024) + `"}`
	in, want := big+"\n", big
	lr := NewLineReader(strings.NewReader(in), DefaultMaxLineSize)
	line, off, err := lr.ReadLine()
	if err != nil {
		t.Fatalf("huge line failed: %v", err)
	}
	if len(line) != len(want) || off != 0 {
		t.Fatalf("len=%d want %d, off=%d", len(line), len(want), off)
	}
	if !strings.HasSuffix(string(line), `"}`) {
		t.Fatal("huge line must not be truncated by the default reader")
	}
}

func TestLineReaderMaxLineTooLong(t *testing.T) {
	in := strings.Repeat("z", 100) + "\n"
	lr := NewLineReader(strings.NewReader(in), 50)
	_, _, err := lr.ReadLine()
	if err == nil {
		t.Fatal("want ErrLineTooLong, got nil")
	}
	var le *LineError
	if !errors.As(err, &le) {
		t.Fatalf("want *LineError, got %T: %v", err, err)
	}
	if !errors.Is(le.Err, ErrLineTooLong) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLineReaderFinalLineWithoutNewline(t *testing.T) {
	in := "{\"a\":1}\n{\"b\":2}" // no trailing newline
	lr := NewLineReader(strings.NewReader(in), 0)
	if line, _, err := lr.ReadLine(); err != nil || string(line) != `{"a":1}` {
		t.Fatalf("line1: %q %v", line, err)
	}
	// Final unterminated line is returned normally; the NEXT call is EOF.
	if line, off, err := lr.ReadLine(); err != nil || string(line) != `{"b":2}` {
		t.Fatalf("final line: %q off=%d err=%v", line, off, err)
	}
	if _, _, err := lr.ReadLine(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestLineReaderCRLF(t *testing.T) {
	lr := NewLineReader(strings.NewReader("{\"a\":1}\r\n{\"b\":2}\r\n"), 0)
	if line, _, err := lr.ReadLine(); err != nil || string(line) != `{"a":1}` {
		t.Fatalf("crlf line: %q err=%v", line, err)
	}
}

func TestLineReaderEmptyLines(t *testing.T) {
	lr := NewLineReader(strings.NewReader("{\"a\":1}\n\n{\"b\":2}\n"), 0)
	seen := 0
	for {
		line, _, err := lr.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(line) > 0 {
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("want 2 non-empty lines, got %d", seen)
	}
}
