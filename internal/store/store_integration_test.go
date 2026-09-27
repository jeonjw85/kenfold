package store

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"os"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/migrations"
)

// fakeEmbedder is a deterministic bag-of-words embedder: texts that share words
// have high cosine similarity. It exercises the vector path without a model.
type fakeEmbedder struct{ model string }

func (f fakeEmbedder) Model() string { return f.model }

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = bagOfWords(t)
	}
	return out, nil
}

// pickyEmbedder fails any request that contains the text reject.
type pickyEmbedder struct {
	fakeEmbedder
	reject string
}

func (p pickyEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	for _, t := range texts {
		if t == p.reject {
			return nil, errors.New("provider rejected input")
		}
	}
	return p.fakeEmbedder.Embed(ctx, texts)
}

// downEmbedder always fails, like an unreachable provider.
type downEmbedder struct{ model string }

func (d downEmbedder) Model() string { return d.model }
func (d downEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("connection refused")
}

func bagOfWords(text string) []float32 {
	v := make([]float32, EmbeddingDim)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		h := fnv.New32a()
		h.Write([]byte(w))
		v[h.Sum32()%EmbeddingDim]++
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	if n == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / math.Sqrt(n))
	}
	return v
}

// TestStoreIntegration exercises the store against a real PostgreSQL+pgvector
// database. It is opt-in and TRUNCATES the memory table, so point it only at a
// throwaway database:
//
//	KENFOLD_TEST_DATABASE_URL=postgres://... go test -run Integration ./internal/store/
func TestStoreIntegration(t *testing.T) {
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, url); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
		t.Fatal(err)
	}

	s := New(pool)
	const agent = "store-test"
	create := func(t *testing.T, p CreateParams) Memory {
		t.Helper()
		if p.SourceAgent == "" {
			p.SourceAgent = agent
		}
		if p.Trust == "" {
			p.Trust = memory.TrustAgent
		}
		if p.Status == "" {
			p.Status = memory.StatusActive
		}
		if p.Confidence == 0 {
			p.Confidence = 0.5
		}
		m, err := s.Create(ctx, p)
		if err != nil {
			t.Fatalf("create %q: %v", p.Content, err)
		}
		return m
	}
	status := func(t *testing.T, id string) memory.Status {
		t.Helper()
		m, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return m.Status
	}

	t.Run("create and get", func(t *testing.T) {
		m := create(t, CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "Go generics landed in 1.18"})
		if !ValidID(m.ID) || m.Status != memory.StatusActive || m.CreatedAt.IsZero() || m.Attrs == nil || m.EmbeddingModel != nil {
			t.Errorf("unexpected row: %+v", m)
		}
		got, err := s.Get(ctx, m.ID)
		if err != nil || got.Content != m.Content {
			t.Errorf("get = %+v, %v", got, err)
		}
	})

	t.Run("invalid and unknown ids", func(t *testing.T) {
		for _, id := range []string{"", "abc", "not-a-uuid-at-all-but-36-characters", "0199aaaa-bbbb-cccc-dddd-eeeeffff000g"} {
			if _, err := s.Get(ctx, id); !errors.Is(err, ErrInvalidID) {
				t.Errorf("Get(%q) err = %v, want ErrInvalidID", id, err)
			}
			if _, err := s.SoftDelete(ctx, id, "", agent); !errors.Is(err, ErrInvalidID) {
				t.Errorf("SoftDelete(%q) err = %v, want ErrInvalidID", id, err)
			}
		}
		missing := "11111111-1111-1111-1111-111111111111"
		if _, err := s.Get(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Errorf("get err = %v, want ErrNotFound", err)
		}
		if _, err := s.SoftDelete(ctx, missing, "", agent); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete err = %v, want ErrNotFound", err)
		}
		if _, err := s.Approve(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Errorf("approve err = %v, want ErrNotFound", err)
		}
	})

	t.Run("temporary requires ttl", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		m := create(t, CreateParams{Type: memory.TypeTemporary, Scope: "user", Content: "WIP note", ExpiresAt: &exp})
		if m.ExpiresAt == nil {
			t.Error("expires_at not persisted")
		}
		_, err := s.Create(ctx, CreateParams{Type: memory.TypeTemporary, Scope: "user", Content: "no ttl",
			SourceAgent: agent, Trust: memory.TrustAgent, Status: memory.StatusActive})
		if err == nil {
			t.Error("temporary without ttl accepted")
		}
	})

	t.Run("embedding round-trip and validation", func(t *testing.T) {
		m := create(t, CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "embedded",
			Embedding: bagOfWords("embedded"), EmbeddingModel: "fake"})
		if m.EmbeddingModel == nil || *m.EmbeddingModel != "fake" {
			t.Errorf("embedding_model = %v", m.EmbeddingModel)
		}
		base := CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "x", SourceAgent: agent,
			Trust: memory.TrustAgent, Status: memory.StatusActive}
		bad := map[string]CreateParams{}
		p := base
		p.Embedding, p.EmbeddingModel = make([]float32, 3), "fake"
		bad["wrong dim"] = p
		p = base
		p.Embedding = bagOfWords("x")
		bad["missing model"] = p
		p = base
		v := bagOfWords("x")
		v[5] = float32(math.NaN())
		p.Embedding, p.EmbeddingModel = v, "fake"
		bad["NaN"] = p
		for name, p := range bad {
			if _, err := s.Create(ctx, p); err == nil {
				t.Errorf("%s: create succeeded", name)
			}
		}
	})

	t.Run("supersede", func(t *testing.T) {
		old := create(t, CreateParams{Type: memory.TypeProject, Scope: "project:ex/sup", Content: "We deploy on Fridays"})
		repl := create(t, CreateParams{Type: memory.TypeProject, Scope: "project:ex/sup", Content: "We never deploy on Fridays", Supersedes: &old.ID})
		if repl.Supersedes == nil || *repl.Supersedes != old.ID {
			t.Errorf("supersedes = %v", repl.Supersedes)
		}
		if got := status(t, old.ID); got != memory.StatusSuperseded {
			t.Errorf("old status = %s, want superseded", got)
		}
		// Superseding the already-superseded memory again must fail.
		_, err := s.Create(ctx, CreateParams{Type: memory.TypeProject, Scope: "project:ex/sup", Content: "third",
			SourceAgent: agent, Trust: memory.TrustAgent, Status: memory.StatusActive, Supersedes: &old.ID})
		if !errors.Is(err, ErrNotActive) {
			t.Errorf("re-supersede err = %v, want ErrNotActive", err)
		}
		missing, bad := "22222222-2222-2222-2222-222222222222", "nope"
		for id, want := range map[*string]error{&missing: ErrNotFound, &bad: ErrInvalidID} {
			_, err := s.Create(ctx, CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "x",
				SourceAgent: agent, Trust: memory.TrustAgent, Status: memory.StatusActive, Supersedes: id})
			if !errors.Is(err, want) {
				t.Errorf("supersede %q err = %v, want %v", *id, err, want)
			}
		}
	})

	t.Run("proposed supersede waits for approval", func(t *testing.T) {
		old := create(t, CreateParams{Type: memory.TypePreference, Scope: "user", Content: "Prefers npm"})
		prop := create(t, CreateParams{Type: memory.TypePreference, Scope: "user", Content: "Prefers pnpm",
			Status: memory.StatusProposed, Supersedes: &old.ID})
		if got := status(t, old.ID); got != memory.StatusActive {
			t.Fatalf("old status before approval = %s, want active", got)
		}
		approved, err := s.Approve(ctx, prop.ID)
		if err != nil || approved.Status != memory.StatusActive || approved.Trust != memory.TrustUser {
			t.Fatalf("approve = %s/%s, %v; want active, trust=user", approved.Status, approved.Trust, err)
		}
		if got := status(t, old.ID); got != memory.StatusSuperseded {
			t.Errorf("old status after approval = %s, want superseded", got)
		}
		if again, err := s.Approve(ctx, prop.ID); err != nil || again.Status != memory.StatusActive {
			t.Errorf("second approve = %s, %v; want idempotent", again.Status, err)
		}
		if _, err := s.Approve(ctx, old.ID); !errors.Is(err, ErrNotProposed) {
			t.Errorf("approve superseded err = %v, want ErrNotProposed", err)
		}
	})

	t.Run("soft delete is idempotent and keeps the first reason", func(t *testing.T) {
		m := create(t, CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "wrong fact"})
		d1, err := s.SoftDelete(ctx, m.ID, "outdated", "codex")
		if err != nil || d1.Status != memory.StatusDeleted {
			t.Fatalf("delete = %+v, %v", d1, err)
		}
		if d1.Attrs["forget_reason"] != "outdated" || d1.Attrs["forgotten_by"] != "codex" {
			t.Errorf("attrs = %v", d1.Attrs)
		}
		d2, err := s.SoftDelete(ctx, m.ID, "other", "claude-code")
		if err != nil || d2.Status != memory.StatusDeleted || d2.Attrs["forget_reason"] != "outdated" || d2.Attrs["forgotten_by"] != "codex" {
			t.Errorf("second delete = %+v, %v", d2.Attrs, err)
		}
		n := create(t, CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "no reason"})
		d3, err := s.SoftDelete(ctx, n.ID, "", "codex")
		if _, has := d3.Attrs["forget_reason"]; err != nil || has {
			t.Errorf("empty reason stored: %v, %v", d3.Attrs, err)
		}
	})

	t.Run("find duplicate", func(t *testing.T) {
		m := create(t, CreateParams{Type: memory.TypeProject, Scope: "project:ex/dup", Content: "Use pgx v5"})
		got, err := s.FindDuplicate(ctx, "project:ex/dup", memory.TypeProject, "Use pgx v5")
		if err != nil || got.ID != m.ID {
			t.Errorf("duplicate = %v, %v", got.ID, err)
		}
		for _, c := range []struct {
			scope string
			typ   memory.Type
		}{{"project:ex/other", memory.TypeProject}, {"project:ex/dup", memory.TypeSemantic}} {
			if _, err := s.FindDuplicate(ctx, c.scope, c.typ, "Use pgx v5"); !errors.Is(err, ErrNotFound) {
				t.Errorf("duplicate in %s/%s err = %v", c.scope, c.typ, err)
			}
		}
		if _, err := s.SoftDelete(ctx, m.ID, "", agent); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FindDuplicate(ctx, "project:ex/dup", memory.TypeProject, "Use pgx v5"); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleted memory counted as duplicate: %v", err)
		}
	})

	t.Run("handoff session", func(t *testing.T) {
		h, err := s.CreateHandoff(ctx, HandoffParams{Scope: "project:ex/hs", Summary: "s", SourceAgent: "codex", SourceSession: "sess-9", ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil || h.SourceSession == nil || *h.SourceSession != "sess-9" {
			t.Errorf("handoff session = %v, %v", h.SourceSession, err)
		}
	})

	t.Run("lexical search", func(t *testing.T) {
		scope := "project:ex/search"
		pipeline := create(t, CreateParams{Type: memory.TypeProject, Scope: scope, Content: "The deployment pipeline uses GitHub Actions and Docker"})
		create(t, CreateParams{Type: memory.TypeSemantic, Scope: scope, Content: "PostgreSQL full text search uses tsvector"})
		userWide := create(t, CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "Docker images should be pinned by digest"})
		other := create(t, CreateParams{Type: memory.TypeProject, Scope: "project:ex/elsewhere", Content: "Docker compose is used elsewhere"})
		past := time.Now().Add(-time.Minute)
		expired := create(t, CreateParams{Type: memory.TypeTemporary, Scope: scope, Content: "Docker temp note", ExpiresAt: &past})

		got, err := s.Search(ctx, SearchParams{Query: "docker", Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]float64{}
		for _, r := range got {
			ids[r.ID] = r.Score
			if r.Score <= 0 || r.Score > 1.0000001 {
				t.Errorf("score %v out of (0,1]", r.Score)
			}
		}
		if _, ok := ids[pipeline.ID]; !ok {
			t.Error("project memory not found")
		}
		if _, ok := ids[userWide.ID]; !ok {
			t.Error("user-wide memory not included")
		}
		if _, ok := ids[other.ID]; ok {
			t.Error("other project's memory leaked into results")
		}
		if _, ok := ids[expired.ID]; ok {
			t.Error("expired memory returned")
		}
		if len(got) > 0 && math.Abs(got[0].Score-1) > 1e-9 {
			t.Errorf("top lexical-only score = %v, want 1.0", got[0].Score)
		}

		nl, err := s.Search(ctx, SearchParams{Query: "how does the deployment pipeline work", Scope: scope})
		if err != nil || len(nl) == 0 || nl[0].ID != pipeline.ID {
			t.Errorf("natural-language query: %d results, err %v; want pipeline first", len(nl), err)
		}

		typed, err := s.Search(ctx, SearchParams{Query: "docker", Scope: scope, Types: []memory.Type{memory.TypeSemantic}})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range typed {
			if r.Type != memory.TypeSemantic {
				t.Errorf("type filter leaked %s", r.Type)
			}
		}

		for _, q := range []string{"", "???", "a&b | !c <-> (d)", "O'Reilly", "한국어 검색"} {
			if _, err := s.Search(ctx, SearchParams{Query: q, Scope: scope}); err != nil {
				t.Errorf("Search(%q) error: %v", q, err)
			}
		}
	})

	t.Run("hybrid search", func(t *testing.T) {
		scope := "project:ex/hybrid"
		emb := fakeEmbedder{model: "fake-v1"}
		mk := func(content, model string) Memory {
			return create(t, CreateParams{Type: memory.TypeProject, Scope: scope, Content: content,
				Embedding: bagOfWords(content), EmbeddingModel: model})
		}
		target := mk("alpha beta gamma delta", "fake-v1")
		otherModel := mk("alpha beta gamma epsilon", "fake-v0")
		mk("completely unrelated words here", "fake-v1")

		// No lexical overlap with the query text, but the vector matches.
		qv, _ := emb.Embed(ctx, []string{"alpha beta gamma"})
		got, err := s.Search(ctx, SearchParams{Query: "zzz", Scope: scope, Vector: qv[0], Model: "fake-v1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != target.ID {
			t.Fatalf("vector-only search = %v; want only the fake-v1 target", contents(got))
		}
		for _, r := range got {
			if r.ID == otherModel.ID {
				t.Error("memory embedded by another model was compared")
			}
		}
		// First in both rankings scores exactly 1.0. (For "alpha beta gamma" the two
		// alpha memories tie on ts_rank and the newer one wins the tie.)
		qv2, _ := emb.Embed(ctx, []string{"alpha beta gamma delta"})
		both, err := s.Search(ctx, SearchParams{Query: "alpha beta gamma delta", Scope: scope, Vector: qv2[0], Model: "fake-v1"})
		if err != nil || len(both) == 0 || both[0].ID != target.ID || math.Abs(both[0].Score-1) > 1e-9 {
			t.Errorf("hybrid top = %v (score %v), err %v; want target with score 1.0", contents(both), scoreOf(both), err)
		}
		// A distant vector is cut off by MaxDistance.
		far, _ := emb.Embed(ctx, []string{"nothing in common"})
		none, err := s.Search(ctx, SearchParams{Query: "zzz", Scope: scope, Vector: far[0], Model: "fake-v1", MaxDistance: 0.3})
		if err != nil || len(none) != 0 {
			t.Errorf("far vector returned %v, %v", contents(none), err)
		}
		if _, err := s.Search(ctx, SearchParams{Query: "q", Vector: qv[0]}); err == nil {
			t.Error("vector without model accepted")
		}
	})

	t.Run("list", func(t *testing.T) {
		scope := "project:ex/list"
		a := create(t, CreateParams{Type: memory.TypeProject, Scope: scope, Content: "first"})
		b := create(t, CreateParams{Type: memory.TypeProject, Scope: scope, Content: "second"})
		p := create(t, CreateParams{Type: memory.TypePreference, Scope: scope, Content: "proposed pref", Status: memory.StatusProposed})
		got, err := s.List(ctx, ListParams{Scopes: []string{scope}, Types: []memory.Type{memory.TypeProject}})
		if err != nil || len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
			t.Errorf("list = %v, %v; want [second first]", contentsM(got), err)
		}
		props, err := s.List(ctx, ListParams{Scopes: []string{scope}, Statuses: []memory.Status{memory.StatusProposed}})
		if err != nil || len(props) != 1 || props[0].ID != p.ID {
			t.Errorf("proposed list = %v, %v", contentsM(props), err)
		}
	})

	t.Run("handoff lifecycle", func(t *testing.T) {
		scope := "project:ex/handoff"
		if _, err := s.LatestHandoff(ctx, scope, false); !errors.Is(err, ErrNotFound) {
			t.Errorf("empty scope err = %v", err)
		}
		exp := time.Now().Add(time.Hour)
		h1, err := s.CreateHandoff(ctx, HandoffParams{Scope: scope, Summary: "first", SourceAgent: "claude-code", ExpiresAt: exp})
		if err != nil {
			t.Fatal(err)
		}
		h2, err := s.CreateHandoff(ctx, HandoffParams{Scope: scope, Summary: "second", NextSteps: []string{"a", "b"}, SourceAgent: "claude-code", ExpiresAt: exp})
		if err != nil {
			t.Fatal(err)
		}
		if got := status(t, h1.ID); got != memory.StatusSuperseded {
			t.Errorf("older handoff status = %s, want superseded", got)
		}
		if h2.Supersedes == nil || *h2.Supersedes != h1.ID {
			t.Errorf("new handoff supersedes = %v, want %s", h2.Supersedes, h1.ID)
		}
		steps, _ := h2.Attrs["next_steps"].([]any)
		if len(steps) != 2 || steps[0] != "a" || h2.Attrs["kind"] != "handoff" {
			t.Errorf("attrs = %v", h2.Attrs)
		}

		pending, err := s.LatestHandoff(ctx, scope, true)
		if err != nil || pending.ID != h2.ID {
			t.Fatalf("pending = %v, %v", pending.ID, err)
		}
		r1, err := s.MarkResumed(ctx, h2.ID, "codex")
		if err != nil || r1.Attrs["resumed_by"] != "codex" || r1.Attrs["resumed_at"] == nil {
			t.Fatalf("mark resumed = %v, %v", r1.Attrs, err)
		}
		r2, err := s.MarkResumed(ctx, h2.ID, "opencode")
		if err != nil || r2.Attrs["resumed_by"] != "codex" {
			t.Errorf("second resume overwrote first: %v, %v", r2.Attrs, err)
		}
		if _, err := s.LatestHandoff(ctx, scope, true); !errors.Is(err, ErrNotFound) {
			t.Errorf("resumed handoff still pending: %v", err)
		}
		if latest, err := s.LatestHandoff(ctx, scope, false); err != nil || latest.ID != h2.ID {
			t.Errorf("latest (incl. resumed) = %v, %v", latest.ID, err)
		}

		past := time.Now().Add(-time.Minute)
		if _, err := s.CreateHandoff(ctx, HandoffParams{Scope: "project:ex/expired", Summary: "old", SourceAgent: "x", ExpiresAt: past}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.LatestHandoff(ctx, "project:ex/expired", false); !errors.Is(err, ErrNotFound) {
			t.Errorf("expired handoff returned: %v", err)
		}
	})

	t.Run("normalized duplicate and session", func(t *testing.T) {
		m := create(t, CreateParams{Type: memory.TypeProject, Scope: "project:ex/norm", Content: "We use pgx v5.", SourceSession: "sess-1"})
		if m.SourceSession == nil || *m.SourceSession != "sess-1" {
			t.Errorf("source_session = %v", m.SourceSession)
		}
		for _, variant := range []string{"we use PGX v5", "  We  use pgx\tv5!! ", "We use pgx v5"} {
			got, err := s.FindDuplicate(ctx, "project:ex/norm", memory.TypeProject, variant)
			if err != nil || got.ID != m.ID {
				t.Errorf("FindDuplicate(%q) = %v, %v", variant, got.ID, err)
			}
		}
		if _, err := s.FindDuplicate(ctx, "project:ex/norm", memory.TypeProject, "We use pgx v4"); !errors.Is(err, ErrNotFound) {
			t.Errorf("different fact matched: %v", err)
		}
		if got, err := s.FindDuplicate(ctx, "project:ex/norm", "", "we use pgx v5"); err != nil || got.ID != m.ID {
			t.Errorf("any-type duplicate = %v, %v", got.ID, err)
		}
		n := create(t, CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "no session"})
		if n.SourceSession != nil {
			t.Errorf("empty session stored as %q", *n.SourceSession)
		}
	})

	t.Run("similar", func(t *testing.T) {
		scope := "project:ex/similar"
		fri := create(t, CreateParams{Type: memory.TypeProject, Scope: scope, Content: "We deploy on Fridays after the release review",
			Embedding: bagOfWords("We deploy on Fridays after the release review"), EmbeddingModel: "fake-v1"})
		create(t, CreateParams{Type: memory.TypeProject, Scope: scope, Content: "Integration tests need the compose Postgres",
			Embedding: bagOfWords("Integration tests need the compose Postgres"), EmbeddingModel: "fake-v1"})
		other := create(t, CreateParams{Type: memory.TypeProject, Scope: "project:ex/elsewhere2", Content: "We deploy on Fridays after the release review"})

		// Lexical path only (no vector).
		got, err := s.Similar(ctx, SimilarParams{Scope: scope, Content: "We never deploy on Fridays after the release review"})
		if err != nil || len(got) != 1 || got[0].ID != fri.ID || got[0].Score < 0.5 {
			t.Fatalf("lexical similar = %v (score %v), %v", contents(got), scoreOf(got), err)
		}
		for _, r := range got {
			if r.ID == other.ID {
				t.Error("similar leaked another scope")
			}
		}
		// Vector path, and Exclude.
		q := "deploy fridays release review"
		got, err = s.Similar(ctx, SimilarParams{Scope: scope, Content: q, Vector: bagOfWords(q), Model: "fake-v1", MaxDistance: 0.5})
		if err != nil || len(got) == 0 || got[0].ID != fri.ID {
			t.Errorf("vector similar = %v, %v", contents(got), err)
		}
		got, err = s.Similar(ctx, SimilarParams{Scope: scope, Content: fri.Content, Exclude: fri.ID})
		if err != nil || len(got) != 0 {
			t.Errorf("exclude failed: %v, %v", contents(got), err)
		}
		if _, err := s.Similar(ctx, SimilarParams{Scope: scope, Content: "x", Exclude: "bad"}); !errors.Is(err, ErrInvalidID) {
			t.Errorf("bad exclude err = %v", err)
		}
		// Unrelated content yields nothing.
		got, err = s.Similar(ctx, SimilarParams{Scope: scope, Content: "Kubernetes ingress TLS renewal"})
		if err != nil || len(got) != 0 {
			t.Errorf("unrelated similar = %v, %v", contents(got), err)
		}
	})

	t.Run("recent episodes", func(t *testing.T) {
		scope := "project:ex/episodes"
		for _, c := range []string{"session one", "session two", "session three"} {
			create(t, CreateParams{Type: memory.TypeEpisodic, Scope: scope, Content: c})
		}
		create(t, CreateParams{Type: memory.TypeProject, Scope: scope, Content: "not an episode"})
		got, err := s.RecentEpisodes(ctx, scope, 2)
		if err != nil || len(got) != 2 || got[0].Content != "session three" || got[1].Content != "session two" {
			t.Errorf("recent = %v, %v", contentsM(got), err)
		}
	})

	t.Run("for each and replace content", func(t *testing.T) {
		m := create(t, CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "contains a secret value",
			Embedding: bagOfWords("contains a secret value"), EmbeddingModel: "fake-v1"})
		seen := 0
		if err := s.ForEach(ctx, []memory.Status{memory.StatusActive}, func(x Memory) error {
			if x.Status != memory.StatusActive {
				t.Errorf("ForEach returned %s", x.Status)
			}
			seen++
			return nil
		}); err != nil || seen == 0 {
			t.Fatalf("ForEach seen %d, %v", seen, err)
		}
		stop := errors.New("stop")
		if err := s.ForEach(ctx, nil, func(Memory) error { return stop }); !errors.Is(err, stop) {
			t.Errorf("ForEach did not propagate error: %v", err)
		}
		r, err := s.Rewrite(ctx, m.ID, "contains a [REDACTED:x] value", map[string]any{"next_steps": []string{"[REDACTED:x]"}}, "secret scan")
		if err != nil || r.Content != "contains a [REDACTED:x] value" || r.EmbeddingModel != nil || r.Attrs["redact_reason"] != "secret scan" || r.Attrs["redacted_at"] == nil {
			t.Errorf("rewrite = %+v, %v", r, err)
		}
		if steps, _ := r.Attrs["next_steps"].([]any); len(steps) != 1 || steps[0] != "[REDACTED:x]" {
			t.Errorf("rewrite attrs = %v", r.Attrs)
		}
		if _, err := s.Rewrite(ctx, m.ID, "  ", nil, "x"); err == nil {
			t.Error("empty replacement accepted")
		}
		if _, err := s.Rewrite(ctx, "11111111-1111-1111-1111-111111111111", "x", nil, "x"); !errors.Is(err, ErrNotFound) {
			t.Errorf("rewrite missing err = %v", err)
		}
	})

	t.Run("find session memory", func(t *testing.T) {
		scope := "project:ex/sessions"
		create(t, CreateParams{Type: memory.TypeEpisodic, Scope: scope, Content: "first summary", SourceAgent: "codex", SourceSession: "s-1"})
		second := create(t, CreateParams{Type: memory.TypeEpisodic, Scope: scope, Content: "second summary", SourceAgent: "codex", SourceSession: "s-1"})
		got, err := s.FindSessionMemory(ctx, scope, memory.TypeEpisodic, "codex", "s-1")
		if err != nil || got.ID != second.ID {
			t.Errorf("session memory = %v, %v", got.Content, err)
		}
		for _, c := range []struct{ agent, session string }{{"claude-code", "s-1"}, {"codex", "s-2"}} {
			if _, err := s.FindSessionMemory(ctx, scope, memory.TypeEpisodic, c.agent, c.session); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s/%s matched: %v", c.agent, c.session, err)
			}
		}
	})

	t.Run("backfill", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
			t.Fatal(err)
		}
		for _, c := range []string{"one", "two", "three"} {
			create(t, CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: c})
		}
		create(t, CreateParams{Type: memory.TypePreference, Scope: "user", Content: "proposed", Status: memory.StatusProposed})
		gone := create(t, CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "deleted"})
		if _, err := s.SoftDelete(ctx, gone.ID, "", agent); err != nil {
			t.Fatal(err)
		}

		st, err := s.Backfill(ctx, fakeEmbedder{model: "fake-v1"}, 2) // batch < rows exercises paging
		if err != nil || st.Embedded != 4 || st.Skipped != 0 {
			t.Fatalf("backfill = %+v, %v; want 4 embedded (active + proposed, not deleted)", st, err)
		}
		if st, err := s.Backfill(ctx, fakeEmbedder{model: "fake-v1"}, 2); err != nil || st.Embedded != 0 {
			t.Errorf("second backfill = %+v, %v; want 0", st, err)
		}
		if st, err := s.Backfill(ctx, fakeEmbedder{model: "fake-v2"}, 100); err != nil || st.Embedded != 4 {
			t.Errorf("backfill after model change = %+v, %v; want 4", st, err)
		}
		got, err := s.Search(ctx, SearchParams{Query: "zzz", Vector: bagOfWords("two"), Model: "fake-v2"})
		if err != nil || len(got) != 1 || got[0].Content != "two" {
			t.Errorf("search after backfill = %v, %v", contents(got), err)
		}

		// A provider that rejects one input must not block the others.
		picky := pickyEmbedder{fakeEmbedder{model: "fake-v3"}, "three"}
		if st, err := s.Backfill(ctx, picky, 100); err != nil || st.Embedded != 3 || st.Skipped != 1 {
			t.Errorf("picky backfill = %+v, %v; want 3 embedded, 1 skipped", st, err)
		}
		// A provider that is down fails the run without touching rows.
		if _, err := s.Backfill(ctx, downEmbedder{"fake-v4"}, 100); err == nil {
			t.Error("backfill with a down provider returned no error")
		}
	})
}

func contents(rs []Scored) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Content
	}
	return out
}

func contentsM(ms []Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Content
	}
	return out
}

func scoreOf(rs []Scored) float64 {
	if len(rs) == 0 {
		return 0
	}
	return rs[0].Score
}
