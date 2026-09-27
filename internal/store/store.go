// Package store is Kenfold's persistence layer over the memory table. It owns
// SQL and row mapping; callers pass already-validated domain values.
//
// Phase 1 scope: create (with supersede), read by id, and soft-delete. Embeddings
// are left NULL for now; vector search lands with the embedding pipeline.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/memory"
)

// ErrNotFound is returned when a memory id does not exist.
var ErrNotFound = errors.New("memory not found")

// Store persists memories in PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool. The pool is owned by the caller.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Memory is a full row of the memory table. Fields that are optional in the
// schema are pointers so NULL round-trips faithfully.
type Memory struct {
	ID            string
	Type          memory.Type
	Scope         string
	Content       string
	Attrs         map[string]any
	SourceAgent   string
	SourceSession *string
	EvidenceURI   *string
	Trust         memory.Trust
	Confidence    float64
	Status        memory.Status
	Supersedes    *string
	ValidFrom     *time.Time
	ValidTo       *time.Time
	ExpiresAt     *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CreateParams is the input for Create. The caller is responsible for having
// validated Type and resolved Scope; the database still enforces its own
// constraints as a backstop.
type CreateParams struct {
	Type          memory.Type
	Scope         string
	Content       string
	Attrs         map[string]any
	SourceAgent   string
	SourceSession *string
	EvidenceURI   *string
	Trust         memory.Trust
	Confidence    float64
	Status        memory.Status
	Supersedes    *string
	ExpiresAt     *time.Time
}

// Create inserts a new memory. When Supersedes is set, the referenced memory is
// marked superseded in the same transaction so a contradiction is never left
// active alongside its replacement.
func (s *Store) Create(ctx context.Context, p CreateParams) (Memory, error) {
	attrs := p.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}

	var m Memory
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if p.Supersedes != nil {
			ct, err := tx.Exec(ctx, `
				UPDATE memory SET status = 'superseded'
				WHERE id = $1 AND status = 'active'`, *p.Supersedes)
			if err != nil {
				return fmt.Errorf("supersede %s: %w", *p.Supersedes, err)
			}
			if ct.RowsAffected() == 0 {
				return fmt.Errorf("supersede %s: %w (or not active)", *p.Supersedes, ErrNotFound)
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO memory
			  (type, scope, content, attrs, source_agent, source_session,
			   evidence_uri, trust, confidence, status, supersedes, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING `+columns,
			p.Type, p.Scope, p.Content, attrs, p.SourceAgent, p.SourceSession,
			p.EvidenceURI, p.Trust, p.Confidence, p.Status, p.Supersedes, p.ExpiresAt)
		return scan(row, &m)
	})
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// Get returns a memory by id regardless of status. It returns ErrNotFound if the
// id does not exist.
func (s *Store) Get(ctx context.Context, id string) (Memory, error) {
	var m Memory
	err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM memory WHERE id = $1`, id), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// SoftDelete marks a memory deleted and records reason in attrs. It is
// idempotent: deleting an already-deleted memory returns its current state
// without error. It returns ErrNotFound if the id does not exist.
func (s *Store) SoftDelete(ctx context.Context, id, reason string) (Memory, error) {
	var m Memory
	q := `
		UPDATE memory
		SET status = 'deleted',
		    attrs = CASE WHEN $2 <> '' THEN attrs || jsonb_build_object('forget_reason', $2::text)
		                 ELSE attrs END
		WHERE id = $1
		RETURNING ` + columns
	err := scan(s.pool.QueryRow(ctx, q, id, reason), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// ---- reads ----

// Scored pairs a memory with a relevance score for search results.
type Scored struct {
	Memory
	Score float64
}

// SearchParams controls a full-text search over active memories.
type SearchParams struct {
	Query string        // natural-language query; matched with plainto_tsquery('simple')
	Scope string        // project/repo scope to include in addition to user-wide; empty = user-wide only
	Types []memory.Type // restrict to these types; empty = all types
	Limit int           // max rows; <= 0 defaults to 10
}

// Search runs a lexical full-text search over active, unexpired memories. It
// always includes user-wide ('user') memories, plus Scope when set. Results are
// ranked by ts_rank (highest first), then recency. Vector/hybrid search is a
// later phase; this is the Phase 1 lexical baseline.
func (s *Store) Search(ctx context.Context, p SearchParams) ([]Scored, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 10
	}
	scopes := []string{"user"}
	if p.Scope != "" && p.Scope != "user" {
		scopes = append(scopes, p.Scope)
	}

	// Natural-language recall: OR the query lexemes rather than AND-ing them,
	// so a question like "how does recall search work" still matches documents
	// containing some of the terms, ranked by ts_rank. We derive the OR query
	// from plainto_tsquery's own lexeme parsing (safe against injection) by
	// swapping its '&' operators for '|'.
	// $1 query, $2 scopes, $3 types (nil = no filter), $4 limit.
	rows, err := s.pool.Query(ctx, `
		WITH q AS (
		    SELECT to_tsquery('simple',
		        replace(plainto_tsquery('simple', $1)::text, '&', '|')) AS tsq
		)
		SELECT `+columns+`,
		       ts_rank(content_tsv, q.tsq) AS score
		FROM memory, q
		WHERE status = 'active'
		  AND (expires_at IS NULL OR expires_at > now())
		  AND scope = ANY($2)
		  AND ($3::text[] IS NULL OR type = ANY($3))
		  AND q.tsq @@ content_tsv
		ORDER BY score DESC, created_at DESC
		LIMIT $4`,
		p.Query, scopes, typeStrings(p.Types), limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	return scanScored(rows)
}

// ListByScopeTypes returns active, unexpired memories in the given scopes and
// types, newest first. It is the injection path for get_context (preferences,
// project knowledge). Empty types means all types.
func (s *Store) ListByScopeTypes(ctx context.Context, scopes []string, types []memory.Type, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+columns+`
		FROM memory
		WHERE status = 'active'
		  AND (expires_at IS NULL OR expires_at > now())
		  AND scope = ANY($1)
		  AND ($2::text[] IS NULL OR type = ANY($2))
		ORDER BY created_at DESC
		LIMIT $3`,
		scopes, typeStrings(types), limit)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()
	return scanMany(rows)
}

// LatestHandoff returns the most recent active, unexpired handoff (a temporary
// memory) for the given scope, or ErrNotFound if there is none.
func (s *Store) LatestHandoff(ctx context.Context, scope string) (Memory, error) {
	var m Memory
	err := scan(s.pool.QueryRow(ctx, `
		SELECT `+columns+`
		FROM memory
		WHERE status = 'active'
		  AND type = 'temporary'
		  AND attrs->>'kind' = 'handoff'
		  AND scope = $1
		  AND (expires_at IS NULL OR expires_at > now())
		ORDER BY created_at DESC
		LIMIT 1`, scope), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// typeStrings converts a nil/empty slice to nil (no filter) or a []string for
// the SQL text[] parameter.
func typeStrings(types []memory.Type) []string {
	if len(types) == 0 {
		return nil
	}
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = string(t)
	}
	return out
}

// columns is the canonical select list; scan must match it exactly.
const columns = `id, type, scope, content, attrs, source_agent, source_session,
	evidence_uri, trust, confidence, status, supersedes,
	valid_from, valid_to, expires_at, created_at, updated_at`

type row interface {
	Scan(dest ...any) error
}

func scan(r row, m *Memory) error {
	return r.Scan(
		&m.ID, &m.Type, &m.Scope, &m.Content, &m.Attrs, &m.SourceAgent, &m.SourceSession,
		&m.EvidenceURI, &m.Trust, &m.Confidence, &m.Status, &m.Supersedes,
		&m.ValidFrom, &m.ValidTo, &m.ExpiresAt, &m.CreatedAt, &m.UpdatedAt,
	)
}

// rows is the subset of pgx.Rows used for scanning result sets.
type rowsIface interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanMany(rs rowsIface) ([]Memory, error) {
	var out []Memory
	for rs.Next() {
		var m Memory
		if err := scan(rs, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rs.Err()
}

func scanScored(rs rowsIface) ([]Scored, error) {
	var out []Scored
	for rs.Next() {
		var s Scored
		if err := rs.Scan(
			&s.ID, &s.Type, &s.Scope, &s.Content, &s.Attrs, &s.SourceAgent, &s.SourceSession,
			&s.EvidenceURI, &s.Trust, &s.Confidence, &s.Status, &s.Supersedes,
			&s.ValidFrom, &s.ValidTo, &s.ExpiresAt, &s.CreatedAt, &s.UpdatedAt,
			&s.Score,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rs.Err()
}
