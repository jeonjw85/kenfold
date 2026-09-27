// Package store is Kenfold's persistence layer over the memory table. It owns
// SQL and row mapping; callers pass already-validated domain values, and the
// database constraints act as a backstop.
package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/memory"
)

// EmbeddingDim is the fixed dimension of memory.embedding (see migrations).
const EmbeddingDim = 1024

var (
	// ErrNotFound is returned when a memory id does not exist.
	ErrNotFound = errors.New("memory not found")
	// ErrInvalidID is returned for ids that are not UUIDs, before touching the database.
	ErrInvalidID = errors.New("invalid memory id: expected a UUID")
	// ErrNotActive is returned when superseding a memory that is no longer active.
	ErrNotActive = errors.New("memory is not active")
	// ErrNotProposed is returned when approving a memory that is not awaiting review.
	ErrNotProposed = errors.New("memory is not proposed")
)

// Store persists memories in PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool. The pool is owned by the caller.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Memory is a row of the memory table (without the embedding vector itself).
// Optional columns are pointers so NULL round-trips faithfully.
type Memory struct {
	ID             string
	Type           memory.Type
	Scope          string
	Content        string
	Attrs          map[string]any
	SourceAgent    string
	SourceSession  *string
	EvidenceURI    *string
	Trust          memory.Trust
	Confidence     float64
	Status         memory.Status
	Supersedes     *string
	ValidFrom      *time.Time
	ValidTo        *time.Time
	ExpiresAt      *time.Time
	EmbeddingModel *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Scored pairs a memory with a relevance score in (0, 1]; higher is better.
type Scored struct {
	Memory
	Score float64
}

// ---- writes ----

// CreateParams is the input for Create.
type CreateParams struct {
	Type          memory.Type
	Scope         string
	Content       string
	Attrs         map[string]any
	SourceAgent   string
	SourceSession string // optional: the agent session that wrote the memory
	Trust         memory.Trust
	Confidence    float64
	Status        memory.Status
	Supersedes    *string
	ExpiresAt     *time.Time
	// Embedding is optional; when set, EmbeddingModel must name the model.
	Embedding      []float32
	EmbeddingModel string
}

// Create inserts a new memory.
//
// Supersedes must reference an active memory (ErrNotActive otherwise). If the new
// memory is active, the old one is marked superseded in the same transaction so a
// contradiction is never served alongside its replacement. If the new memory is
// proposed, the old one stays active until Approve.
func (s *Store) Create(ctx context.Context, p CreateParams) (Memory, error) {
	if p.Supersedes != nil && !ValidID(*p.Supersedes) {
		return Memory{}, ErrInvalidID
	}
	vec, err := vectorParam(p.Embedding)
	if err != nil {
		return Memory{}, err
	}
	var model *string
	if vec != nil {
		if p.EmbeddingModel == "" {
			return Memory{}, errors.New("embedding given without a model name")
		}
		model = &p.EmbeddingModel
	}
	attrs := p.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}

	var m Memory
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if p.Supersedes != nil {
			if err := lockActive(ctx, tx, *p.Supersedes); err != nil {
				return err
			}
			if p.Status == memory.StatusActive {
				if _, err := tx.Exec(ctx, `UPDATE memory SET status = 'superseded' WHERE id = $1`, *p.Supersedes); err != nil {
					return fmt.Errorf("supersede: %w", err)
				}
			}
		}
		return scan(tx.QueryRow(ctx, `
			INSERT INTO memory
			  (type, scope, content, attrs, source_agent, source_session, trust, confidence,
			   status, supersedes, expires_at, embedding, embedding_model)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11, $12::text::vector, $13)
			RETURNING `+columns,
			p.Type, p.Scope, p.Content, attrs, p.SourceAgent, p.SourceSession, p.Trust, p.Confidence,
			p.Status, p.Supersedes, p.ExpiresAt, vec, model), &m)
	})
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// lockActive row-locks id and fails unless it exists and is active.
func lockActive(ctx context.Context, tx pgx.Tx, id string) error {
	var status memory.Status
	err := tx.QueryRow(ctx, `SELECT status FROM memory WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != memory.StatusActive {
		return ErrNotActive
	}
	return nil
}

// ContentKey normalizes content the same way as the memory.content_key column
// (migrations/00003): trimmed, whitespace runs collapsed to one space,
// trailing '.', '!' or '。' removed, lowercased. Two memories with the same
// key state the same thing.
func ContentKey(content string) string {
	s := strings.Join(strings.FieldsFunc(content, unicode.IsSpace), " ")
	s = strings.TrimRight(s, ".!。")
	return strings.ToLower(s)
}

// FindDuplicate returns an active or proposed, unexpired memory in the same
// scope and type (any type when typ is "") whose content differs from content
// only in case, whitespace, or trailing punctuation (see content_key in
// migrations), or ErrNotFound. It lets remember be idempotent when agents
// store the same fact again.
func (s *Store) FindDuplicate(ctx context.Context, scope string, typ memory.Type, content string) (Memory, error) {
	var m Memory
	err := scan(s.pool.QueryRow(ctx, `
		SELECT `+columns+` FROM memory
		WHERE scope = $1 AND ($2 = '' OR type = $2)
		  AND content_key = lower(regexp_replace(regexp_replace(btrim($3::text), '\s+', ' ', 'g'), '[.!。]+$', ''))
		  AND status IN ('active', 'proposed')
		  AND (expires_at IS NULL OR expires_at > now())
		ORDER BY created_at DESC
		LIMIT 1`, scope, typ, content), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// Approve activates a proposed memory and marks it user-confirmed (trust=user):
// approval is the user's confirmation. If it supersedes another memory, that
// memory is retired in the same transaction. Approving an already active memory
// is a no-op; any other status returns ErrNotProposed.
func (s *Store) Approve(ctx context.Context, id string) (Memory, error) {
	if !ValidID(id) {
		return Memory{}, ErrInvalidID
	}
	var m Memory
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status memory.Status
		var supersedes *string
		err := tx.QueryRow(ctx, `SELECT status, supersedes FROM memory WHERE id = $1 FOR UPDATE`, id).Scan(&status, &supersedes)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch status {
		case memory.StatusActive:
			return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM memory WHERE id = $1`, id), &m)
		case memory.StatusProposed:
		default:
			return fmt.Errorf("%w (status is %s)", ErrNotProposed, status)
		}
		if supersedes != nil {
			// The old memory may already be gone (superseded or deleted meanwhile); that is fine.
			if _, err := tx.Exec(ctx, `UPDATE memory SET status = 'superseded' WHERE id = $1 AND status = 'active'`, *supersedes); err != nil {
				return fmt.Errorf("supersede: %w", err)
			}
		}
		return scan(tx.QueryRow(ctx, `UPDATE memory SET status = 'active', trust = 'user' WHERE id = $1 RETURNING `+columns, id), &m)
	})
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// SoftDelete marks a memory deleted and records who deleted it and why. It is
// idempotent: deleting an already-deleted memory returns it without error.
func (s *Store) SoftDelete(ctx context.Context, id, reason, agent string) (Memory, error) {
	if !ValidID(id) {
		return Memory{}, ErrInvalidID
	}
	var m Memory
	err := scan(s.pool.QueryRow(ctx, `
		UPDATE memory
		SET attrs = CASE
		        WHEN status = 'deleted' THEN attrs
		        ELSE attrs || jsonb_strip_nulls(jsonb_build_object(
		            'forgotten_by', $3::text,
		            'forget_reason', NULLIF($2::text, '')))
		    END,
		    status = 'deleted'
		WHERE id = $1
		RETURNING `+columns, id, reason, agent), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// ---- handoffs ----

// HandoffParams is the input for CreateHandoff.
type HandoffParams struct {
	Scope         string
	Summary       string
	NextSteps     []string
	SourceAgent   string
	SourceSession string
	ExpiresAt     time.Time
}

// CreateHandoff stores a handoff note as a temporary memory. There is at most
// one active handoff per scope: earlier ones are marked superseded, and the new
// note links to the latest of them.
func (s *Store) CreateHandoff(ctx context.Context, p HandoffParams) (Memory, error) {
	attrs := map[string]any{"kind": "handoff"}
	if len(p.NextSteps) > 0 {
		attrs["next_steps"] = p.NextSteps
	}
	var m Memory
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Serialize handoffs per scope so concurrent writers cannot both stay active.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kenfold.handoff:' || $1, 0))`, p.Scope); err != nil {
			return err
		}
		var prev *string
		err := tx.QueryRow(ctx, `
			SELECT id FROM memory
			WHERE scope = $1 AND type = 'temporary' AND attrs->>'kind' = 'handoff' AND status = 'active'
			ORDER BY created_at DESC LIMIT 1`, p.Scope).Scan(&prev)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE memory SET status = 'superseded'
			WHERE scope = $1 AND type = 'temporary' AND attrs->>'kind' = 'handoff' AND status = 'active'`, p.Scope); err != nil {
			return err
		}
		return scan(tx.QueryRow(ctx, `
			INSERT INTO memory (type, scope, content, attrs, source_agent, source_session, trust, status, supersedes, expires_at)
			VALUES ('temporary', $1, $2, $3, $4, NULLIF($5, ''), 'agent', 'active', $6, $7)
			RETURNING `+columns,
			p.Scope, p.Summary, attrs, p.SourceAgent, p.SourceSession, prev, p.ExpiresAt), &m)
	})
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// LatestHandoff returns the most recent active, unexpired handoff for scope, or
// ErrNotFound. With pendingOnly, handoffs that were already resumed are skipped.
func (s *Store) LatestHandoff(ctx context.Context, scope string, pendingOnly bool) (Memory, error) {
	var m Memory
	err := scan(s.pool.QueryRow(ctx, `
		SELECT `+columns+` FROM memory
		WHERE scope = $1 AND type = 'temporary' AND attrs->>'kind' = 'handoff'
		  AND status = 'active'
		  AND (expires_at IS NULL OR expires_at > now())
		  AND (NOT $2 OR NOT attrs ? 'resumed_at')
		ORDER BY created_at DESC
		LIMIT 1`, scope, pendingOnly), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// MarkResumed records the first agent that resumed a handoff. Later calls keep
// the original record and return the memory unchanged.
func (s *Store) MarkResumed(ctx context.Context, id, agent string) (Memory, error) {
	if !ValidID(id) {
		return Memory{}, ErrInvalidID
	}
	var m Memory
	err := scan(s.pool.QueryRow(ctx, `
		UPDATE memory
		SET attrs = attrs || jsonb_build_object('resumed_at', now(), 'resumed_by', $2::text)
		WHERE id = $1 AND NOT attrs ? 'resumed_at'
		RETURNING `+columns, id, agent), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.Get(ctx, id)
	}
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// ---- reads ----

// Get returns a memory by id regardless of status.
func (s *Store) Get(ctx context.Context, id string) (Memory, error) {
	if !ValidID(id) {
		return Memory{}, ErrInvalidID
	}
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

// ListParams filters List. Zero values mean "no filter", except Statuses
// (default: active only) and Limit (default 50, max 500).
type ListParams struct {
	Scopes   []string
	Types    []memory.Type
	Statuses []memory.Status
	Limit    int
}

// List returns unexpired memories matching p, newest first.
func (s *Store) List(ctx context.Context, p ListParams) ([]Memory, error) {
	statuses := p.Statuses
	if len(statuses) == 0 {
		statuses = []memory.Status{memory.StatusActive}
	}
	limit := clamp(p.Limit, 50, 500)
	rows, err := s.pool.Query(ctx, `
		SELECT `+columns+` FROM memory
		WHERE status = ANY($1)
		  AND ($2::text[] IS NULL OR scope = ANY($2))
		  AND ($3::text[] IS NULL OR type = ANY($3))
		  AND (expires_at IS NULL OR expires_at > now())
		ORDER BY created_at DESC, id DESC
		LIMIT $4`,
		stringsOf(statuses), nilIfEmpty(p.Scopes), stringsOf(p.Types), limit)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Memory, error) {
		var m Memory
		return m, scan(r, &m)
	})
}

// DefaultMaxDistance is the cosine distance above which vector matches are
// ignored, so that unrelated memories are not returned just for being nearest.
const DefaultMaxDistance = 0.55

// SearchParams controls Search.
type SearchParams struct {
	Query string        // natural-language query
	Scope string        // project/repo scope searched in addition to 'user'; empty = user only
	Types []memory.Type // empty = all types
	Limit int           // default 10, max 50

	// Vector is the query embedding (optional). Only memories embedded with the
	// same Model are compared.
	Vector      []float32
	Model       string
	MaxDistance float64 // <= 0 means DefaultMaxDistance
}

// rrfK is the Reciprocal Rank Fusion constant (Cormack et al., 2009).
const rrfK = 60

// Search returns active, unexpired memories relevant to the query, in the given
// scope plus user-wide memories. It fuses two rankings with Reciprocal Rank
// Fusion: full-text (OR of the query terms, ranked by ts_rank) and, when a query
// vector is given, cosine similarity. Scores are normalized so a memory ranked
// first by every ranker scores 1.0.
func (s *Store) Search(ctx context.Context, p SearchParams) ([]Scored, error) {
	limit := clamp(p.Limit, 10, 50)
	candidates := max(50, limit*4)
	scopes := []string{"user"}
	if p.Scope != "" && p.Scope != "user" {
		scopes = append(scopes, p.Scope)
	}
	vec, err := vectorParam(p.Vector)
	if err != nil {
		return nil, err
	}
	rankers := 1.0
	if vec != nil {
		if p.Model == "" {
			return nil, errors.New("query vector given without a model name")
		}
		rankers = 2
	}
	maxDist := p.MaxDistance
	if maxDist <= 0 {
		maxDist = DefaultMaxDistance
	}

	q := searchSQL
	args := []any{p.Query, scopes, stringsOf(p.Types), limit, candidates, vec, p.Model, maxDist, rankers}
	collect := func(rows pgx.Rows) ([]Scored, error) {
		return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Scored, error) {
			var sc Scored
			return sc, r.Scan(append(fields(&sc.Memory), &sc.Score)...)
		})
	}

	if vec == nil {
		rows, err := s.pool.Query(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		return collect(rows)
	}

	// With filters, an HNSW index scan can return fewer than LIMIT rows; iterative
	// scans (pgvector 0.8+) keep scanning. relaxed_order is re-sorted above.
	var out []Scored
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = relaxed_order`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		out, err = collect(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return out, nil
}

// ---- similarity and review ----

// SimilarParams controls Similar.
type SimilarParams struct {
	Scope   string
	Content string
	Exclude string    // memory id to leave out (e.g. the one just written)
	Vector  []float32 // embedding of Content; optional
	Model   string
	// MaxDistance is the cosine distance cutoff (default 0.25: much tighter
	// than search, because a hint claims "this may be the same fact").
	MaxDistance float64
	// MinTrigram is the pg_trgm similarity cutoff (default 0.5).
	MinTrigram float64
	Limit      int // default 3

	includeProposed bool // see SimilarAny
}

// Similar returns active memories in the same scope whose content is close to
// Content, by vector distance or trigram similarity. Callers present them as
// "maybe the same fact, or one this contradicts"; nothing is merged. Score is
// the larger of cosine similarity and trigram similarity.
func (s *Store) Similar(ctx context.Context, p SimilarParams) ([]Scored, error) {
	limit := clamp(p.Limit, 3, 10)
	maxDist := p.MaxDistance
	if maxDist <= 0 {
		maxDist = 0.25
	}
	minTrgm := p.MinTrigram
	if minTrgm <= 0 {
		minTrgm = 0.5
	}
	vec, err := vectorParam(p.Vector)
	if err != nil {
		return nil, err
	}
	var exclude *string
	if p.Exclude != "" {
		if !ValidID(p.Exclude) {
			return nil, ErrInvalidID
		}
		exclude = &p.Exclude
	}
	statuses := []string{"active"}
	if p.includeProposed {
		statuses = append(statuses, "proposed")
	}
	// $1 scope, $2 content, $3 exclude, $4 vector, $5 model, $6 max distance,
	// $7 min trigram similarity, $8 limit, $9 statuses.
	rows, err := s.pool.Query(ctx, `
		WITH cand AS (
		    SELECT m.id, 1 - (m.embedding <=> $4::text::vector) AS sim
		    FROM memory m
		    WHERE $4::text IS NOT NULL AND m.embedding IS NOT NULL AND m.embedding_model = $5
		      AND m.scope = $1 AND m.status = ANY($9)
		      AND (m.expires_at IS NULL OR m.expires_at > now())
		      AND ($3::uuid IS NULL OR m.id <> $3::uuid)
		      AND m.embedding <=> $4::text::vector <= $6
		    UNION ALL
		    SELECT m.id, similarity(m.content, $2) AS sim
		    FROM memory m
		    WHERE m.scope = $1 AND m.status = ANY($9)
		      AND (m.expires_at IS NULL OR m.expires_at > now())
		      AND ($3::uuid IS NULL OR m.id <> $3::uuid)
		      AND m.content % $2
		      AND similarity(m.content, $2) >= $7
		),
		best AS (SELECT id, max(sim) AS sim FROM cand GROUP BY id)
		SELECT `+mColumns+`, b.sim::float8
		FROM best b JOIN memory m ON m.id = b.id
		WHERE m.type <> 'temporary'
		ORDER BY b.sim DESC, m.created_at DESC
		LIMIT $8`,
		p.Scope, p.Content, exclude, vec, p.Model, maxDist, minTrgm, limit, statuses)
	if err != nil {
		return nil, fmt.Errorf("similar: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Scored, error) {
		var sc Scored
		return sc, r.Scan(append(fields(&sc.Memory), &sc.Score)...)
	})
}

// RecentEpisodes returns the newest active episodic memories (session
// summaries) in scope, newest first.
func (s *Store) RecentEpisodes(ctx context.Context, scope string, limit int) ([]Memory, error) {
	return s.List(ctx, ListParams{Scopes: []string{scope}, Types: []memory.Type{memory.TypeEpisodic}, Limit: clamp(limit, 3, 20)})
}

// ForEach calls fn for every memory with one of statuses (all statuses when
// empty), in id order. It stops at the first error fn returns.
func (s *Store) ForEach(ctx context.Context, statuses []memory.Status, fn func(Memory) error) error {
	var after *string
	for {
		rows, err := s.pool.Query(ctx, `
			SELECT `+columns+` FROM memory
			WHERE ($1::text[] IS NULL OR status = ANY($1))
			  AND ($2::uuid IS NULL OR id > $2::uuid)
			ORDER BY id LIMIT 500`, stringsOf(statuses), after)
		if err != nil {
			return err
		}
		batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Memory, error) {
			var m Memory
			return m, scan(r, &m)
		})
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, m := range batch {
			if err := fn(m); err != nil {
				return err
			}
		}
		after = &batch[len(batch)-1].ID
	}
}

// Rewrite replaces a memory's content and attrs in place and clears its
// embedding (so it is re-embedded from the new text). It exists for one
// purpose: removing secrets that were stored before the secret filter. Normal
// updates go through supersede, which keeps history. attrs replaces the whole
// attrs object; redacted_at and redact_reason are added to it.
func (s *Store) Rewrite(ctx context.Context, id, content string, attrs map[string]any, reason string) (Memory, error) {
	if !ValidID(id) {
		return Memory{}, ErrInvalidID
	}
	if strings.TrimSpace(content) == "" {
		return Memory{}, errors.New("replacement content is empty")
	}
	if attrs == nil {
		attrs = map[string]any{}
	}
	var m Memory
	err := scan(s.pool.QueryRow(ctx, `
		UPDATE memory
		SET content = $2, embedding = NULL, embedding_model = NULL,
		    attrs = $3::jsonb || jsonb_build_object('redacted_at', now(), 'redact_reason', $4::text)
		WHERE id = $1
		RETURNING `+columns, id, content, attrs, reason), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// FindSessionMemory returns the newest active memory of typ in scope written by
// agent in session, or ErrNotFound. Hooks use it so that a session's summary is
// replaced, not duplicated, when a session ends more than once.
func (s *Store) FindSessionMemory(ctx context.Context, scope string, typ memory.Type, agent, session string) (Memory, error) {
	var m Memory
	err := scan(s.pool.QueryRow(ctx, `
		SELECT `+columns+` FROM memory
		WHERE scope = $1 AND type = $2 AND source_agent = $3 AND source_session = $4
		  AND status = 'active'
		ORDER BY created_at DESC LIMIT 1`, scope, typ, agent, session), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// ---- embeddings ----

// Embedder produces embeddings. It is satisfied by *embed.Client; defined here
// so the store does not depend on any particular provider.
type Embedder interface {
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// SetEmbedding stores a memory's embedding and the model that produced it.
func (s *Store) SetEmbedding(ctx context.Context, id string, v []float32, model string) error {
	if !ValidID(id) {
		return ErrInvalidID
	}
	vec, err := vectorParam(v)
	if err != nil {
		return err
	}
	if vec == nil || model == "" {
		return errors.New("SetEmbedding: vector and model are required")
	}
	ct, err := s.pool.Exec(ctx, `UPDATE memory SET embedding = $2::text::vector, embedding_model = $3 WHERE id = $1`, id, vec, model)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// BackfillStats reports the outcome of Backfill.
type BackfillStats struct {
	Embedded int // rows that received an embedding
	Skipped  int // rows the provider could not embed; retried on the next run
}

// Backfill embeds active and proposed memories that have no embedding from
// e's model (never embedded, or embedded by a different model). It walks rows
// in id order so it always terminates. If a batch fails, its rows are retried
// one at a time so a single input the provider rejects cannot block the rest;
// it returns an error only if the provider fails for a whole batch.
func (s *Store) Backfill(ctx context.Context, e Embedder, batch int) (BackfillStats, error) {
	batch = clamp(batch, 16, 256)
	model := e.Model()
	var after *string
	var st BackfillStats
	for {
		rows, err := s.pool.Query(ctx, `
			SELECT id, content FROM memory
			WHERE status IN ('active', 'proposed')
			  AND embedding_model IS DISTINCT FROM $1
			  AND ($2::uuid IS NULL OR id > $2::uuid)
			ORDER BY id
			LIMIT $3`, model, after, batch)
		if err != nil {
			return st, fmt.Errorf("backfill: %w", err)
		}
		type pending struct{ ID, Content string }
		todo, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pending])
		if err != nil {
			return st, fmt.Errorf("backfill: %w", err)
		}
		if len(todo) == 0 {
			return st, nil
		}
		texts := make([]string, len(todo))
		for i, t := range todo {
			texts[i] = t.Content
		}
		vecs, err := e.Embed(ctx, texts)
		if err == nil && len(vecs) != len(todo) {
			err = fmt.Errorf("embedder returned %d vectors for %d texts", len(vecs), len(todo))
		}
		if err != nil {
			vecs, err = embedEach(ctx, e, texts)
			if err != nil {
				return st, fmt.Errorf("backfill: embed: %w", err)
			}
		}
		for i, t := range todo {
			if vecs[i] == nil {
				st.Skipped++
				continue
			}
			if _, verr := vectorParam(vecs[i]); verr != nil {
				st.Skipped++ // e.g. wrong dimension or NaN from the provider
				continue
			}
			if err := s.SetEmbedding(ctx, t.ID, vecs[i], model); err != nil && !errors.Is(err, ErrNotFound) {
				return st, fmt.Errorf("backfill: %s: %w", t.ID, err)
			}
			st.Embedded++
		}
		after = &todo[len(todo)-1].ID
	}
}

// embedEach embeds texts one at a time, leaving nil for inputs that fail. It
// returns an error only if every input fails (the provider is likely down).
func embedEach(ctx context.Context, e Embedder, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	var lastErr error
	ok := 0
	for i, t := range texts {
		v, err := e.Embed(ctx, []string{t})
		if err == nil && len(v) != 1 {
			err = fmt.Errorf("embedder returned %d vectors for 1 text", len(v))
		}
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		out[i] = v[0]
		ok++
	}
	if ok == 0 {
		return nil, lastErr
	}
	return out, nil
}

// vectorParam renders v as a pgvector text literal, or nil (SQL NULL) when v is
// empty. It rejects wrong dimensions and non-finite values, which pgvector
// would otherwise reject with a less helpful error.
func vectorParam(v []float32) (*string, error) {
	if len(v) == 0 {
		return nil, nil
	}
	if len(v) != EmbeddingDim {
		return nil, fmt.Errorf("embedding has %d dimensions; Kenfold requires %d", len(v), EmbeddingDim)
	}
	var b strings.Builder
	b.Grow(len(v) * 10)
	b.WriteByte('[')
	for i, f := range v {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return nil, fmt.Errorf("embedding value %d is not finite", i)
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	lit := b.String()
	return &lit, nil
}

// ---- helpers ----

// ValidID reports whether id is a canonical, hyphenated UUID string.
func ValidID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

func clamp(v, def, maxV int) int {
	if v <= 0 {
		return def
	}
	return min(v, maxV)
}

// stringsOf converts enum slices to []string for text[] parameters; empty → nil (no filter).
func stringsOf[T ~string](vs []T) []string {
	if len(vs) == 0 {
		return nil
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// columnList is the canonical select list; fields must match it exactly.
var columnList = []string{
	"id", "type", "scope", "content", "attrs", "source_agent", "source_session",
	"evidence_uri", "trust", "confidence", "status", "supersedes",
	"valid_from", "valid_to", "expires_at", "embedding_model", "created_at", "updated_at",
}

var (
	columns  = strings.Join(columnList, ", ")
	mColumns = "m." + strings.Join(columnList, ", m.")
)

// searchSQL implements Search. Parameters:
//
//	$1 query text   $2 scopes   $3 types (NULL = all)   $4 limit   $5 candidates per ranker
//	$6 query vector literal (NULL = lexical only)   $7 embedding model   $8 max cosine distance
//	$9 number of rankers (score normalization)
var searchSQL = fmt.Sprintf(`
	WITH q AS (
	    -- OR the query's lexemes (parsed by plainto_tsquery, so input is never
	    -- interpreted as tsquery syntax) instead of requiring all of them.
	    SELECT to_tsquery('simple', replace(plainto_tsquery('simple', $1)::text, '&', '|')) AS tsq
	),
	lex AS (
	    SELECT m.id, row_number() OVER (ORDER BY ts_rank(m.content_tsv, q.tsq) DESC, m.id DESC) AS rnk
	    FROM memory m, q
	    WHERE m.status = 'active'
	      AND (m.expires_at IS NULL OR m.expires_at > now())
	      AND m.scope = ANY($2)
	      AND ($3::text[] IS NULL OR m.type = ANY($3))
	      AND m.content_tsv @@ q.tsq
	    ORDER BY rnk
	    LIMIT $5
	),
	vec AS MATERIALIZED (
	    SELECT m.id, m.embedding <=> $6::text::vector AS dist
	    FROM memory m
	    WHERE $6::text IS NOT NULL
	      AND m.embedding IS NOT NULL
	      AND m.embedding_model = $7
	      AND m.status = 'active'
	      AND (m.expires_at IS NULL OR m.expires_at > now())
	      AND m.scope = ANY($2)
	      AND ($3::text[] IS NULL OR m.type = ANY($3))
	    ORDER BY m.embedding <=> $6::text::vector
	    LIMIT $5
	),
	vec_ranked AS (
	    SELECT id, row_number() OVER (ORDER BY dist, id DESC) AS rnk
	    FROM vec WHERE dist <= $8
	),
	fused AS (
	    SELECT id, sum(1.0 / (%[1]d + rnk)) AS rrf
	    FROM (SELECT id, rnk FROM lex UNION ALL SELECT id, rnk FROM vec_ranked) u
	    GROUP BY id
	)
	SELECT %[2]s, (f.rrf * %[3]d / $9::float8)::float8 AS score
	FROM fused f JOIN memory m ON m.id = f.id
	ORDER BY score DESC, m.created_at DESC
	LIMIT $4`, rrfK, mColumns, rrfK+1)

func fields(m *Memory) []any {
	return []any{
		&m.ID, &m.Type, &m.Scope, &m.Content, &m.Attrs, &m.SourceAgent, &m.SourceSession,
		&m.EvidenceURI, &m.Trust, &m.Confidence, &m.Status, &m.Supersedes,
		&m.ValidFrom, &m.ValidTo, &m.ExpiresAt, &m.EmbeddingModel, &m.CreatedAt, &m.UpdatedAt,
	}
}

type row interface {
	Scan(dest ...any) error
}

func scan(r row, m *Memory) error { return r.Scan(fields(m)...) }
