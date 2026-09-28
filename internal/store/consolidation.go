package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Consolidation proposal kinds (migration 00007).
const (
	// KindDuplicate: one memory says everything the other says. Keep it and
	// retire the other.
	KindDuplicate = "duplicate"
	// KindConflict: the memories contradict each other. Keep the newer and
	// retire the older.
	KindConflict = "conflict"
	// KindDigest: old session summaries of a scope, replaced by one digest.
	KindDigest = "digest"
	// KindDistinct: the model found the pair different. Recorded (dismissed)
	// so it is not judged again; never shown.
	KindDistinct = "distinct"
)

// Proposal statuses.
const (
	ProposalPending   = "pending"
	ProposalApplied   = "applied"
	ProposalRejected  = "rejected"
	ProposalDismissed = "dismissed"
	// ProposalStale: a member was retired, forgotten, or expired before the
	// owner decided.
	ProposalStale = "stale"
)

// RelReplaces links the memory that stays active (or a digest) to each
// memory a consolidation retired.
const RelReplaces = "replaces"

// DigestAgent is the source_agent of digests.
const DigestAgent = "kenfold-consolidator"

// notDigest holds for memories (alias m) that are not consolidation digests.
// Digests are not extracted (their sessions were) and not digested again.
const notDigest = `coalesce(m.attrs->>'kind', '') <> 'digest'`

var (
	ErrProposalDecided = errors.New("the proposal was already decided")
	ErrProposalStale   = errors.New("the memories changed since the proposal was made; nothing was changed")
)

// Proposal is a consolidation proposal.
type Proposal struct {
	ID        string
	Kind      string
	Scope     string
	MemberIDs []string // sorted
	Keep      string   // duplicate, conflict: the member that stays active
	Content   string   // digest
	Reason    string
	Model     string
	Status    string
	ResultID  string // applied: the kept memory or the digest
	DecidedBy string
	CreatedAt time.Time
	Members   []Memory // PendingProposals: the members, oldest first
}

const proposalColumns = `id::text, kind, scope, member_ids::text[], coalesce(keep_id::text, ''), content, reason, model,
	status, coalesce(result_id::text, ''), coalesce(decided_by, ''), created_at`

func scanProposal(r pgx.Row, p *Proposal) error {
	return r.Scan(&p.ID, &p.Kind, &p.Scope, &p.MemberIDs, &p.Keep, &p.Content, &p.Reason, &p.Model,
		&p.Status, &p.ResultID, &p.DecidedBy, &p.CreatedAt)
}

// proposalLive holds for proposals (alias c) whose members are all active
// and unexpired.
const proposalLive = `NOT EXISTS (
	SELECT 1 FROM unnest(c.member_ids) AS x(id) LEFT JOIN memory lm ON lm.id = x.id
	WHERE lm.id IS NULL OR lm.status <> 'active' OR (lm.expires_at IS NOT NULL AND lm.expires_at <= now()))`

// PairParams controls ConsolidationPairs.
type PairParams struct {
	Before      time.Time // only memories created before this (settled)
	MaxDistance float64   // cosine distance cutoff (default 0.25, as for similarity hints)
	MinTrigram  float64   // trigram similarity cutoff (default 0.5)
	Limit       int       // default 10, max 100
}

// Pair is two active memories of the same scope and type that may say the
// same thing. A is the older.
type Pair struct {
	A, B  Memory
	Score float64
}

// ConsolidationPairs returns pairs of similar active memories, most similar
// first. Episodic and temporary memories are left out, as are pairs judged
// before and memories in a pending proposal.
func (s *Store) ConsolidationPairs(ctx context.Context, p PairParams) ([]Pair, error) {
	rows, err := s.pool.Query(ctx, `
		WITH cand AS (
		    SELECT id, scope, type, content, embedding, embedding_model FROM memory
		    WHERE status = 'active' AND type NOT IN ('episodic', 'temporary')
		      AND (expires_at IS NULL OR expires_at > now()) AND created_at < $1::timestamptz
		),
		near AS (
		    SELECT a.id AS a, n.id AS b, n.sim FROM cand a
		    CROSS JOIN LATERAL (
		        SELECT m.id, 1 - (m.embedding <=> a.embedding) AS sim FROM memory m
		        WHERE m.embedding IS NOT NULL AND m.embedding_model = a.embedding_model
		          AND m.status = 'active' AND m.scope = a.scope AND m.type = a.type AND m.id <> a.id
		          AND (m.expires_at IS NULL OR m.expires_at > now()) AND m.created_at < $1::timestamptz
		        ORDER BY m.embedding <=> a.embedding
		        LIMIT 3
		    ) n
		    WHERE a.embedding IS NOT NULL AND n.sim >= 1 - $2::float8
		    UNION ALL
		    SELECT a.id, m.id, similarity(a.content, m.content) FROM cand a
		    JOIN memory m ON m.status = 'active' AND m.content % a.content
		      AND m.scope = a.scope AND m.type = a.type AND m.id > a.id
		      AND (m.expires_at IS NULL OR m.expires_at > now()) AND m.created_at < $1::timestamptz
		    WHERE similarity(a.content, m.content) >= $3::float8
		),
		pairs AS (SELECT least(a, b) AS a, greatest(a, b) AS b, max(sim) AS sim FROM near GROUP BY 1, 2)
		SELECT p.a::text, p.b::text, p.sim::float8 FROM pairs p
		WHERE NOT EXISTS (
		    SELECT 1 FROM consolidation c
		    WHERE c.member_ids = ARRAY[p.a, p.b] OR (c.status = 'pending' AND c.member_ids && ARRAY[p.a, p.b]))
		ORDER BY p.sim DESC, p.b DESC
		LIMIT $4::int`,
		p.Before, cmp.Or(p.MaxDistance, 0.25), cmp.Or(p.MinTrigram, 0.5), clamp(p.Limit, 10, 100))
	if err != nil {
		return nil, fmt.Errorf("consolidation pairs: %w", err)
	}
	type idPair struct {
		a, b  string
		score float64
	}
	ids, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (idPair, error) {
		var x idPair
		return x, r.Scan(&x.a, &x.b, &x.score)
	})
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	var all []string
	for _, x := range ids {
		all = append(all, x.a, x.b)
	}
	byID, err := s.memoriesByID(ctx, all)
	if err != nil {
		return nil, err
	}
	out := make([]Pair, 0, len(ids))
	for _, x := range ids {
		a, b := byID[x.a], byID[x.b]
		if a.ID == "" || b.ID == "" {
			continue
		}
		if b.CreatedAt.Before(a.CreatedAt) {
			a, b = b, a
		}
		out = append(out, Pair{A: a, B: b, Score: x.score})
	}
	return out, nil
}

func (s *Store) memoriesByID(ctx context.Context, ids []string) (map[string]Memory, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM memory WHERE id = ANY($1::text[]::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("load memories: %w", err)
	}
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Memory, error) {
		var m Memory
		return m, scan(r, &m)
	})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Memory, len(ms))
	for _, m := range ms {
		byID[m.ID] = m
	}
	return byID, nil
}

// DigestParams controls DigestGroups.
type DigestParams struct {
	Before time.Time // sessions created before this are old enough
	Min    int       // sessions a scope needs (default 6)
	Max    int       // sessions per digest (default 10, max 50)
	Limit  int       // scopes (default 1, max 20)
}

// DigestGroup is the oldest old session summaries of a scope, oldest first.
type DigestGroup struct {
	Scope    string
	Sessions []Memory
}

// digestEligible selects (alias m, $1 = Before) active episodic memories
// that are old enough, are not digests themselves, and are in no pending,
// rejected, or dismissed digest.
const digestEligible = `m.type = 'episodic' AND m.status = 'active' AND (m.expires_at IS NULL OR m.expires_at > now())
	AND m.created_at < $1::timestamptz AND ` + notDigest + `
	AND NOT EXISTS (SELECT 1 FROM consolidation c
	                WHERE c.kind = 'digest' AND c.status IN ('pending', 'rejected', 'dismissed') AND c.member_ids @> ARRAY[m.id])`

// DigestGroups returns, for scopes with at least Min old session summaries,
// the oldest Max of them.
func (s *Store) DigestGroups(ctx context.Context, p DigestParams) ([]DigestGroup, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.scope FROM memory m WHERE `+digestEligible+`
		GROUP BY m.scope HAVING count(*) >= $2::int
		ORDER BY min(m.created_at), m.scope
		LIMIT $3::int`, p.Before, max(cmp.Or(p.Min, 6), 2), clamp(p.Limit, 1, 20))
	if err != nil {
		return nil, fmt.Errorf("digest scopes: %w", err)
	}
	scopes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []DigestGroup
	for _, scope := range scopes {
		rows, err := s.pool.Query(ctx, `
			SELECT `+mColumns+` FROM memory m WHERE m.scope = $2 AND `+digestEligible+`
			ORDER BY m.created_at, m.id
			LIMIT $3::int`, p.Before, scope, clamp(p.Max, 10, 50))
		if err != nil {
			return nil, fmt.Errorf("digest sessions: %w", err)
		}
		ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Memory, error) {
			var m Memory
			return m, scan(r, &m)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, DigestGroup{Scope: scope, Sessions: ms})
	}
	return out, nil
}

// NewProposal is the input for CreateProposal.
type NewProposal struct {
	Kind      string
	Scope     string
	MemberIDs []string
	Keep      string // duplicate, conflict
	Content   string // digest
	Reason    string
	Model     string
	// Dismissed records the proposal as dismissed instead of pending (always
	// for KindDistinct): e.g. a digest that failed validation, so the same
	// sessions are not sent to the model again.
	Dismissed bool
}

// CreateProposal records a proposal, or for KindDistinct a dismissed pair.
// It returns ok=false, and no error, when the member set was judged before.
func (s *Store) CreateProposal(ctx context.Context, p NewProposal) (Proposal, bool, error) {
	if len(p.MemberIDs) < 2 {
		return Proposal{}, false, errors.New("a proposal needs at least two memories")
	}
	for _, id := range append(slices.Clone(p.MemberIDs), cmp.Or(p.Keep, p.MemberIDs[0])) {
		if !ValidID(id) {
			return Proposal{}, false, ErrInvalidID
		}
	}
	var out Proposal
	err := scanProposal(s.pool.QueryRow(ctx, `
		INSERT INTO consolidation (kind, scope, member_ids, keep_id, content, reason, model, status)
		VALUES ($1::text, $2, ARRAY(SELECT DISTINCT x FROM unnest($3::text[]::uuid[]) AS x ORDER BY 1),
		        NULLIF($4, '')::uuid, $5, $6, $7, CASE WHEN $1::text = 'distinct' OR $8 THEN 'dismissed' ELSE 'pending' END)
		ON CONFLICT (member_ids) DO NOTHING
		RETURNING `+proposalColumns,
		p.Kind, p.Scope, p.MemberIDs, p.Keep, p.Content, p.Reason, p.Model, p.Dismissed), &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, false, nil
	}
	if err != nil {
		return Proposal{}, false, fmt.Errorf("create proposal: %w", err)
	}
	return out, true, nil
}

// PendingProposals returns pending proposals whose members are all still
// active, oldest first, with their members.
func (s *Store) PendingProposals(ctx context.Context, limit int) ([]Proposal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+proposalColumns+` FROM consolidation c
		WHERE c.status = 'pending' AND `+proposalLive+`
		ORDER BY c.created_at, c.id
		LIMIT $1`, clamp(limit, 50, 200))
	if err != nil {
		return nil, fmt.Errorf("pending proposals: %w", err)
	}
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Proposal, error) {
		var p Proposal
		return p, scanProposal(r, &p)
	})
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	var ids []string
	for _, p := range ps {
		ids = append(ids, p.MemberIDs...)
	}
	byID, err := s.memoriesByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range ps {
		for _, id := range ps[i].MemberIDs {
			if m, ok := byID[id]; ok {
				ps[i].Members = append(ps[i].Members, m)
			}
		}
		slices.SortStableFunc(ps[i].Members, func(a, b Memory) int { return a.CreatedAt.Compare(b.CreatedAt) })
	}
	return ps, nil
}

// CountPendingProposals counts pending proposals whose members are all active.
func (s *Store) CountPendingProposals(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM consolidation c WHERE c.status = 'pending' AND `+proposalLive).Scan(&n)
	return n, err
}

// MarkStaleProposals marks pending proposals with a member that is no longer
// active as stale.
func (s *Store) MarkStaleProposals(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE consolidation c SET status = 'stale' WHERE c.status = 'pending' AND NOT `+proposalLive)
	if err != nil {
		return 0, fmt.Errorf("mark stale proposals: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ProposalCounts counts proposals by status.
func (s *Store) ProposalCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM consolidation GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("proposal counts: %w", err)
	}
	out := map[string]int{}
	return out, collectCounts(rows, func(k string, n int) { out[k] = n })
}

// ApplyProposal carries out pending proposal id; agent is who decided. A
// duplicate or conflict retires every member except Keep. A digest is
// stored as an active episodic memory, dated like the last session it
// covers so it does not count as a recent session, and retires the
// sessions. Retired memories become superseded (kept as history) and are
// linked (RelReplaces) from the memory that replaces them. If a member is
// no longer active, the proposal becomes stale and ErrProposalStale is
// returned.
func (s *Store) ApplyProposal(ctx context.Context, id, agent string) (Proposal, error) {
	if !ValidID(id) {
		return Proposal{}, ErrInvalidID
	}
	var p Proposal
	stale := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := scanProposal(tx.QueryRow(ctx, `SELECT `+proposalColumns+` FROM consolidation WHERE id = $1 FOR UPDATE`, id), &p)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if p.Status != ProposalPending {
			return fmt.Errorf("%w (status is %s)", ErrProposalDecided, p.Status)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM memory WHERE id = ANY($1::text[]::uuid[]) ORDER BY id FOR UPDATE`, p.MemberIDs); err != nil {
			return err
		}
		var active int
		var first, last time.Time
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE status = 'active' AND (expires_at IS NULL OR expires_at > now())),
			       coalesce(min(created_at), now()), coalesce(max(created_at), now())
			FROM memory WHERE id = ANY($1::text[]::uuid[])`, p.MemberIDs).Scan(&active, &first, &last); err != nil {
			return err
		}
		if active != len(p.MemberIDs) {
			stale = true
			_, err := tx.Exec(ctx, `UPDATE consolidation SET status = 'stale' WHERE id = $1`, id)
			return err
		}

		result := p.Keep
		if p.Kind == KindDigest {
			attrs := map[string]any{
				"kind": "digest", "consolidator_model": p.Model, "consolidation_id": p.ID, "session_count": len(p.MemberIDs),
				"period_from": first.UTC().Format(time.DateOnly), "period_to": last.UTC().Format(time.DateOnly),
				"consolidated_at": time.Now().UTC().Format(time.RFC3339),
			}
			if err := tx.QueryRow(ctx, `
				INSERT INTO memory (type, scope, content, attrs, source_agent, trust, confidence, status, created_at)
				VALUES ('episodic', $1, $2, $3, $4, 'user', 0.9, 'active', $5)
				RETURNING id::text`, p.Scope, p.Content, attrs, DigestAgent, last).Scan(&result); err != nil {
				return fmt.Errorf("store digest: %w", err)
			}
		}
		retired := slices.DeleteFunc(slices.Clone(p.MemberIDs), func(m string) bool { return m == p.Keep })
		if _, err := tx.Exec(ctx, `UPDATE memory SET status = 'superseded' WHERE id = ANY($1::text[]::uuid[])`, retired); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_edge (src, dst, relation, source_agent)
			SELECT $1::uuid, x, $3, $4 FROM unnest($2::text[]::uuid[]) AS x
			ON CONFLICT DO NOTHING`, result, retired, RelReplaces, agent); err != nil {
			return err
		}
		return scanProposal(tx.QueryRow(ctx, `
			UPDATE consolidation SET status = 'applied', decided_by = $2, decided_at = now(), result_id = $3::uuid
			WHERE id = $1 RETURNING `+proposalColumns, id, agent, result), &p)
	})
	if err == nil && stale {
		return p, ErrProposalStale
	}
	if err != nil {
		return Proposal{}, err
	}
	return p, nil
}

// RejectProposal records that the owner keeps the memories as they are; the
// same memories are not proposed together again.
func (s *Store) RejectProposal(ctx context.Context, id, agent string) error {
	if !ValidID(id) {
		return ErrInvalidID
	}
	var updated int
	var status string
	err := s.pool.QueryRow(ctx, `
		WITH upd AS (
		    UPDATE consolidation SET status = 'rejected', decided_by = $2, decided_at = now()
		    WHERE id = $1 AND status = 'pending' RETURNING 1)
		SELECT (SELECT count(*) FROM upd), coalesce((SELECT status FROM consolidation WHERE id = $1), '')`, id, agent).Scan(&updated, &status)
	switch {
	case err != nil:
		return fmt.Errorf("reject proposal: %w", err)
	case updated == 1:
		return nil
	case status == "":
		return ErrNotFound
	default:
		return fmt.Errorf("%w (status is %s)", ErrProposalDecided, status)
	}
}
