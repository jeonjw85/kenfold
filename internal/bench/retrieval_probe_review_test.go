package bench

import (
	"context"
	"errors"
	"testing"

	"github.com/kenfold/kenfold/internal/retrieve"
	"github.com/kenfold/kenfold/internal/store"
)

type lateProbeEmbedding struct {
	store.Embedder
	calls int
}

func (e *lateProbeEmbedding) Endpoint() string {
	return e.Embedder.(interface{ Endpoint() string }).Endpoint()
}

func (e *lateProbeEmbedding) Embed(ctx context.Context, in []string) ([][]float32, error) {
	e.calls++
	if e.calls <= 2 {
		return nil, errors.New("fixture warmup failure")
	}
	return e.Embedder.Embed(ctx, in)
}

func TestRetrievalProbeSealedWarmupFailureDoesNotChangeArms(t *testing.T) {
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	inner := &lateProbeEmbedding{Embedder: d.Embedder}
	cache := &probeEmbeddingCache{inner: inner, values: map[string][][]float32{}}
	if _, err := cache.Embed(context.Background(), []string{qs[0].Text}); err == nil {
		t.Fatal("fixture did not fail initial warmup")
	}
	cache.Sealed = true
	for _, variant := range probeVariants() {
		ret := &retrieve.Retriever{Store: d.Store, Embedder: cache, Options: variant.Options}
		report, err := measureRetrieval(context.Background(), ret, qs)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Questions[0].Degraded {
			t.Errorf("%s retried a failed warmup and changed vector availability", variant.Name)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("sealed outcomes made additional requests: %d", inner.calls)
	}
	if f.reader.Load() != 0 || f.judge.Load() != 0 {
		t.Fatal("probe called a completion")
	}
}
