package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kenfold/kenfold/internal/memory"
)

// Code reference states (see migrations/00005_code_refs.sql).
const (
	RefPending    = "pending"
	RefCurrent    = "current"
	RefChanged    = "changed"
	RefMissing    = "missing"
	RefUnresolved = "unresolved"
)

// RefTarget is a file and/or symbol a memory mentions.
type RefTarget struct {
	Path   string
	Symbol string
}

// CodeRef is a stored reference with its verification state.
type CodeRef struct {
	Path          string
	Symbol        string
	State         string
	ResolvedPath  *string
	AnchorCommit  *string
	CheckedCommit *string
	CheckedAt     *time.Time
}

// Stale reports whether the reference suggests the memory is outdated: the
// file or symbol is gone, or the symbol's definition changed. A changed file
// alone is not stale; files change all the time.
func (r CodeRef) Stale() bool {
	return r.State == RefMissing || (r.State == RefChanged && r.Symbol != "")
}

// SyncTarget is a reference as clients check it; references that share a
// target and anchor are checked once.
type SyncTarget struct {
	Path         string
	Symbol       string
	AnchorCommit string // "" when not anchored yet
	AnchorHash   string
}

// RefCheck is a client's report for one SyncTarget at a commit.
type RefCheck struct {
	Path         string
	Symbol       string
	AnchorCommit string // the anchor the result applies to ("" = anchor now)
	Found        bool
	Hash         string
	ResolvedPath string
}

// RefCheckSummary counts the references ApplyRefChecks updated.
type RefCheckSummary struct {
	Updated   int            // reference rows updated
	Anchored  int            // pending references anchored (now current)
	Unmatched int            // results that matched no reference
	States    map[string]int // updated rows by resulting state
}

const (
	maxRefsPerMemory = 16
	maxRefField      = 300
	maxRefChecks     = 1000
	maxSyncTargets   = 500
)

func refScope(scope string) bool {
	return strings.HasPrefix(scope, "project:") || strings.HasPrefix(scope, "repo:")
}

// AddRefs records the code references of a memory. References that exist
// already are left as they are; scopes outside a project are ignored.
func (s *Store) AddRefs(ctx context.Context, id, scope string, refs []RefTarget) error {
	if !ValidID(id) {
		return ErrInvalidID
	}
	return addRefs(ctx, s.pool, id, scope, refs)
}

// execer is a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func addRefs(ctx context.Context, db execer, id, scope string, refs []RefTarget) error {
	if !refScope(scope) || len(refs) == 0 {
		return nil
	}
	var paths, symbols []string
	for _, r := range refs {
		if len(paths) == maxRefsPerMemory {
			break
		}
		if (r.Path == "" && r.Symbol == "") || len(r.Path) > maxRefField || len(r.Symbol) > maxRefField {
			continue
		}
		paths, symbols = append(paths, r.Path), append(symbols, r.Symbol)
	}
	if len(paths) == 0 {
		return nil
	}
	_, err := db.Exec(ctx, `
		INSERT INTO memory_ref (memory_id, scope, path, symbol)
		SELECT $1, $2, p, s FROM unnest($3::text[], $4::text[]) AS t(p, s)
		ON CONFLICT DO NOTHING`, id, scope, paths, symbols)
	if err != nil {
		return fmt.Errorf("add refs: %w", err)
	}
	return nil
}

// SyncTargets returns the distinct references of active and proposed
// memories in scope, for a client to check: never-checked ones first, then
// the least recently checked, so a client that runs out of time before the
// end makes progress over successive syncs.
func (s *Store) SyncTargets(ctx context.Context, scope string) ([]SyncTarget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.path, r.symbol, coalesce(r.anchor_commit, ''), coalesce(r.anchor_hash, '')
		FROM memory_ref r JOIN memory m ON m.id = r.memory_id
		WHERE r.scope = $1 AND m.status IN ('active', 'proposed')
		GROUP BY r.path, r.symbol, r.anchor_commit, r.anchor_hash
		ORDER BY min(r.checked_at) ASC NULLS FIRST, r.path, r.symbol
		LIMIT $2`, scope, maxSyncTargets)
	if err != nil {
		return nil, fmt.Errorf("sync targets: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SyncTarget, error) {
		var t SyncTarget
		return t, r.Scan(&t.Path, &t.Symbol, &t.AnchorCommit, &t.AnchorHash)
	})
}

// ErrTooManyChecks is returned for oversized check reports.
var ErrTooManyChecks = errors.New("too many results in one report")

// ApplyRefChecks records a client's check of scope's references at commit.
// A result without an anchor commit anchors the pending (and unresolved)
// references to its target: found ones become current, others unresolved. A
// result for an anchor compares hashes: equal is current, different is
// changed, not found is missing.
func (s *Store) ApplyRefChecks(ctx context.Context, scope, commit string, checks []RefCheck) (RefCheckSummary, error) {
	sum := RefCheckSummary{States: map[string]int{}}
	if len(checks) > maxRefChecks {
		return sum, ErrTooManyChecks
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, c := range checks {
			var rows pgx.Rows
			var err error
			if c.AnchorCommit == "" {
				rows, err = tx.Query(ctx, `
					UPDATE memory_ref SET
					    state          = CASE WHEN $4 THEN 'current' ELSE 'unresolved' END,
					    anchor_hash    = CASE WHEN $4 THEN $5 END,
					    anchor_commit  = CASE WHEN $4 THEN $6 END,
					    resolved_path  = NULLIF($7, ''),
					    checked_commit = $6, checked_at = now()
					WHERE scope = $1 AND path = $2 AND symbol = $3 AND anchor_hash IS NULL
					RETURNING state`, scope, c.Path, c.Symbol, c.Found, c.Hash, commit, c.ResolvedPath)
			} else {
				rows, err = tx.Query(ctx, `
					UPDATE memory_ref SET
					    state          = CASE WHEN NOT $4 THEN 'missing' WHEN anchor_hash = $5 THEN 'current' ELSE 'changed' END,
					    resolved_path  = coalesce(NULLIF($7, ''), resolved_path),
					    checked_commit = $6, checked_at = now()
					WHERE scope = $1 AND path = $2 AND symbol = $3 AND anchor_commit = $8 AND anchor_hash IS NOT NULL
					RETURNING state`, scope, c.Path, c.Symbol, c.Found, c.Hash, commit, c.ResolvedPath, c.AnchorCommit)
			}
			if err != nil {
				return err
			}
			states, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			if len(states) == 0 {
				sum.Unmatched++
			}
			for _, st := range states {
				sum.States[st]++
				sum.Updated++
				if c.AnchorCommit == "" && st == RefCurrent {
					sum.Anchored++
				}
			}
		}
		return nil
	})
	if err != nil {
		return RefCheckSummary{}, fmt.Errorf("apply ref checks: %w", err)
	}
	return sum, nil
}

// Refs returns the code references of the given memories, without
// unresolved ones (they point at nothing the client could find).
func (s *Store) Refs(ctx context.Context, ids []string) (map[string][]CodeRef, error) {
	out := map[string][]CodeRef{}
	if len(ids) == 0 {
		return out, nil
	}
	for _, id := range ids {
		if !ValidID(id) {
			return nil, ErrInvalidID
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT memory_id::text, path, symbol, state, resolved_path, anchor_commit, checked_commit, checked_at
		FROM memory_ref
		WHERE memory_id = ANY($1::uuid[]) AND state <> 'unresolved'
		ORDER BY memory_id, created_at, path, symbol`, ids)
	if err != nil {
		return nil, fmt.Errorf("refs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r CodeRef
		if err := rows.Scan(&id, &r.Path, &r.Symbol, &r.State, &r.ResolvedPath, &r.AnchorCommit, &r.CheckedCommit, &r.CheckedAt); err != nil {
			return nil, err
		}
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}

// RefCounts returns the number of references per state in scope ("" = all
// scopes), for active and proposed memories.
func (s *Store) RefCounts(ctx context.Context, scope string) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.state, count(*) FROM memory_ref r JOIN memory m ON m.id = r.memory_id
		WHERE ($1 = '' OR r.scope = $1) AND m.status IN ('active', 'proposed')
		GROUP BY r.state`, scope)
	if err != nil {
		return nil, fmt.Errorf("ref counts: %w", err)
	}
	out := map[string]int{}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// BackfillRefs records references for active and proposed project memories
// that have none, using extract to find them. It returns how many memories
// gained references. Memories that mention no code are re-read on every
// call; that is cheap next to the rest of startup.
func (s *Store) BackfillRefs(ctx context.Context, types []memory.Type, extract func(content string) []RefTarget) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id::text, m.scope, m.content FROM memory m
		WHERE m.status IN ('active', 'proposed') AND m.type = ANY($1)
		  AND (m.scope LIKE 'project:%' OR m.scope LIKE 'repo:%')
		  AND NOT EXISTS (SELECT 1 FROM memory_ref r WHERE r.memory_id = m.id)`, stringsOf(types))
	if err != nil {
		return 0, fmt.Errorf("backfill refs: %w", err)
	}
	type row struct{ id, scope, content string }
	todo, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.scope, &x.content)
	})
	if err != nil {
		return 0, fmt.Errorf("backfill refs: %w", err)
	}
	n := 0
	for _, x := range todo {
		refs := extract(x.content)
		if len(refs) == 0 {
			continue
		}
		if err := addRefs(ctx, s.pool, x.id, x.scope, refs); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
