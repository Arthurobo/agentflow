package ingest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/arthurobo/agentflow/internal/store"
)

// TestEventKeyLayout guards the keying rule regression.
func TestEventKeyLayout(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	b := &store.Batch{
		Events: []store.Incoming{
			{SessionID: "s", Seq: 1, Event: "session_state", Content: "normal"},
			{SessionID: "s", Seq: 2, Event: "session_state", Content: "default"},
		},
		Session: &store.SessionMeta{SessionID: "s", CWD: "/x", Project: "x", FilePath: "/x.jsonl"},
	}
	if err := st.InsertBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	c, _ := st.Counts(ctx)
	if c.Events != 2 {
		t.Fatalf("two distinct snapshot records must both persist, got %d", c.Events)
	}
}
