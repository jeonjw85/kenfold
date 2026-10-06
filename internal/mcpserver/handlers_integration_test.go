package mcpserver

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

func integrationHandlers(t *testing.T, e store.Embedder) (*handlers, *pgxpool.Pool) {
	t.Helper()
	u := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, u); err != nil {
		t.Fatal(err)
	}
	p, err := pgxpool.New(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if _, err := p.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
		t.Fatal(err)
	}
	return newHandlers(Deps{Store: store.New(p), Embedder: e, Agent: "test"}), p
}

// Both requests finish their initial duplicate lookup before either can write.
type barrierEmbedder struct{ reached, release chan struct{} }

func (e *barrierEmbedder) Model() string { return "test" }
func (e *barrierEmbedder) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	select {
	case e.reached <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	v := make([]float32, store.EmbeddingDim)
	v[0] = 1
	return [][]float32{v}, nil
}

type alternatingClassifier struct{ n atomic.Int32 }

func (c *alternatingClassifier) Classify(context.Context, string, bool) (memory.Type, float64, error) {
	if c.n.Add(1) == 1 {
		return memory.TypeSemantic, 0.9, nil
	}
	return memory.TypeProject, 0.9, nil
}

func TestConcurrentRememberReturnsOneMemoryIntegration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     memory.Type
		session bool
	}{
		{"project", memory.TypeProject, false},
		{"preference", memory.TypePreference, false},
		{"omitted type", "", false},
		{"session summary replacement", memory.TypeEpisodic, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typ := tc.typ
			e := &barrierEmbedder{reached: make(chan struct{}, 2), release: make(chan struct{})}
			h, pool := integrationHandlers(t, e)
			h.classifier = &alternatingClassifier{}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var req *mcp.CallToolRequest
			var oldID string
			if tc.session {
				old, err := h.store.Create(ctx, store.CreateParams{Type: memory.TypeEpisodic, Scope: "project:ex/concurrent", Content: "The previous session summary.",
					SourceAgent: "test", SourceSession: "session-1", Trust: memory.TrustAgent, Status: memory.StatusActive})
				if err != nil {
					t.Fatal(err)
				}
				oldID = old.ID
				req = &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{MetaSessionID: "session-1"}}}
			}
			type result struct {
				out RememberOutput
				err error
			}
			done := make(chan result, 2)
			for _, content := range []string{"Use pgx for database access.", "use  PGX for database access"} {
				go func() {
					res, out, err := h.remember(ctx, req, RememberInput{Content: content, Type: typ, Project: "ex/concurrent"})
					if res != nil && res.IsError {
						err = errors.New("remember returned a tool error")
					}
					done <- result{out, err}
				}()
			}
			for range 2 {
				select {
				case <-e.reached:
				case <-ctx.Done():
					t.Fatal("requests did not reach the embedding barrier")
				}
			}
			close(e.release)
			first, second := <-done, <-done
			if first.err != nil || second.err != nil {
				t.Fatalf("remember errors: %v, %v", first.err, second.err)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM memory WHERE status IN ('active', 'proposed')`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if first.out.ID != second.out.ID || count != 1 || first.out.Deduplicated == second.out.Deduplicated {
				t.Errorf("concurrent remember was not idempotent: ids=%s/%s rows=%d deduplicated=%v/%v", first.out.ID, second.out.ID, count, first.out.Deduplicated, second.out.Deduplicated)
			}
			if typ == memory.TypePreference && (first.out.Status != memory.StatusProposed || second.out.Status != memory.StatusProposed) {
				t.Error("deduplication activated an unapproved preference")
			}
			if tc.session {
				if old, err := h.store.Get(ctx, oldID); err != nil || old.Status != memory.StatusSuperseded {
					t.Errorf("previous summary not retired: status=%s err=%v", old.Status, err)
				}
				if cur, err := h.store.Get(ctx, first.out.ID); err != nil || cur.Supersedes == nil || *cur.Supersedes != oldID {
					t.Error("session summary replacement lost its history link")
				}
			}
		})
	}
}

func TestRememberDoesNotDeduplicateRetiredMemoriesIntegration(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "expired"}[expired], func(t *testing.T) {
			h, _ := integrationHandlers(t, nil)
			ctx := context.Background()
			p := store.CreateParams{Type: memory.TypeTemporary, Scope: "user", Content: "A temporary deployment note.", SourceAgent: "test", Trust: memory.TrustAgent, Status: memory.StatusActive}
			exp := time.Now().Add(time.Hour)
			if expired {
				exp = time.Now().Add(-time.Hour)
			}
			p.ExpiresAt = &exp
			old, err := h.store.Create(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if !expired {
				if _, err := h.store.SoftDelete(ctx, old.ID, "outdated", "test"); err != nil {
					t.Fatal(err)
				}
			}
			res, out, err := h.remember(ctx, nil, RememberInput{Content: p.Content, Type: p.Type, TTLSeconds: 3600})
			if err != nil || (res != nil && res.IsError) || out.ID == old.ID || out.Deduplicated {
				t.Errorf("retired memory prevented new write: out=%+v err=%v", out, err)
			}
		})
	}
}

func TestRememberExplicitReplacementPreservesIntentIntegration(t *testing.T) {
	h, _ := integrationHandlers(t, nil)
	ctx := context.Background()
	p := store.CreateParams{Type: memory.TypeProject, Scope: "project:ex/replacement", Content: "Use npm for installation.", SourceAgent: "test", Trust: memory.TrustAgent, Status: memory.StatusActive}
	old, err := h.store.Create(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	p.Content = "Use pnpm for installation."
	other, err := h.store.Create(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	res, out, err := h.remember(ctx, nil, RememberInput{Content: p.Content, Project: "ex/replacement", Supersedes: old.ID})
	if err != nil || (res != nil && res.IsError) || out.ID == other.ID || out.Deduplicated {
		t.Fatalf("explicit replacement lost to unrelated duplicate: out=%+v err=%v", out, err)
	}
	if m, err := h.store.Get(ctx, old.ID); err != nil || m.Status != memory.StatusSuperseded {
		t.Errorf("old memory not retired: status=%s err=%v", m.Status, err)
	}
	if m, err := h.store.Get(ctx, out.ID); err != nil || m.Supersedes == nil || *m.Supersedes != old.ID {
		t.Error("replacement history link lost")
	}
}
