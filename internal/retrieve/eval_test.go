package retrieve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/embed"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/rerank"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

type corpus struct {
	Memories []struct {
		Key         string `json:"key"`
		Project     string `json:"project"`
		Type        string `json:"type"`
		Agent       string `json:"agent"`
		AgeDays     int    `json:"age_days"`
		Status      string `json:"status"`
		Trust       string `json:"trust"`
		DerivedFrom string `json:"derived_from"`
		Content     string `json:"content"`
		Refs        []struct {
			Path   string `json:"path"`
			Symbol string `json:"symbol"`
			State  string `json:"state"`
		} `json:"refs"`
	} `json:"memories"`
	Queries []evalQuery `json:"queries"`
}

type evalQuery struct {
	ID       string   `json:"id"`
	Split    string   `json:"split"`
	Project  string   `json:"project"`
	Query    string   `json:"query"`
	Relevant []string `json:"relevant"`
}

// loaded is the corpus in a database: memory key <-> id.
type loaded struct {
	idOf  map[string]string
	keyOf map[string]string
}

// TestEvalRetrieval measures ranking quality on testdata/corpus.json with a
// real embedding model. It is opt-in and TRUNCATES the memory table:
//
//	KENFOLD_TEST_DATABASE_URL=postgres://.../kenfold_test \
//	KENFOLD_EVAL_EMBED_URL=http://127.0.0.1:11435/v1 KENFOLD_EVAL_EMBED_MODEL=kenfold-embed \
//	  go test -run TestEvalRetrieval -v ./internal/retrieve/
//
// It prints recall@5, hit@5 and MRR@10 per configuration and split, and the
// misses of the default configuration.
func TestEvalRetrieval(t *testing.T) {
	dbURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	embedURL := os.Getenv("KENFOLD_EVAL_EMBED_URL")
	if dbURL == "" || embedURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL and KENFOLD_EVAL_EMBED_URL not set")
	}
	ctx := context.Background()
	embedModel := envOr("KENFOLD_EVAL_EMBED_MODEL", "bge-m3")
	emb, err := embed.New(embed.Config{BaseURL: embedURL, Model: embedModel, Name: "bge-m3", Dim: store.EmbeddingDim, Timeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	st := store.New(pool)

	var c corpus
	raw, err := os.ReadFile("testdata/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	// Held-out queries (and the memories written with them) were added after
	// the pipeline was tuned on the dev queries, by an author without access
	// to the ranking code.
	if raw, err := os.ReadFile("testdata/holdout.json"); err == nil {
		var h corpus
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatal(err)
		}
		c.Memories = append(c.Memories, h.Memories...)
		c.Queries = append(c.Queries, h.Queries...)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	l := load(t, ctx, pool, st, emb, &c)

	var rr Reranker
	if u := os.Getenv("KENFOLD_EVAL_RERANK_URL"); u != "" {
		rc, err := rerank.New(rerank.Config{BaseURL: u, Model: envOr("KENFOLD_EVAL_RERANK_MODEL", "bge-reranker-v2-m3"), Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		rr = rc
	}
	configs := evalConfigs(st, emb, rr)
	rerankTop, _ := strconv.Atoi(os.Getenv("KENFOLD_EVAL_RERANK_TOP")) // 0 = default
	for _, cfg := range configs {
		cfg.r.Options.RerankTimeout = time.Minute // measure quality, not the production timeout
		cfg.r.Options.RerankTop = rerankTop
	}
	var report strings.Builder
	type agg struct {
		recall, hit, mrr float64
		n                int
	}
	results := map[string]map[string]*agg{} // config -> split -> agg
	var misses strings.Builder
	perQuery := map[string]float64{} // config|query -> recall@5
	latency := map[string]time.Duration{}
	for _, cfg := range configs {
		results[cfg.name] = map[string]*agg{}
		for _, q := range c.Queries {
			scope := "user"
			if q.Project != "" {
				s, err := memory.Scope(q.Project)
				if err != nil {
					t.Fatalf("%s: %v", q.ID, err)
				}
				scope = s
			}
			start := time.Now()
			res, err := cfg.r.Search(ctx, Query{Text: q.Query, Scope: scope, Limit: 10})
			if err != nil {
				t.Fatalf("%s/%s: %v", cfg.name, q.ID, err)
			}
			latency[cfg.name] += time.Since(start)
			ranked := make([]string, len(res))
			for i, r := range res {
				ranked[i] = l.keyOf[r.ID]
			}
			rec, hit, rr := score(ranked, q.Relevant)
			perQuery[cfg.name+"|"+q.ID] = rec
			for _, sp := range []string{q.Split, "all"} {
				a := results[cfg.name][sp]
				if a == nil {
					a = &agg{}
					results[cfg.name][sp] = a
				}
				a.recall += rec
				a.hit += hit
				a.mrr += rr
				a.n++
			}
			if cfg.name == configs[len(configs)-1].name && rec < 1 {
				fmt.Fprintf(&misses, "   %-26s [%s] recall %.2f  want %v\n      got %v\n", q.ID, q.Split, rec, q.Relevant, head(ranked, 6))
			}
		}
	}

	fmt.Fprintf(&report, "\n%-22s %-8s %5s %9s %6s %7s %9s\n", "config", "split", "n", "recall@5", "hit@5", "MRR@10", "ms/query")
	for _, cfg := range configs {
		for _, sp := range []string{"dev", "holdout", "all"} {
			a := results[cfg.name][sp]
			if a == nil || a.n == 0 {
				continue
			}
			n := float64(a.n)
			ms := ""
			if sp == "all" {
				ms = fmt.Sprintf("%.0f", float64(latency[cfg.name].Microseconds())/1000/n)
			}
			fmt.Fprintf(&report, "%-22s %-8s %5d %9.3f %6.3f %7.3f %9s\n", cfg.name, sp, a.n, a.recall/n, a.hit/n, a.mrr/n, ms)
		}
	}
	last := configs[len(configs)-1].name
	fmt.Fprintf(&report, "\nmisses of %s:\n%s", last, misses.String())
	fmt.Fprintf(&report, "\nqueries where %s is below hybrid (baseline):\n", last)
	for _, q := range c.Queries {
		if b, f := perQuery["hybrid (baseline)|"+q.ID], perQuery[last+"|"+q.ID]; f < b {
			fmt.Fprintf(&report, "   %-26s [%s] %.2f -> %.2f\n", q.ID, q.Split, b, f)
		}
	}
	t.Log(report.String())
}

type evalConfig struct {
	name string
	r    *Retriever
}

func evalConfigs(st *store.Store, emb store.Embedder, rr Reranker) []evalConfig {
	off := Options{NoGraph: true, NoRecency: true, NoStale: true}
	cfgs := []evalConfig{
		{"lexical", &Retriever{Store: st, Options: off}},
		{"lexical + signals", &Retriever{Store: st}}, // `make up`: no models
		{"hybrid (baseline)", &Retriever{Store: st, Embedder: emb, Options: off}},
		{"hybrid + signals", &Retriever{Store: st, Embedder: emb}}, // `make up-embed RERANK=0`
	}
	if rr != nil {
		cfgs = append(cfgs,
			evalConfig{"hybrid + rerank", &Retriever{Store: st, Embedder: emb, Reranker: rr, Options: off}},
			evalConfig{"full - graph", &Retriever{Store: st, Embedder: emb, Reranker: rr, Options: Options{NoGraph: true}}},
			evalConfig{"full - recency", &Retriever{Store: st, Embedder: emb, Reranker: rr, Options: Options{NoRecency: true}}},
			evalConfig{"full - stale", &Retriever{Store: st, Embedder: emb, Reranker: rr, Options: Options{NoStale: true}}},
			evalConfig{"full", &Retriever{Store: st, Embedder: emb, Reranker: rr}},
		)
	}
	return cfgs
}

// score returns recall@5 (relevant found in the top 5, over min(|relevant|, 5)),
// hit@5 (1 if any relevant is in the top 5) and the reciprocal rank of the
// first relevant memory within the top 10.
func score(ranked, relevant []string) (recall, hit, rr float64) {
	found := 0
	for i, k := range ranked {
		if !slices.Contains(relevant, k) {
			continue
		}
		if i < 5 {
			found++
		}
		if rr == 0 && i < 10 {
			rr = 1 / float64(i+1)
		}
	}
	if found > 0 {
		hit = 1
	}
	return float64(found) / float64(min(len(relevant), 5)), hit, rr
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func head(s []string, n int) []string { return s[:min(n, len(s))] }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// load writes the corpus into an empty memory table.
func load(t *testing.T, ctx context.Context, pool *pgxpool.Pool, st *store.Store, emb store.Embedder, c *corpus) loaded {
	t.Helper()
	if _, err := pool.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	texts := make([]string, len(c.Memories))
	for i := range c.Memories {
		m := &c.Memories[i]
		date := now.Add(-time.Duration(m.AgeDays) * 24 * time.Hour).UTC().Format("2006-01-02")
		m.Content = strings.ReplaceAll(m.Content, "{{DATE}}", date)
		texts[i] = m.Content
	}
	start := time.Now()
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		t.Fatalf("embed corpus: %v", err)
	}
	t.Logf("embedded %d memories in %.1fs", len(texts), time.Since(start).Seconds())

	l := loaded{idOf: map[string]string{}, keyOf: map[string]string{}}
	for i, m := range c.Memories {
		if _, dup := l.idOf[m.Key]; dup {
			t.Fatalf("duplicate key %s", m.Key)
		}
		scope := "user"
		if m.Project != "" {
			s, err := memory.Scope(m.Project)
			if err != nil {
				t.Fatal(err)
			}
			scope = s
		}
		status := memory.Status(m.Status)
		if status == "" {
			status = memory.StatusActive
		}
		trust := memory.Trust(m.Trust)
		if trust == "" {
			trust = memory.TrustAgent
		}
		row, err := st.Create(ctx, store.CreateParams{
			Type: memory.Type(m.Type), Scope: scope, Content: m.Content, SourceAgent: m.Agent,
			Trust: trust, Confidence: 0.5, Status: status,
			Embedding: vecs[i], EmbeddingModel: emb.Model(),
		})
		if err != nil {
			t.Fatalf("create %s: %v", m.Key, err)
		}
		created := now.Add(-time.Duration(m.AgeDays)*24*time.Hour - time.Duration(i)*time.Minute)
		if _, err := pool.Exec(ctx, `UPDATE memory SET created_at = $2, updated_at = $2 WHERE id = $1`, row.ID, created); err != nil {
			t.Fatal(err)
		}
		l.idOf[m.Key], l.keyOf[row.ID] = row.ID, m.Key
	}
	for _, m := range c.Memories {
		if m.DerivedFrom == "" {
			continue
		}
		dst, ok := l.idOf[m.DerivedFrom]
		if !ok {
			t.Fatalf("%s: derived_from %s not found", m.Key, m.DerivedFrom)
		}
		if err := st.AddEdge(ctx, l.idOf[m.Key], "derived_from", dst, "kenfold-extractor"); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range c.Memories {
		for _, r := range m.Refs {
			anchored := r.State != "" && r.State != store.RefPending && r.State != store.RefUnresolved
			if _, err := pool.Exec(ctx, `
				INSERT INTO memory_ref (memory_id, scope, path, symbol, state, anchor_hash, anchor_commit, checked_commit)
				SELECT id, scope, $2, $3, $4,
				       CASE WHEN $5 THEN 'h' END, CASE WHEN $5 THEN 'c1' END, CASE WHEN $5 THEN 'c2' END
				FROM memory WHERE id = $1`, l.idOf[m.Key], r.Path, r.Symbol, cmpOr(r.State, store.RefPending), anchored); err != nil {
				t.Fatalf("%s: ref: %v", m.Key, err)
			}
		}
	}
	for _, q := range c.Queries {
		for _, k := range q.Relevant {
			if _, ok := l.idOf[k]; !ok {
				t.Fatalf("query %s: relevant %s not in corpus", q.ID, k)
			}
		}
	}
	return l
}
