package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kenfold/kenfold/internal/memory"
)

// Extraction job states.
const (
	ExtractRunning = "running"
	ExtractDone    = "done"
	ExtractFailed  = "failed"
)

// ExtractionJob is a claimed session summary.
type ExtractionJob struct {
	Source   Memory
	Attempts int // including this one
}

// ClaimExtraction claims the oldest active session summary that has not been
// extracted: never claimed, a failed attempt due for retry, or a running claim
// whose lease expired. Only summaries created before notBefore are claimed, so
// a session that is still ending (and may replace its summary) is left alone.
// It returns ErrNotFound when there is nothing to do.
func (s *Store) ClaimExtraction(ctx context.Context, notBefore time.Time, lease time.Duration, model string) (ExtractionJob, error) {
	var job ExtractionJob
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `
			SELECT m.id FROM memory m
			LEFT JOIN extraction e ON e.source_id = m.id
			WHERE m.type = 'episodic' AND m.status = 'active' AND m.created_at < $1
			  AND (e.source_id IS NULL
			       OR (e.status = 'failed' AND e.next_attempt_at IS NOT NULL AND e.next_attempt_at <= now())
			       OR (e.status = 'running' AND e.locked_until < now()))
			ORDER BY m.created_at
			LIMIT 1
			FOR UPDATE OF m SKIP LOCKED`, notBefore).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO extraction (source_id, status, attempts, model, locked_until)
			VALUES ($1, 'running', 1, $2, now() + $3::interval)
			ON CONFLICT (source_id) DO UPDATE
			  SET status = 'running', attempts = extraction.attempts + 1, model = EXCLUDED.model,
			      locked_until = EXCLUDED.locked_until, next_attempt_at = NULL, error = NULL
			RETURNING attempts`, id, model, lease.String()).Scan(&job.Attempts); err != nil {
			return err
		}
		return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM memory WHERE id = $1`, id), &job.Source)
	})
	if err != nil {
		return ExtractionJob{}, err
	}
	return job, nil
}

// FinishExtraction records a successful extraction.
func (s *Store) FinishExtraction(ctx context.Context, sourceID string, candidates, stored int) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE extraction SET status = 'done', candidates = $2, stored = $3, locked_until = NULL, error = NULL
		WHERE source_id = $1`, sourceID, candidates, stored)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FailExtraction records a failed attempt. retryAt is when to try again; nil
// means give up.
func (s *Store) FailExtraction(ctx context.Context, sourceID, msg string, retryAt *time.Time) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	ct, err := s.pool.Exec(ctx, `
		UPDATE extraction SET status = 'failed', error = $2, next_attempt_at = $3, locked_until = NULL
		WHERE source_id = $1`, sourceID, msg, retryAt)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ExtractionStats counts summaries by extraction state.
type ExtractionStats struct {
	Pending, Running, Done, Failed, Stored int
}

// ExtractionStatus reports progress over active session summaries.
func (s *Store) ExtractionStatus(ctx context.Context) (ExtractionStats, error) {
	var st ExtractionStats
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE e.source_id IS NULL),
		       count(*) FILTER (WHERE e.status = 'running'),
		       count(*) FILTER (WHERE e.status = 'done'),
		       count(*) FILTER (WHERE e.status = 'failed'),
		       coalesce(sum(e.stored), 0)
		FROM memory m LEFT JOIN extraction e ON e.source_id = m.id
		WHERE m.type = 'episodic' AND m.status = 'active'`).Scan(&st.Pending, &st.Running, &st.Done, &st.Failed, &st.Stored)
	return st, err
}

// SeenContent returns a memory in scope with the same content key (see
// ContentKey), in any status, or ErrNotFound. The extractor uses it so that a
// fact the user rejected (deleted) or replaced (superseded) is not proposed
// again from a later session.
func (s *Store) SeenContent(ctx context.Context, scope, content string) (Memory, error) {
	var m Memory
	err := scan(s.pool.QueryRow(ctx, `
		SELECT `+columns+` FROM memory
		WHERE scope = $1
		  AND content_key = lower(regexp_replace(regexp_replace(btrim($2::text), '\s+', ' ', 'g'), '[.!。]+$', ''))
		ORDER BY created_at DESC LIMIT 1`, scope, content), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// AddEdge links src to dst (e.g. an extracted memory 'derived_from' its
// session summary). Adding an existing edge is a no-op.
func (s *Store) AddEdge(ctx context.Context, src, relation, dst, agent string) error {
	if !ValidID(src) || !ValidID(dst) {
		return ErrInvalidID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO memory_edge (src, dst, relation, source_agent) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, src, dst, relation, agent)
	if err != nil {
		return fmt.Errorf("add edge: %w", err)
	}
	return nil
}

// DerivedFrom returns the memories src was derived from (relation 'derived_from').
func (s *Store) DerivedFrom(ctx context.Context, src string) ([]Memory, error) {
	if !ValidID(src) {
		return nil, ErrInvalidID
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+mColumns+` FROM memory_edge e JOIN memory m ON m.id = e.dst
		WHERE e.src = $1 AND e.relation = 'derived_from'
		ORDER BY m.created_at`, src)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Memory, error) {
		var m Memory
		return m, scan(r, &m)
	})
}

// SimilarAny is like Similar but also considers proposed memories, so the
// extractor does not propose a near-duplicate of a memory awaiting review.
func (s *Store) SimilarAny(ctx context.Context, p SimilarParams) ([]Scored, error) {
	p.includeProposed = true
	return s.Similar(ctx, p)
}

// ApproveReplacing approves proposed memory id and retires target, an active
// memory in the same scope that id replaces (e.g. one it contradicts). It is
// the review action for "the new memory is right, the old one is outdated".
func (s *Store) ApproveReplacing(ctx context.Context, id, target string) (Memory, error) {
	if !ValidID(id) || !ValidID(target) {
		return Memory{}, ErrInvalidID
	}
	if id == target {
		return Memory{}, errors.New("a memory cannot replace itself")
	}
	var m Memory
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status memory.Status
		var scope string
		err := tx.QueryRow(ctx, `SELECT status, scope FROM memory WHERE id = $1 FOR UPDATE`, id).Scan(&status, &scope)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != memory.StatusProposed {
			return fmt.Errorf("%w (status is %s)", ErrNotProposed, status)
		}
		var tStatus memory.Status
		var tScope string
		err = tx.QueryRow(ctx, `SELECT status, scope FROM memory WHERE id = $1 FOR UPDATE`, target).Scan(&tStatus, &tScope)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("replaced memory: %w", ErrNotFound)
		}
		if err != nil {
			return err
		}
		if tStatus != memory.StatusActive {
			return fmt.Errorf("replaced memory: %w", ErrNotActive)
		}
		if tScope != scope {
			return fmt.Errorf("replaced memory is in scope %q, not %q", tScope, scope)
		}
		if _, err := tx.Exec(ctx, `UPDATE memory SET status = 'superseded' WHERE id = $1`, target); err != nil {
			return err
		}
		return scan(tx.QueryRow(ctx, `
			UPDATE memory SET status = 'active', trust = 'user', supersedes = $2
			WHERE id = $1 RETURNING `+columns, id, target), &m)
	})
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// Reject soft-deletes a proposed memory on the user's behalf and records the
// reason. Only proposed memories can be rejected (ErrNotProposed otherwise).
func (s *Store) Reject(ctx context.Context, id, reason, agent string) (Memory, error) {
	m, err := s.Get(ctx, id)
	if err != nil {
		return Memory{}, err
	}
	if m.Status != memory.StatusProposed {
		return Memory{}, fmt.Errorf("%w (status is %s)", ErrNotProposed, m.Status)
	}
	return s.SoftDelete(ctx, id, reason, agent)
}
