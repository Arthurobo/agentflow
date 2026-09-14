package main

// make-resumable / undo-make-resumable — thin CLI over internal/resumable.
import (
	"fmt"
	"log/slog"
	"os"

	"github.com/arthurobo/agentflow/internal/resumable"
	"github.com/arthurobo/agentflow/internal/store"
)

func runMakeResumable(log *slog.Logger, cfg config, id string) {
	st, err := store.Open(cfg.dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentd:", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	rep, err := resumable.MakeResumable(st, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentd make-resumable:", err)
		os.Exit(1)
	}
	fmt.Printf("session %s\n  transcript: %s\n", rep.SessionID, rep.Transcript)
	fmt.Printf("  rewrote %d entrypoint sdk-* -> cli, %d promptSource sdk -> typed\n",
		rep.RewrittenEntry, rep.RewrittenPrompt)
	if rep.RewrittenEntry == 0 {
		fmt.Println("  (no sdk entrypoint records found - nothing changed)")
	}
	fmt.Printf("backup: %s\n", rep.Backup)
	fmt.Println("Next: run `claude -r` (or /resume) in the project - the session should now be listed.")
	fmt.Println("Resume still works by id/title regardless: claude -p --resume", rep.SessionID)
}

func runUndoMakeResumable(log *slog.Logger, cfg config, id string) {
	st, err := store.Open(cfg.dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentd:", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	path, err := resumable.Undo(st, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentd undo-make-resumable:", err)
		os.Exit(1)
	}
	fmt.Printf("restored from %s\n", path)
}
