package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/kenfold/kenfold/internal/memory"
)

// History returns the versions of the memory that id belongs to, oldest
// first: the memories it replaced (following supersedes backward) and the
// ones that replaced it (forward), including id itself.
func (s *Store) History(ctx context.Context, id string) ([]Memory, error) {
	if !ValidID(id) {
		return nil, ErrInvalidID
	}
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE older AS (
		    SELECT id, supersedes, 0 AS depth FROM memory WHERE id = $1
		    UNION ALL
		    SELECT m.id, m.supersedes, o.depth - 1 FROM memory m JOIN older o ON m.id = o.supersedes WHERE o.depth > -50
		),
		newer AS (
		    SELECT id, 0 AS depth FROM memory WHERE id = $1
		    UNION ALL
		    SELECT m.id, n.depth + 1 FROM memory m JOIN newer n ON m.supersedes = n.id WHERE n.depth < 50
		),
		chain AS (SELECT id, depth FROM older UNION SELECT id, depth FROM newer)
		SELECT `+mColumns+` FROM chain c JOIN memory m ON m.id = c.id
		ORDER BY c.depth, m.created_at`, id)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Memory, error) {
		var m Memory
		return m, scan(r, &m)
	})
}

// Link is an edge to or from a memory, with the memory at the other end.
type Link struct {
	Relation string
	Outgoing bool // true: this memory -> Memory; false: Memory -> this memory
	Memory   Memory
}

// Links returns the edges of memory id in both directions (any status).
func (s *Store) Links(ctx context.Context, id string) ([]Link, error) {
	if !ValidID(id) {
		return nil, ErrInvalidID
	}
	rows, err := s.pool.Query(ctx, `
		SELECT e.relation, true, `+mColumns+` FROM memory_edge e JOIN memory m ON m.id = e.dst WHERE e.src = $1
		UNION ALL
		SELECT e.relation, false, `+mColumns+` FROM memory_edge e JOIN memory m ON m.id = e.src WHERE e.dst = $1
		ORDER BY 1, 2 DESC
		LIMIT 200`, id)
	if err != nil {
		return nil, fmt.Errorf("links: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Link, error) {
		var l Link
		return l, r.Scan(append([]any{&l.Relation, &l.Outgoing}, fields(&l.Memory)...)...)
	})
}

// Stats summarizes the store for the dashboard's overview.
type Stats struct {
	ByStatus   map[memory.Status]int
	ActiveType map[memory.Type]int // active, unexpired memories by type
	Stale      int                 // active memories with a stale code reference
	Scopes     []ScopeCount        // scopes with active or proposed memories
}

// ScopeCount is the number of active and proposed memories in a scope.
type ScopeCount struct {
	Scope    string
	Active   int
	Proposed int
}

// Stats counts memories by status, type, staleness, and scope.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{ByStatus: map[memory.Status]int{}, ActiveType: map[memory.Type]int{}}
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM memory GROUP BY status`)
	if err != nil {
		return st, fmt.Errorf("stats: %w", err)
	}
	if err := collectCounts(rows, func(k string, n int) { st.ByStatus[memory.Status(k)] = n }); err != nil {
		return st, err
	}
	rows, err = s.pool.Query(ctx, `SELECT type, count(*) FROM memory WHERE status = 'active' AND (expires_at IS NULL OR expires_at > now()) GROUP BY type`)
	if err != nil {
		return st, fmt.Errorf("stats: %w", err)
	}
	if err := collectCounts(rows, func(k string, n int) { st.ActiveType[memory.Type(k)] = n }); err != nil {
		return st, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory m WHERE m.status = 'active' AND EXISTS (
		    SELECT 1 FROM memory_ref r WHERE r.memory_id = m.id AND (r.state = 'missing' OR (r.state = 'changed' AND r.symbol <> '')))`).
		Scan(&st.Stale); err != nil {
		return st, fmt.Errorf("stats: %w", err)
	}
	rows, err = s.pool.Query(ctx, `
		SELECT scope, count(*) FILTER (WHERE status = 'active'), count(*) FILTER (WHERE status = 'proposed')
		FROM memory WHERE status IN ('active', 'proposed') AND (expires_at IS NULL OR expires_at > now())
		GROUP BY scope ORDER BY scope = 'user' DESC, scope LIMIT 500`)
	if err != nil {
		return st, fmt.Errorf("stats: %w", err)
	}
	st.Scopes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ScopeCount, error) {
		var c ScopeCount
		return c, r.Scan(&c.Scope, &c.Active, &c.Proposed)
	})
	return st, err
}

func collectCounts(rows pgx.Rows, put func(string, int)) error {
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		put(k, n)
	}
	return rows.Err()
}

// CountStatus counts unexpired memories with status st.
func (s *Store) CountStatus(ctx context.Context, st memory.Status) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM memory WHERE status = $1 AND (expires_at IS NULL OR expires_at > now())`, st).Scan(&n)
	return n, err
}
