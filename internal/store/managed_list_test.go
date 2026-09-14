package store

import (
	"context"
	"testing"
	"time"
)

// The Sessions list used to read one table: the corpus, which holds
// transcripts something has ingested. Claude sessions reached it through a
// tailer of ~/.claude/projects. Nothing tails an OpenCode run, and an
// Agent-Flow-spawned OpenCode session is not written where the one-time
// OpenCode backfill reads either, so a running OpenCode session appeared
// nowhere at all. These tests pin the second source.

func seedCorpus(t *testing.T, s *Store, sessionID, project, prompt string, ts int64) {
	t.Helper()
	if err := s.InsertBatch(context.Background(), &Batch{
		Events: []Incoming{{
			SessionID: sessionID, Seq: 1, Event: "user_message",
			Content: prompt, TS: ts, Source: "tailer",
		}},
		Session: &SessionMeta{
			SessionID: sessionID, Project: project, CWD: "/w",
			FilePath: "/" + sessionID + ".jsonl",
		},
	}); err != nil {
		t.Fatalf("seed corpus %s: %v", sessionID, err)
	}
}

func seedManaged(t *testing.T, s *Store, m *ManagedSession) {
	t.Helper()
	if err := s.UpsertManagedSession(context.Background(), m); err != nil {
		t.Fatalf("seed managed %s: %v", m.ID, err)
	}
}

// Tapping a row the list just showed must not 404.
func TestGetSessionResolvesAManagedOnlyRun(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Now().UnixMilli()

	seedManaged(t, s, &ManagedSession{
		ID: "run-oc", SessionID: "ses_live", Kind: "tty", State: "running",
		Engine: "opencode", Title: "Vlog", Project: "myapp", CWD: "/code/af",
		StartedAt: now - 1000, UpdatedAt: now,
	})
	seedManaged(t, s, &ManagedSession{
		ID: "run-unbound", SessionID: "", Kind: "tty", State: "starting",
		Engine: "opencode", Project: "af", UpdatedAt: now,
	})

	// A bound run lists under its session id, so that is what gets tapped.
	got, err := s.GetSession(ctx, "ses_live")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("a managed-only run must resolve, not 404")
	}
	if got.Title != "Vlog" || got.Engine != "opencode" || !got.Managed || !got.IsActive {
		t.Errorf("resolved row is wrong: %+v", got)
	}
	if got.Cwd != "/code/af" {
		t.Errorf("cwd = %q, want the run's own", got.Cwd)
	}

	// An unbound run lists under its run id, so that is what gets tapped.
	if got, err = s.GetSession(ctx, "run-unbound"); err != nil || got == nil {
		t.Fatalf("unbound run must resolve by run id: %+v %v", got, err)
	}
	// The title falls back to something a person recognises.
	if got.Title != "af" {
		t.Errorf("fallback title = %q, want the project", got.Title)
	}

	// An id nobody has ever heard of is still a 404.
	if got, err = s.GetSession(ctx, "no-such-session"); err != nil || got != nil {
		t.Fatalf("unknown id must stay nil: %+v %v", got, err)
	}
}

// A corpus row still wins the lookup when both exist.
func TestGetSessionPrefersTheCorpusRow(t *testing.T) {
	s := mustOpen(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	now := time.Now().UnixMilli()

	seedCorpus(t, s, "ses-both", "webapp", "fix the export", now)
	seedManaged(t, s, &ManagedSession{
		ID: "run-both", SessionID: "ses-both", Kind: "tty", State: "running",
		Engine: "claude", Title: "Named At Spawn", Project: "webapp", UpdatedAt: now,
	})

	got, err := s.GetSession(ctx, "ses-both")
	if err != nil || got == nil {
		t.Fatalf("get: %+v %v", got, err)
	}
	if got.Managed {
		t.Error("the corpus row is the better row")
	}
	if got.EventCount == 0 {
		t.Error("the corpus row carries the event counts")
	}
}
