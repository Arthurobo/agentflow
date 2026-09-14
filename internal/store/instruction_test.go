package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/arthurobo/agentflow/internal/store"
)

// The floor exists to catch one real failure: the engineer typing the same
// short name into both halves of the form and the orchestrator being handed
// nothing to act on.
func TestValidateInstructionRefusesANameAndAcceptsAnInstruction(t *testing.T) {
	for _, tc := range []struct {
		name, title, task string
		ok                bool
	}{
		{"the observed failure", "New PR Test", "New PR Test", false},
		{"short and wordy enough is still short", "x", "a b c d e f", false},
		{"long enough but three words", "x", "aaaaaaaaaa bbbbbbbbbb cccccccccc", false},
		{"whitespace only", "x", " \t\n ", false},
		{"repeats the name with different spacing and case", "Flaky Export", "flaky    export", false},
		{"a real instruction", "Flaky export", "Find why the guest export goes flaky above 500 rows", true},
		{"no title given", "", "Find why the guest export goes flaky above 500 rows", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := store.ValidateInstruction(tc.title, tc.task)
			if tc.ok && err != nil {
				t.Fatalf("must be accepted: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("must be refused")
				}
				if !errors.Is(err, store.ErrInstructionTooThin) {
					t.Fatalf("must be the typed refusal, got %v", err)
				}
				// The message has to say what is missing, or the form cannot
				// tell him which half to fix.
				if !strings.Contains(err.Error(), "instruction") {
					t.Fatalf("the reason must name the instruction, got %q", err)
				}
			}
		})
	}
}
