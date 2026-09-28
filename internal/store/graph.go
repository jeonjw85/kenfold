package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/kenfold/kenfold/internal/memory"
)

// Relations that Neighbors reports besides stored edge relations.
const (
	// RelSibling links two memories derived from the same source (e.g. two
	// facts extracted from one session summary).
	RelSibling = "sibling"
)

// Neighbor is a memory adjacent to one of the seeds of Neighbors.
type Neighbor struct {
	Memory
	Via      string // the seed it is adjacent to
	Relation string // edge relation (either direction), or RelSibling
}

// NeighborParams controls Neighbors.
type NeighborParams struct {
	Seeds  []string      // memory ids
	Scopes []string      // only neighbors in these scopes
	Types  []memory.Type // empty = all
	Limit  int           // default 50, max 200
}

// Neighbors returns active, unexpired memories one edge away from the seeds
// (in either direction), plus memories derived from the same source as a
// seed. Seeds themselves are not returned. A memory adjacent to several
// seeds is returned once per seed.
func (s *Store) Neighbors(ctx context.Context, p NeighborParams) ([]Neighbor, error) {
	if len(p.Seeds) == 0 {
		return nil, nil
	}
	for _, id := range p.Seeds {
		if !ValidID(id) {
			return nil, ErrInvalidID
		}
	}
	limit := clamp(p.Limit, 50, 200)
	rows, err := s.pool.Query(ctx, `
		WITH seeds AS (SELECT unnest($1::uuid[]) AS id),
		adj AS (
		    SELECT e.dst AS id, e.src AS via, e.relation FROM memory_edge e JOIN seeds s ON e.src = s.id
		    UNION
		    SELECT e.src, e.dst, e.relation FROM memory_edge e JOIN seeds s ON e.dst = s.id
		    UNION
		    SELECT e2.src, e1.src, '`+RelSibling+`'
		    FROM memory_edge e1 JOIN seeds s ON e1.src = s.id
		    JOIN memory_edge e2 ON e2.dst = e1.dst AND e2.relation = e1.relation AND e2.src <> e1.src
		    WHERE e1.relation = 'derived_from'
		)
		SELECT `+mColumns+`, a.via::text, a.relation
		FROM adj a JOIN memory m ON m.id = a.id
		WHERE m.status = 'active'
		  AND (m.expires_at IS NULL OR m.expires_at > now())
		  AND m.scope = ANY($2)
		  AND ($3::text[] IS NULL OR m.type = ANY($3))
		  AND m.id <> ALL($1::uuid[])
		ORDER BY m.created_at DESC, m.id
		LIMIT $4`, p.Seeds, p.Scopes, stringsOf(p.Types), limit)
	if err != nil {
		return nil, fmt.Errorf("neighbors: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Neighbor, error) {
		var n Neighbor
		return n, r.Scan(append(fields(&n.Memory), &n.Via, &n.Relation)...)
	})
}
