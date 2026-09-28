// Package retrieve ranks memories for a query in stages:
//
//  1. First stage: the store's hybrid search (full-text and vector rankings
//     fused with RRF) returns a candidate pool.
//  2. Graph expansion: memories one edge away from the top candidates (and
//     facts extracted from the same session) join the pool, so a session
//     summary brings the facts derived from it and vice versa.
//  3. Rerank (optional): a cross-encoder scores the best candidates against
//     the query. It reads query and memory together, which the first stage
//     cannot.
//  4. Signals: code staleness (memories about code that has changed or
//     disappeared since they were written rank lower) and recency (queries
//     that ask about recent work prefer recent memories; session summaries
//     age).
//
// Graph expansion and the aging of session summaries are soft adjustments
// sized for the reranker's scores, which are probabilities spread over
// (0, 1). First-stage scores are fused ranks packed into a narrow band, where
// the same adjustments would reorder far more than intended (on the eval
// set they pushed answers out of the top 5). So they apply only when the
// reranker scored the candidates; staleness and temporal intent are strong
// by design and apply either way.
//
// Every stage degrades: without an embedder the first stage is full-text
// only, and a failing reranker leaves the first-stage order in place.
package retrieve

import (
	"cmp"
	"context"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// embedTimeout bounds the query embedding; on timeout or error the search
// runs full-text only.
const embedTimeout = 10 * time.Second

// Reranker scores documents against a query (see package rerank). Scores are
// in [0, 1], in document order.
type Reranker interface {
	Model() string
	Rerank(ctx context.Context, query string, docs []string) ([]float64, error)
}

// Options tunes the pipeline. The zero value is the default configuration;
// the No* fields exist for evaluation and troubleshooting.
type Options struct {
	// MaxDistance is the cosine distance cutoff of vector matches
	// (store.DefaultMaxDistance when zero).
	MaxDistance float64
	// Pool is the number of first-stage candidates (default 30).
	Pool int
	// RerankTop is how many of the best candidates are reranked (default 15).
	RerankTop int
	// RerankTimeout bounds the rerank call (default 4s); on timeout the
	// earlier order is kept.
	RerankTimeout time.Duration

	NoGraph   bool
	NoRecency bool
	NoStale   bool

	Now func() time.Time
}

// Pipeline parameters. They were tuned on the dev split of
// testdata/corpus.json; see testdata/RESULTS.md.
const (
	defaultPool      = 30
	defaultRerankTop = 15
	defaultRerankTO  = 4 * time.Second
	graphSeeds       = 8
	// A neighbor enters the pool with this fraction of its seed's score...
	graphWeight = 0.5
	// ...and a candidate already in the pool gains this fraction.
	graphBoost = 0.1
	// Without temporal intent, session summaries lose up to this share of
	// their score with age (half-life episodicHalfLife).
	episodicAgeLoss  = 0.4
	episodicHalfLife = 30 * 24 * time.Hour
	// With temporal intent ("what did we do last time"), every memory decays
	// with this half-life.
	intentHalfLife = 7 * 24 * time.Hour
	// Memories about code that no longer exists, or whose symbol changed,
	// keep this share of their score (see store.CodeRef.Stale). After
	// reranking a score is a probability of relevance, so the factor is a
	// prior probability that the memory is still accurate: rarely when its
	// subject is gone, often when the code merely changed. These are not
	// fitted (the dev split has two stale memories); a stale memory is also
	// flagged to the agent.
	staleMissing = 0.05
	staleChanged = 0.6
)

// Retriever ranks memories for queries.
type Retriever struct {
	Store *store.Store
	// Embedder enables vector search; nil means full-text search only.
	Embedder store.Embedder
	// Reranker enables stage 3; nil skips it.
	Reranker Reranker
	Logger   *slog.Logger
	Options  Options
}

// Query is a search request.
type Query struct {
	Text  string
	Scope string // project/repo scope searched in addition to 'user'
	// AllScopes searches every scope (the owner's dashboard); Scope is ignored.
	AllScopes bool
	Types     []memory.Type // empty = all
	Limit     int           // default 10, max 50
}

type candidate struct {
	m        store.Memory
	first    float64 // first-stage score (0 if found only through the graph)
	graph    float64 // best seed score × graphWeight
	reranked bool
	score    float64
}

// Search returns memories relevant to q, best first. Scores are in (0, 1].
func (r *Retriever) Search(ctx context.Context, q Query) ([]store.Scored, error) {
	o := r.Options
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 50)
	pool := cmp.Or(o.Pool, defaultPool)
	pool = max(pool, limit)

	p := store.SearchParams{Query: q.Text, Scope: q.Scope, AllScopes: q.AllScopes, Types: q.Types, Limit: pool, MaxDistance: o.MaxDistance}
	if vec := r.embed(ctx, q.Text); vec != nil {
		p.Vector, p.Model = vec, r.Embedder.Model()
	}
	first, err := r.Store.Search(ctx, p)
	if err != nil {
		return nil, err
	}

	cands := make([]*candidate, 0, len(first))
	byID := make(map[string]*candidate, len(first))
	for _, s := range first {
		c := &candidate{m: s.Memory, first: s.Score}
		cands = append(cands, c)
		byID[s.ID] = c
	}

	reranked := false
	if r.Reranker != nil && len(cands) > 0 {
		if !o.NoGraph {
			cands = r.expand(ctx, q, cands, byID)
		}
		for _, c := range cands {
			c.score = c.first + graphBoost*c.graph
			if c.first == 0 {
				c.score = c.graph
			}
		}
		sortCandidates(cands)
		reranked = r.rerank(ctx, q.Text, cands)
	}
	if !reranked {
		// Without the reranker's judgment, graph neighbors are guesses on the
		// first stage's scale: drop them and their boosts.
		cands = slices.DeleteFunc(cands, func(c *candidate) bool { return c.first == 0 })
		for _, c := range cands {
			c.score = c.first
		}
	}
	if !o.NoStale {
		r.applyStaleness(ctx, cands)
	}
	if !o.NoRecency {
		now := time.Now()
		if o.Now != nil {
			now = o.Now()
		}
		intent := temporalIntent(q.Text)
		for _, c := range cands {
			c.score *= recencyFactor(c.m, now, intent, reranked)
		}
	}
	sortCandidates(cands)

	out := make([]store.Scored, 0, min(limit, len(cands)))
	for _, c := range cands[:min(limit, len(cands))] {
		out = append(out, store.Scored{Memory: c.m, Score: clampScore(c.score)})
	}
	return out, nil
}

// expand adds graph neighbors of the top candidates.
func (r *Retriever) expand(ctx context.Context, q Query, cands []*candidate, byID map[string]*candidate) []*candidate {
	seeds := make([]string, 0, graphSeeds)
	for _, c := range cands[:min(graphSeeds, len(cands))] {
		seeds = append(seeds, c.m.ID)
	}
	scopes := []string{"user"}
	if q.Scope != "" && q.Scope != "user" {
		scopes = append(scopes, q.Scope)
	}
	if q.AllScopes {
		scopes = nil
	}
	nbrs, err := r.Store.Neighbors(ctx, store.NeighborParams{Seeds: seeds, Scopes: scopes, Types: q.Types})
	if err != nil {
		r.logger().WarnContext(ctx, "graph expansion failed; ranking without it", "err", err)
		return cands
	}
	for _, n := range nbrs {
		seed := byID[n.Via]
		if seed == nil {
			continue
		}
		s := seed.first * graphWeight
		c := byID[n.ID]
		if c == nil {
			c = &candidate{m: n.Memory}
			cands = append(cands, c)
			byID[n.ID] = c
		}
		c.graph = max(c.graph, s)
	}
	return cands
}

// rerank rescores the best candidates with the cross-encoder and reports
// whether it did. Reranked candidates rank above the rest; on failure
// nothing changes.
func (r *Retriever) rerank(ctx context.Context, query string, cands []*candidate) bool {
	top := cands[:min(cmp.Or(r.Options.RerankTop, defaultRerankTop), len(cands))]
	docs := make([]string, len(top))
	for i, c := range top {
		docs[i] = c.m.Content
	}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(r.Options.RerankTimeout, defaultRerankTO))
	defer cancel()
	scores, err := r.Reranker.Rerank(ctx, query, docs)
	if err != nil || len(scores) != len(top) {
		r.logger().WarnContext(ctx, "rerank failed; using first-stage order", "err", err)
		return false
	}
	for i, c := range top {
		c.reranked, c.score = true, scores[i]
	}
	return true
}

// applyStaleness lowers the scores of memories whose code references are stale.
func (r *Retriever) applyStaleness(ctx context.Context, cands []*candidate) {
	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.m.ID
	}
	refs, err := r.Store.Refs(ctx, ids)
	if err != nil {
		r.logger().WarnContext(ctx, "code references unavailable; ranking without staleness", "err", err)
		return
	}
	for _, c := range cands {
		c.score *= staleFactor(refs[c.m.ID])
	}
}

// staleFactor is the share of its score a memory keeps given its references.
func staleFactor(refs []store.CodeRef) float64 {
	f := 1.0
	for _, ref := range refs {
		switch {
		case ref.State == store.RefMissing:
			f = min(f, staleMissing)
		case ref.Stale():
			f = min(f, staleChanged)
		}
	}
	return f
}

// sortCandidates orders reranked candidates first, then by score, then newest.
func sortCandidates(cands []*candidate) {
	slices.SortStableFunc(cands, func(a, b *candidate) int {
		if a.reranked != b.reranked {
			if a.reranked {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return b.m.CreatedAt.Compare(a.m.CreatedAt)
	})
}

// temporalRE matches queries about recent events, in English and Korean.
var temporalRE = regexp.MustCompile(`(?i)\b(last|latest|recent(ly)?|most recent|previous|yesterday|today|this (week|morning)|lately|just now)\b(?:[^-]|$)|최근|지난번|지난 ?세션|저번|마지막|어제|오늘|방금|요즘|최신|이번 ?주`)

func temporalIntent(q string) bool { return temporalRE.MatchString(q) }

// recencyFactor is the share of its score a memory keeps at its age. Session
// summaries age only when scores are calibrated (reranked); see the package
// comment.
func recencyFactor(m store.Memory, now time.Time, intent, calibrated bool) float64 {
	age := max(now.Sub(m.CreatedAt), 0)
	switch {
	case intent:
		return halfLife(age, intentHalfLife)
	case m.Type == memory.TypeEpisodic && calibrated:
		return 1 - episodicAgeLoss*(1-halfLife(age, episodicHalfLife))
	default:
		return 1
	}
}

func halfLife(age, h time.Duration) float64 {
	return math.Exp2(-float64(age) / float64(h))
}

func clampScore(s float64) float64 {
	switch {
	case math.IsNaN(s) || s <= 0:
		return 1e-6
	case s > 1:
		return 1
	}
	return s
}

// embed returns the query embedding, or nil when embeddings are disabled or
// the provider fails (logged, not returned: search degrades to full-text).
func (r *Retriever) embed(ctx context.Context, text string) []float32 {
	if r.Embedder == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	vecs, err := r.Embedder.Embed(ctx, []string{text})
	if err != nil || len(vecs) != 1 {
		r.logger().WarnContext(ctx, "embedding failed; using full-text search only", "err", err)
		return nil
	}
	return vecs[0]
}

func (r *Retriever) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}
