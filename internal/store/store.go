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
