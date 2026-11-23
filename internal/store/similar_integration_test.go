package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/migrations"
)

// similarTestStore opens the throwaway integration database and clears memory.
func similarTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
		t.Fatal(err)
	}
	return New(pool), pool
}

// TestSimilarUsesHNSWIndexIntegration checks that the vector branch is written
// so the HNSW index can serve it. The previous form filtered with a distance
// range predicate, which no HNSW plan can use: every remember scanned every
// embedding in the scope.
func TestSimilarUsesHNSWIndexIntegration(t *testing.T) {
	s, pool := similarTestStore(t)
	ctx := context.Background()
	scope := "project:ex/similar-plan"
	for i := range 120 {
		text := fmt.Sprintf("plan fixture number %d about deployment reviews", i)
		if _, err := s.Create(ctx, CreateParams{Type: memory.TypeProject, Scope: scope, Content: text,
			SourceAgent: "test", Trust: memory.TrustAgent, Status: memory.StatusActive,
			Embedding: bagOfWords(text), EmbeddingModel: "plan-v1"}); err != nil {
			t.Fatal(err)
		}
	}
	const q = "deployment reviews"
	args, hasVector, err := similarArgs(SimilarParams{Scope: scope, Content: q, Vector: bagOfWords(q), Model: "plan-v1"})
	if err != nil || !hasVector {
		t.Fatalf("similarArgs: hasVector=%v err=%v", hasVector, err)
	}

	var plan []string
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Price the alternatives out: a sequential scan or a sort cannot be
		// the answer if the distance ordering is index-usable. The previous
		// query shape (a range predicate) had no plan that used the index.
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET LOCAL enable_sort = off`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `EXPLAIN (COSTS OFF) `+similarSQL, args...)
		if err != nil {
			return err
		}
		plan, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(plan, "\n"); !strings.Contains(joined, "memory_embedding_hnsw_idx") {
		t.Fatalf("the vector branch does not use the HNSW index:\n%s", joined)
	}
}

// TestSimilarLargeScopeStillFindsTheNearestIntegration checks that the
// index-ordered candidate list keeps the exact top match, respects the scope
// and embedding model, and honors Exclude.
func TestSimilarLargeScopeStillFindsTheNearestIntegration(t *testing.T) {
	s, _ := similarTestStore(t)
	ctx := context.Background()
	scope := "project:ex/similar-nearest"
	create := func(scope, content, model string, vec []float32) Memory {
		t.Helper()
		m, err := s.Create(ctx, CreateParams{Type: memory.TypeProject, Scope: scope, Content: content,
			SourceAgent: "test", Trust: memory.TrustAgent, Status: memory.StatusActive,
			Embedding: vec, EmbeddingModel: model})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	const target = "The release checklist lives in docs/release.md"
	vec := bagOfWords(target)
	nearest := create(scope, target, "near-v1", vec)
	for i := range 300 {
		text := fmt.Sprintf("unrelated note number %d about logging and retries", i)
		create(scope, text, "near-v1", bagOfWords(text))
	}
	// Same vector, but outside the scope and under another embedding model.
	create("project:ex/similar-elsewhere", target, "near-v1", vec)
	create(scope, target+" (other model)", "near-v2", vec)

	got, err := s.Similar(ctx, SimilarParams{Scope: scope, Content: target, Vector: vec, Model: "near-v1",
		MaxDistance: 0.05, MinTrigram: 0.9, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].ID != nearest.ID {
		t.Fatalf("nearest = %v (score %v), want %s", contents(got), scoreOf(got), nearest.Content)
	}
	for _, r := range got[1:] {
		if r.ID == nearest.ID {
			t.Errorf("excluded target appeared twice")
		}
	}

	got, err = s.Similar(ctx, SimilarParams{Scope: scope, Content: target, Vector: vec, Model: "near-v1",
		MaxDistance: 0.05, MinTrigram: 0.9, Limit: 5, Exclude: nearest.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.ID == nearest.ID {
			t.Errorf("Exclude returned the excluded memory")
		}
	}
}
