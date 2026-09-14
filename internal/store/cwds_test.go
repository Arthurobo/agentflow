package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRecentManagedCwdsAreDistinctAndNewestFirst(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "cwds.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, m := range []*ManagedSession{
		{ID: "a", CWD: "/old", StartedAt: 100, UpdatedAt: 100, State: "finished", Kind: "tty"},
		{ID: "b", CWD: "/new", StartedAt: 300, UpdatedAt: 300, State: "finished", Kind: "tty"},
		{ID: "c", CWD: "/old", StartedAt: 200, UpdatedAt: 200, State: "finished", Kind: "tty"},
		{ID: "d", CWD: "", StartedAt: 900, UpdatedAt: 900, State: "finished", Kind: "tty"},
	} {
		if err := st.UpsertManagedSession(ctx, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}
	got, err := st.RecentManagedCwds(ctx, 50)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 || got[0].Cwd != "/new" || got[1].Cwd != "/old" {
		t.Fatalf("want [/new /old], got %+v", got)
	}
	if got[0].LastUsedAt < 300 || got[1].LastUsedAt < 200 {
		t.Fatalf("last used must be the newest per cwd: %+v", got)
	}
}
