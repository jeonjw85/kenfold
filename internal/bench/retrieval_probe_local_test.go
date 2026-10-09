package bench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/embed"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/rerank"
	"github.com/kenfold/kenfold/internal/retrieve"
	"github.com/kenfold/kenfold/internal/store"
)

func probeQuestions(samples []LoCoMoSample, full bool) ([]scoredQ, error) {
	byType := map[string][]scoredQ{}
	for _, sample := range samples {
		scope, err := memory.Scope("bench-locomo-" + sample.ID)
		if err != nil {
			return nil, err
		}
		for _, q := range sample.Questions {
			q.When = sample.Present
			byType[q.Type] = append(byType[q.Type], scoredQ{Question: q, Scope: scope})
		}
	}
	var out []scoredQ
	for _, typ := range []string{"1", "2", "3", "4"} {
		qs := byType[typ]
		if !full {
			if len(qs) < 32 {
				return nil, errors.New("probe requires 32 questions per category")
			}
			slices.SortFunc(qs, func(a, b scoredQ) int {
				return strings.Compare(sha256Hex([]byte("quality-v1:"+a.ID)), sha256Hex([]byte("quality-v1:"+b.ID)))
			})
			qs = qs[:32]
		}
		out = append(out, qs...)
	}
	return out, nil
}

func probePendingQuestions(qs []scoredQ, ids []string) ([]scoredQ, error) {
	byID := make(map[string]scoredQ, len(qs))
	for _, q := range qs {
		byID[q.ID] = q
	}
	if len(ids) == 0 {
		return nil, errors.New("probe pending selection is empty")
	}
	var pending []scoredQ
	seen := map[string]bool{}
	for _, id := range ids {
		q, ok := byID[id]
		if !ok || seen[id] {
			return nil, errors.New("probe pending selection has duplicate or unknown IDs")
		}
		seen[id] = true
		pending = append(pending, q)
	}
	return pending, nil
}

// Opt-in local diagnostics: no Run/TestBench, ingestion, database creation,
// hosted models, reader, judge or budget. Only new private artifacts are written.
func TestLocalRetrievalQualityProbe(t *testing.T) {
	if os.Getenv("KENFOLD_RETRIEVAL_PROBE") != "1" {
		t.Skip("read-only local retrieval probe is opt-in")
	}
	ctx := context.Background()
	dataDir, outDir := os.Getenv("KENFOLD_PROBE_DATA"), os.Getenv("KENFOLD_PROBE_OUT")
	info, err := os.Lstat(outDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("probe output directory must already exist and be private, not a symlink")
	}
	for _, endpoint := range []string{os.Getenv("KENFOLD_PROBE_EMBED_URL"), os.Getenv("KENFOLD_PROBE_RERANK_URL")} {
		if err := probeModelURL(endpoint); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := newProbePool(ctx, os.Getenv("KENFOLD_PROBE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	samples, err := LoadLoCoMo(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := newLoCoMoTurnIndex(samples)
	if err != nil {
		t.Fatal(err)
	}
	full := os.Getenv("KENFOLD_PROBE_FULL") == "1"
	qs, err := probeQuestions(samples, full)
	if err != nil {
		t.Fatal(err)
	}
	if (!full && len(qs) != 128) || (full && len(qs) != 1540) {
		t.Fatal("unexpected frozen question scope")
	}
	variants := probeVariants()
	if full {
		chosen := os.Getenv("KENFOLD_PROBE_CANDIDATE")
		if chosen != "rerank30" && chosen != "no-recency" {
			t.Fatal("full retrieval confirmation requires one frozen candidate")
		}
		variants = slices.DeleteFunc(variants, func(v probeVariant) bool { return v.Name != "baseline" && v.Name != chosen })
	}
	if path := os.Getenv("KENFOLD_PROBE_PENDING_IDS"); path != "" {
		if !full {
			t.Fatal("resume requires the frozen full scope")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		if err := json.Unmarshal(data, &ids); err != nil {
			t.Fatal(err)
		}
		qs, err = probePendingQuestions(qs, ids)
		if err != nil {
			t.Fatal(err)
		}
		variants = slices.DeleteFunc(variants, func(v probeVariant) bool { return v.Name == "baseline" })
	}
	// Check every raw memory against frozen parser inputs, not only top-10 hits.
	rows, err := pool.Query(ctx, `SELECT scope,content,attrs,created_at,embedding_model,vector_dims(embedding) FROM memory WHERE scope LIKE 'project:bench-locomo-%' AND scope NOT LIKE 'project:bench-locomo-x-%' ORDER BY scope,id`)
	if err != nil {
		t.Fatal(err)
	}
	n, seen := 0, map[string]bool{}
	for rows.Next() {
		var scope, content, model string
		var attrs map[string]any
		var when time.Time
		var dim int
		if err := rows.Scan(&scope, &content, &attrs, &when, &model, &dim); err != nil {
			t.Fatal(err)
		}
		id, _ := attrs["bench_dia"].(string)
		found := false
		for _, turn := range index[scope][sessionOfDia(id)] {
			if turn.DiaID == id {
				found = content == clipBytes(turn.Content, maxContentBytes) && when.Equal(turn.When)
				break
			}
		}
		key := scope + ":" + id
		if !found || model != "bge-m3" || dim != store.EmbeddingDim || seen[key] {
			t.Fatal("DB raw source does not match frozen input/model")
		}
		seen[key] = true
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	expected := 0
	for _, s := range samples {
		expected += len(s.Turns)
	}
	if n != expected {
		t.Fatalf("raw turn count %d, want %d", n, expected)
	}
	var before, after string
	dbHash := `SELECT md5(string_agg(row_to_json(m)::text,'' ORDER BY id)) FROM memory m`
	if err := pool.QueryRow(ctx, dbHash).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if expected := os.Getenv("KENFOLD_PROBE_PENDING_DB_MD5"); expected != "" && before != expected {
		t.Fatal("resume DB no longer matches saved measurements")
	}
	// All arms reuse the same successful embeddings. Latency is warm/warm;
	// cold embedding calls have the same 10-second query deadline and own timing.
	httpClient := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{Proxy: nil}}
	emb, err := embed.New(embed.Config{BaseURL: os.Getenv("KENFOLD_PROBE_EMBED_URL"), Model: "kenfold-embed", Name: "bge-m3", Dim: store.EmbeddingDim, Timeout: 3 * time.Minute, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	rr, err := rerank.New(rerank.Config{BaseURL: os.Getenv("KENFOLD_PROBE_RERANK_URL"), Model: "bge-reranker-v2-m3", Timeout: time.Minute, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	cache := &probeEmbeddingCache{inner: emb, values: map[string][][]float32{}}
	out, err := os.OpenFile(filepath.Join(outDir, "retrieval.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	encoder := json.NewEncoder(out)
	write := func(v any) {
		t.Helper()
		if err := encoder.Encode(v); err != nil {
			t.Fatal(err)
		}
		if err := out.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, locomoFile))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(qs))
	for i, q := range qs {
		ids[i] = q.ID
	}
	write(map[string]any{"kind": "selection", "full": full, "ids": ids, "dataset_sha256": sha256Hex(raw), "db_before_md5": before, "raw_turns": n, "query_embed_deadline_seconds": 10, "rerank_deadline_seconds": 30, "latency": "frozen successful query embeddings; failed warmups stay degraded; cold timings separate", "paid_calls": 0})
	for _, q := range qs {
		began := time.Now()
		coldCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := cache.Embed(coldCtx, []string{q.Text})
		cancel()
		write(map[string]any{"kind": "cold-embedding", "id": q.ID, "elapsed_ms": float64(time.Since(began).Microseconds()) / 1000, "degraded": err != nil})
	}
	cache.Sealed = true
	for _, variant := range variants {
		write(map[string]any{"kind": "variant", "name": variant.Name, "pool": variant.Options.Pool, "rerank_top": variant.Options.RerankTop, "no_recency": variant.Options.NoRecency})
		ret := &retrieve.Retriever{Store: store.New(pool), Embedder: cache, Reranker: rr, Options: variant.Options}
		for i, q := range qs {
			hitsBefore := cache.Hits
			report, err := measureRetrieval(ctx, ret, []scoredQ{q})
			if err != nil {
				t.Fatal(err)
			}
			m := report.Questions[0]
			m.EmbeddingCacheHit = cache.Hits > hitsBefore
			var hits []store.Scored
			var contents []string
			for _, h := range m.Hits {
				hits = append(hits, store.Scored{Memory: store.Memory{ID: h.ID, Scope: h.Scope, Content: h.Content, CreatedAt: h.Date, Attrs: map[string]any{"bench_dia": h.DiaID}}, Score: h.Score})
				contents = append(contents, h.Content)
			}
			context, err := buildLoCoMoReaderContext(q.Scope, hits, index, 1, 20000)
			if err != nil {
				t.Fatal(err)
			}
			m.ContextRecall = evidenceRecall(q.Evidence, context.IDs)
			s, u, _ := readerMessages(q.Question, contents, true, "")
			m.LegacyInputBytes = len(s) + len(u)
			s, u, _ = readerMessages(q.Question, contents, true, "grounded-v2")
			m.GroundedInputBytes = len(s) + len(u)
			s, u, _ = readerMessages(q.Question, context.Contents, true, "grounded-v2")
			m.NeighborInputBytes = len(s) + len(u)
			write(map[string]any{"kind": "measurement", "variant": variant.Name, "measurement": m})
			if (i+1)%16 == 0 || i+1 == len(qs) {
				t.Logf("read-only %s %d/%d", variant.Name, i+1, len(qs))
			}
		}
	}
	if err := pool.QueryRow(ctx, dbHash).Scan(&after); err != nil || before != after {
		t.Fatal("protected DB state changed during probe")
	}
	write(map[string]any{"kind": "complete", "db_after_md5": after, "cache_hits": cache.Hits, "paid_calls": 0})
}
