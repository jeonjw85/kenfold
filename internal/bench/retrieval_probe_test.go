package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kenfold/kenfold/internal/retrieve"
	"github.com/kenfold/kenfold/internal/store"
)

type retrievalMeasurement struct {
	ID, Scope, Type, When, QuerySHA256 string
	Hits                               []scoreInputHit
	Stages                             []map[string]any
	Degraded                           bool
	ElapsedMS                          float64
	Recall5, Recall10, PoolRecall      float64
	EmbeddingCacheHit                  bool
	ContextRecall                      float64
	LegacyInputBytes                   int
	GroundedInputBytes                 int
	NeighborInputBytes                 int
}
type retrievalReport struct{ Questions []retrievalMeasurement }

func newProbePool(ctx context.Context, rawURL string) (*pgxpool.Pool, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || !net.ParseIP(u.Hostname()).IsLoopback() || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("probe requires a credential-free loopback DB URL")
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			return nil, errors.New("probe DB passwords must use private PGPASSFILE")
		}
	}
	cfg, err := pgxpool.ParseConfig(rawURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["application_name"] = "kenfold-read-only-retrieval-probe"
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var mode string
	if err = pool.QueryRow(ctx, "SHOW transaction_read_only").Scan(&mode); err != nil || mode != "on" {
		pool.Close()
		return nil, errors.New("probe requires DB-enforced read-only connections")
	}
	return pool, nil
}
func probeModelURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("probe models require credential-free numeric loopback HTTP URLs")
	}
	return nil
}

type probeEmbeddingCache struct {
	inner  store.Embedder
	values map[string][][]float32
	Hits   int
	Sealed bool
}

func (e *probeEmbeddingCache) Model() string { return e.inner.Model() }
func (e *probeEmbeddingCache) Endpoint() string {
	return e.inner.(interface{ Endpoint() string }).Endpoint()
}
func (e *probeEmbeddingCache) Embed(ctx context.Context, in []string) ([][]float32, error) {
	b, _ := json.Marshal([]any{e.Model(), e.Endpoint(), in})
	key := sha256Hex(b)
	if values, ok := e.values[key]; ok {
		e.Hits++
		return cloneProbeVectors(values), nil
	}
	if e.Sealed {
		return nil, errors.New("frozen query embedding unavailable")
	}
	values, err := e.inner.Embed(ctx, in)
	if err == nil && len(values) == len(in) {
		if e.values == nil {
			e.values = map[string][][]float32{}
		}
		e.values[key] = cloneProbeVectors(values)
	}
	return values, err
}

func cloneProbeVectors(values [][]float32) [][]float32 {
	out := make([][]float32, len(values))
	for i, v := range values {
		out[i] = append([]float32(nil), v...)
	}
	return out
}

type probeVariant struct {
	Name    string
	Options retrieve.Options
}

func probeVariants() []probeVariant {
	b := retrieve.Options{Pool: 30, RerankTop: 15, RerankTimeout: 30 * time.Second}
	r, a := b, b
	r.RerankTop = 30
	a.NoRecency = true
	return []probeVariant{{"baseline", b}, {"rerank30", r}, {"no-recency", a}}
}

// Retrieval only: no Run, ingestion, health completion, reader, judge or budget.
// Callers construct the Store with newProbePool (DB-enforced read-only).
func measureRetrieval(ctx context.Context, ret *retrieve.Retriever, qs []scoredQ) (retrievalReport, error) {
	var out retrievalReport
	for _, model := range []any{ret.Embedder, ret.Reranker} {
		if model == nil {
			continue
		}
		endpoint, ok := model.(interface{ Endpoint() string })
		if !ok {
			return out, errors.New("probe model must expose its local endpoint")
		}
		if err := probeModelURL(endpoint.Endpoint()); err != nil {
			return out, err
		}
	}
	for _, q := range qs {
		if q.When.IsZero() {
			return out, errors.New("probe requires an explicit retrieval reference time")
		}
		m := retrievalMeasurement{ID: q.ID, Scope: q.Scope, Type: q.Type, When: q.When.UTC().Format(time.RFC3339Nano), QuerySHA256: sha256Hex([]byte(q.Text)), PoolRecall: -1}
		copyRet := *ret
		copyRet.Options.Now = func() time.Time { return q.When }
		copyRet.Logger = newBenchDiagnosticLogger(func(_ string, args ...any) {
			var event map[string]any
			if len(args) == 1 {
				if b, ok := args[0].([]byte); ok && json.Unmarshal(b, &event) == nil {
					m.Stages = append(m.Stages, event)
					if event["degraded"] == true {
						m.Degraded = true
					}
				}
			}
		})
		started := time.Now()
		hits, err := copyRet.Search(ctx, retrieve.Query{Text: q.Text, Scope: q.Scope, Limit: topK})
		if err != nil {
			return out, err
		}
		m.ElapsedMS = float64(time.Since(started).Microseconds()) / 1000
		snapshot := newScoreInputSnapshot(hits, nil, "", "", AnswerOptions{})
		m.Hits = snapshot.Hits
		var ids []string
		for _, h := range m.Hits {
			ids = append(ids, probeEvidenceID(h.DiaID, h.Session))
		}
		m.Recall5, m.Recall10 = evidenceRecall(q.Evidence, ids[:min(5, len(ids))]), evidenceRecall(q.Evidence, ids)
		for _, event := range m.Stages {
			if event["stage"] != "first-stage" {
				continue
			}
			var poolIDs []string
			for _, id := range event["ids"].([]any) {
				memory, err := ret.Store.Get(ctx, id.(string))
				if err != nil {
					return out, fmt.Errorf("probe candidate: %w", err)
				}
				poolIDs = append(poolIDs, probeEvidenceID(attrString(memory, "bench_dia"), attrString(memory, "bench_session")))
			}
			m.PoolRecall = evidenceRecall(q.Evidence, poolIDs)
		}
		out.Questions = append(out.Questions, m)
	}
	return out, nil
}

func probeEvidenceID(dia, session string) string {
	if dia != "" {
		return dia
	}
	return session
}
