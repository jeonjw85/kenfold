package store

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/migrations"
)

// TestCodeRefIntegration covers code references and graph neighbors. It
// TRUNCATES memory; use a throwaway database.
func TestCodeRefIntegration(t *testing.T) {
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
	s := New(pool)
	const scope = "project:ex/refs"
	mk := func(typ memory.Type, scope, content string, refs ...RefTarget) Memory {
		t.Helper()
		m, err := s.Create(ctx, CreateParams{Type: typ, Scope: scope, Content: content, SourceAgent: "t",
			Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive, Refs: refs})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	file := RefTarget{Path: "internal/webhook/handler.go"}
	sym := RefTarget{Path: "internal/webhook/handler.go", Symbol: "dedupe"}
	loose := RefTarget{Symbol: "Register"}
	ghost := RefTarget{Symbol: "NoSuchThing"}
	a := mk(memory.TypeCodebase, scope, "dedupe in handler.go", file, sym, ghost)
	b := mk(memory.TypeCodebase, scope, "Register registers workers", loose, file)
	user := mk(memory.TypeSemantic, "user", "user-wide memories have no refs", file)

	t.Run("create stores refs for project scopes only", func(t *testing.T) {
		refs, err := s.Refs(ctx, []string{a.ID, b.ID, user.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(refs[a.ID]) != 3 || len(refs[b.ID]) != 2 || len(refs[user.ID]) != 0 {
			t.Fatalf("refs = %+v", refs)
		}
		if refs[a.ID][0].State != RefPending {
			t.Errorf("new ref state = %s", refs[a.ID][0].State)
		}
	})

	targets, err := s.SyncTargets(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 4 { // file (shared by a and b), sym, loose, ghost
		t.Fatalf("targets = %+v", targets)
	}

	const c1, c2 = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	t.Run("anchor", func(t *testing.T) {
		sum, err := s.ApplyRefChecks(ctx, scope, c1, []RefCheck{
			{Path: file.Path, Found: true, Hash: "blob:f1"},
			{Path: sym.Path, Symbol: sym.Symbol, Found: true, Hash: "sym:s1"},
			{Symbol: loose.Symbol, Found: true, Hash: "sym:l1", ResolvedPath: "internal/jobs/workers.go"},
			{Symbol: ghost.Symbol, Found: false},
			{Path: "not/referenced.go", Found: true, Hash: "blob:x"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if sum.Anchored != 4 || sum.Updated != 5 || sum.Unmatched != 1 || sum.States[RefUnresolved] != 1 {
			t.Errorf("summary = %+v", sum)
		}
		refs, _ := s.Refs(ctx, []string{a.ID, b.ID})
		if len(refs[a.ID]) != 2 { // the unresolved ghost is hidden
			t.Errorf("a refs = %+v", refs[a.ID])
		}
		for _, r := range append(refs[a.ID], refs[b.ID]...) {
			if r.State != RefCurrent || r.AnchorCommit == nil || *r.AnchorCommit != c1 {
				t.Errorf("anchored ref = %+v", r)
			}
			if r.Symbol == loose.Symbol && (r.ResolvedPath == nil || *r.ResolvedPath != "internal/jobs/workers.go") {
				t.Errorf("resolved path = %v", r.ResolvedPath)
			}
		}
		// Anchored targets now carry their anchor; the unresolved one is still offered (it may appear later).
		targets, _ := s.SyncTargets(ctx, scope)
		if len(targets) != 4 || targets[0].Symbol != ghost.Symbol || targets[0].AnchorCommit != "" {
			t.Errorf("targets after anchoring = %+v", targets)
		}
	})

	t.Run("check", func(t *testing.T) {
		sum, err := s.ApplyRefChecks(ctx, scope, c2, []RefCheck{
			{Path: file.Path, AnchorCommit: c1, Found: true, Hash: "blob:f2"},              // file changed
			{Path: sym.Path, Symbol: sym.Symbol, AnchorCommit: c1, Found: false},           // symbol removed
			{Symbol: loose.Symbol, AnchorCommit: c1, Found: true, Hash: "sym:l1"},          // unchanged
			{Path: sym.Path, Symbol: sym.Symbol, AnchorCommit: c2, Found: true, Hash: "x"}, // wrong anchor: no match
		})
		if err != nil {
			t.Fatal(err)
		}
		if sum.States[RefChanged] != 2 || sum.States[RefMissing] != 1 || sum.States[RefCurrent] != 1 || sum.Unmatched != 1 {
			t.Errorf("summary = %+v", sum)
		}
		refs, _ := s.Refs(ctx, []string{a.ID})
		var stale []string
		for _, r := range refs[a.ID] {
			if r.Stale() {
				stale = append(stale, r.Path+"#"+r.Symbol)
			}
			if r.CheckedCommit == nil || *r.CheckedCommit != c2 {
				t.Errorf("checked commit = %v", r.CheckedCommit)
			}
		}
		if !slices.Equal(stale, []string{"internal/webhook/handler.go#dedupe"}) {
			t.Errorf("stale = %v (a changed file alone is not stale)", stale)
		}
		counts, err := s.RefCounts(ctx, scope)
		if err != nil || counts[RefMissing] != 1 || counts[RefChanged] != 2 || counts[RefCurrent] != 1 || counts[RefUnresolved] != 1 {
			t.Errorf("counts = %v, %v", counts, err)
		}
		listed, err := s.List(ctx, ListParams{Scopes: []string{scope}, Stale: true})
		if err != nil || len(listed) != 1 || listed[0].ID != a.ID {
			t.Errorf("stale list = %+v, %v", listed, err)
		}
	})

	t.Run("targets rotate: never checked first, then least recently checked", func(t *testing.T) {
		// After "check", every target has been checked once; touch one to make it the most recent.
		if _, err := pool.Exec(ctx, `UPDATE memory_ref SET checked_at = now() + interval '1 hour' WHERE scope = $1 AND symbol = $2`, scope, loose.Symbol); err != nil {
			t.Fatal(err)
		}
		fresh := mk(memory.TypeCodebase, scope, "new fact", RefTarget{Path: "fresh.go"})
		targets, err := s.SyncTargets(ctx, scope)
		if err != nil || len(targets) < 3 {
			t.Fatalf("targets = %+v, %v", targets, err)
		}
		if targets[0].Path != "fresh.go" || targets[len(targets)-1].Symbol != loose.Symbol {
			t.Errorf("order = %+v", targets)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM memory WHERE id = $1`, fresh.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("validation", func(t *testing.T) {
		if _, err := s.ApplyRefChecks(ctx, scope, c2, make([]RefCheck, maxRefChecks+1)); err != ErrTooManyChecks {
			t.Errorf("oversized report: %v", err)
		}
		if err := s.AddRefs(ctx, "nope", scope, []RefTarget{file}); err != ErrInvalidID {
			t.Errorf("bad id: %v", err)
		}
		long := RefTarget{Path: strings.Repeat("a", maxRefField+1) + ".go"}
		if err := s.AddRefs(ctx, b.ID, scope, []RefTarget{long, {}}); err != nil {
			t.Fatal(err)
		}
		if refs, _ := s.Refs(ctx, []string{b.ID}); len(refs[b.ID]) != 2 {
			t.Errorf("invalid refs were stored: %+v", refs[b.ID])
		}
	})

	t.Run("backfill", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `DELETE FROM memory_ref WHERE memory_id = $1`, b.ID); err != nil {
			t.Fatal(err)
		}
		n, err := s.BackfillRefs(ctx, []memory.Type{memory.TypeCodebase}, func(content string) []RefTarget {
			if strings.Contains(content, "Register") {
				return []RefTarget{loose}
			}
			return nil
		})
		if err != nil || n != 1 {
			t.Fatalf("backfill = %d, %v", n, err)
		}
		if refs, _ := s.Refs(ctx, []string{b.ID}); len(refs[b.ID]) != 1 || refs[b.ID][0].State != RefPending {
			t.Errorf("backfilled refs = %+v", refs[b.ID])
		}
	})

	t.Run("neighbors", func(t *testing.T) {
		summary := mk(memory.TypeEpisodic, scope, "Session summary: webhooks")
		f1 := mk(memory.TypeCodebase, scope, "fact one from the session")
		f2 := mk(memory.TypeProject, scope, "fact two from the session")
		other := mk(memory.TypeProject, "project:ex/other", "fact in another project")
		for _, f := range []Memory{f1, f2, other} {
			if err := s.AddEdge(ctx, f.ID, "derived_from", summary.ID, "t"); err != nil {
				t.Fatal(err)
			}
		}
		got := func(seeds []string, types ...memory.Type) map[string]string {
			t.Helper()
			ns, err := s.Neighbors(ctx, NeighborParams{Seeds: seeds, Scopes: []string{"user", scope}, Types: types})
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]string{}
			for _, n := range ns {
				out[n.ID+"<"+n.Via] = n.Relation
			}
			return out
		}
		// From the summary: both facts in scope (not the other project's).
		if g := got([]string{summary.ID}); len(g) != 2 || g[f1.ID+"<"+summary.ID] != "derived_from" || g[f2.ID+"<"+summary.ID] != "derived_from" {
			t.Errorf("from summary = %v", g)
		}
		// From a fact: its source and its sibling.
		if g := got([]string{f1.ID}); len(g) != 2 || g[summary.ID+"<"+f1.ID] != "derived_from" || g[f2.ID+"<"+f1.ID] != RelSibling {
			t.Errorf("from fact = %v", g)
		}
		// Seeds are not returned; type filters apply.
		if g := got([]string{f1.ID, f2.ID}, memory.TypeEpisodic); len(g) != 2 || g[summary.ID+"<"+f1.ID] == "" || g[summary.ID+"<"+f2.ID] == "" {
			t.Errorf("typed = %v", g)
		}
		if _, err := s.Neighbors(ctx, NeighborParams{Seeds: []string{"x"}}); err != ErrInvalidID {
			t.Errorf("bad seed: %v", err)
		}
	})
}
