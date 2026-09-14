package store

import "context"

// RecentCwd is one working directory managed runs have used, with the most
// recent time any run in it was updated or started (unix ms).
type RecentCwd struct {
	Cwd        string
	LastUsedAt int64
}

// RecentManagedCwds returns the distinct non-empty cwds of managed runs, most
// recently used first. Read-only.
func (s *Store) RecentManagedCwds(ctx context.Context, limit int) ([]RecentCwd, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT cwd, MAX(MAX(updated_at, started_at)) AS last_used
		FROM managed_sessions
		WHERE cwd != ''
		GROUP BY cwd
		ORDER BY last_used DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RecentCwd{}
	for rows.Next() {
		var c RecentCwd
		if err := rows.Scan(&c.Cwd, &c.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
