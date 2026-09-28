package retrieve

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

// keywordReranker scores documents by whether they contain a keyword.
type keywordReranker struct {
	keyword string
	err     error
	calls   int
	docs    int
}

func (k *keywordReranker) Model() string { return "fake" }
func (k *keywordReranker) Rerank(_ context.Context, _ string, docs []string) ([]float64, error) {
	k.calls++
	k.docs = len(docs)
	if k.err != nil {
		return nil, k.err
	}
	out := make([]float64, len(docs))
	for i, d := range docs {
		out[i] = 0.01
		if strings.Contains(d, k.keyword) {
			out[i] = 0.9
		}
	}
	return out, nil
}

// TestRetrieveIntegration checks the pipeline stages against Postgres with
// full-text search only (no embedder). It TRUNCATES memory.
func TestRetrieveIntegration(t *testing.T) {
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
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
		t.Fatal(err)
	}
	st := store.New(pool)
	const scope = "project:ex/retrieve"
	mk := func(typ memory.Type, content string) store.Memory {
		t.Helper()
		m, err := st.Create(ctx, store.CreateParams{Type: typ, Scope: scope, Content: content, SourceAgent: "t",
			Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	summary := mk(memory.TypeEpisodic, "Session summary: the login loop was caused by the cookie")
	fact := mk(memory.TypeCodebase, "Set Domain=.acme.dev on the session cookie so the dashboard can read it")
	if err := st.AddEdge(ctx, fact.ID, "derived_from", summary.ID, "t"); err != nil {
		t.Fatal(err)
	}
	stale := mk(memory.TypeCodebase, "Buttons use CSS module variants; the CSS module defines each variant")
	current := mk(memory.TypeCodebase, "Buttons use Tailwind variants in Button.tsx")
	if _, err := pool.Exec(ctx, `INSERT INTO memory_ref (memory_id, scope, path, state, anchor_hash, anchor_commit)
		VALUES ($1, $2, 'src/Button.module.css', 'missing', 'h', 'c')`, stale.ID, scope); err != nil {
		t.Fatal(err)
	}

	ids := func(res []store.Scored) []string {
		var out []string
		for _, r := range res {
			out = append(out, r.ID)
		}
		return out
	}
	search := func(r *Retriever, q string) []store.Scored {
		t.Helper()
		res, err := r.Search(ctx, Query{Text: q, Scope: scope, Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range res {
			if x.Score <= 0 || x.Score > 1 {
				t.Errorf("score %v out of (0, 1]", x.Score)
			}
		}
		return res
	}

	t.Run("graph expansion brings facts derived from a matching summary", func(t *testing.T) {
		// The fact does not match "login loop"; the reranker judges it the better answer.
		rr := &keywordReranker{keyword: "Domain"}
		got := ids(search(&Retriever{Store: st, Reranker: rr, Options: Options{NoGraph: true}}, "login loop"))
		if len(got) != 1 || got[0] != summary.ID {
			t.Fatalf("without graph = %v", got)
		}
		got = ids(search(&Retriever{Store: st, Reranker: rr}, "login loop"))
		if len(got) != 2 || got[0] != fact.ID || got[1] != summary.ID || rr.docs != 2 {
			t.Errorf("with graph = %v (reranked %d)", got, rr.docs)
		}
		// Without a reranker to judge them, neighbors are not added.
		if got := ids(search(&Retriever{Store: st}, "login loop")); len(got) != 1 {
			t.Errorf("graph without reranker = %v", got)
		}
		// Nor when the reranker fails.
		if got := ids(search(&Retriever{Store: st, Reranker: &keywordReranker{err: errors.New("down")}}, "login loop")); len(got) != 1 {
			t.Errorf("graph after rerank failure = %v", got)
		}
	})

	t.Run("stale code ranks lower", func(t *testing.T) {
		off := &Retriever{Store: st, Options: Options{NoStale: true, NoGraph: true}}
		// "CSS module" favors the stale memory lexically.
		if got := ids(search(off, "Buttons CSS module variants")); len(got) != 2 || got[0] != stale.ID {
			t.Fatalf("without staleness = %v", got)
		}
		rr := &keywordReranker{keyword: "Buttons"} // both match: staleness decides
		got := ids(search(&Retriever{Store: st, Reranker: rr, Options: Options{NoGraph: true}}, "Buttons CSS module variants"))
		if len(got) != 2 || got[0] != current.ID {
			t.Errorf("with staleness and a tie from the reranker = %v", got)
		}
	})

	t.Run("reranker reorders, and its failure keeps the first-stage order", func(t *testing.T) {
		rr := &keywordReranker{keyword: "Tailwind"}
		got := ids(search(&Retriever{Store: st, Reranker: rr, Options: Options{NoStale: true, NoGraph: true, RerankTop: 1}}, "Buttons CSS module variants"))
		// Only the top candidate is reranked (RerankTop 1): it stays first, the rest follow.
		if rr.calls != 1 || rr.docs != 1 || len(got) != 2 || got[0] != stale.ID {
			t.Errorf("rerank top 1 = %v (calls %d, docs %d)", got, rr.calls, rr.docs)
		}
		got = ids(search(&Retriever{Store: st, Reranker: rr, Options: Options{NoStale: true, NoGraph: true}}, "Buttons CSS module variants"))
		if got[0] != current.ID {
			t.Errorf("reranked = %v", got)
		}
		failing := &keywordReranker{err: errors.New("down")}
		got = ids(search(&Retriever{Store: st, Reranker: failing, Options: Options{NoStale: true, NoGraph: true}}, "Buttons CSS module variants"))
		if len(got) != 2 || got[0] != stale.ID {
			t.Errorf("after rerank failure = %v", got)
		}
	})

	t.Run("types and limits", func(t *testing.T) {
		res, err := (&Retriever{Store: st}).Search(ctx, Query{Text: "login loop cookie", Scope: scope, Types: []memory.Type{memory.TypeCodebase}, Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.Type != memory.TypeCodebase {
				t.Errorf("type filter leaked %s", r.Type)
			}
		}
		if res, err := (&Retriever{Store: st}).Search(ctx, Query{Text: "nothing matches zzzz", Scope: scope}); err != nil || len(res) != 0 {
			t.Errorf("no match = %v, %v", res, err)
		}
	})
}
