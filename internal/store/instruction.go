package store

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInstructionTooThin is the loop refusing to start on a name.
var ErrInstructionTooThin = errors.New("store: instruction is too thin to act on")

// MinInstructionChars and MinInstructionWords are the floor.
//
// Every loop created so far carried two to four words, because the one field
// was read as a name by the list, the board and the session title while being
// labelled as the instruction. The orchestrator then had nothing to work from
// and had to ask, or invent. The floor is what makes the two fields mean two
// different things rather than the label claiming it.
const (
	MinInstructionChars = 20
	MinInstructionWords = 4
)

// ValidateInstruction refuses a task that is a title wearing the wrong label.
//
// It is deliberately crude. It cannot tell a good instruction from a bad one
// and does not try; it catches the one failure that actually happened, which
// is the engineer typing the same short name into both halves of the form.
func ValidateInstruction(title, task string) error {
	t := strings.Join(strings.Fields(task), " ")
	if t == "" {
		return fmt.Errorf("%w: say what the loop should do", ErrInstructionTooThin)
	}
	if name := strings.Join(strings.Fields(title), " "); name != "" && strings.EqualFold(name, t) {
		return fmt.Errorf("%w: the instruction repeats the name; say what to do, not what to call it",
			ErrInstructionTooThin)
	}
	if n := utf8.RuneCountInString(t); n < MinInstructionChars {
		return fmt.Errorf("%w: %d characters, need at least %d", ErrInstructionTooThin, n, MinInstructionChars)
	}
	if n := len(strings.Fields(t)); n < MinInstructionWords {
		return fmt.Errorf("%w: %d words, need at least %d", ErrInstructionTooThin, n, MinInstructionWords)
	}
	return nil
}
