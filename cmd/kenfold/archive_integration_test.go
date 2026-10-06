package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

// TestArchiveIntegration exports memories (with history, graph edges, and
// code references), wipes the database, imports the archive, and checks that
// everything came back as it was. It TRUNCATES memory.
func TestArchiveIntegration(t *testing.T) {
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
	const scope = "project:github.com/acme/archive"
	mk := func(p store.CreateParams) store.Memory {
		t.Helper()
		if p.SourceAgent == "" {
			p.SourceAgent = "codex"
		}
		if p.Trust == "" {
			p.Trust = memory.TrustAgent
		}
		if p.Status == "" {
			p.Status = memory.StatusActive
		}
		if p.Scope == "" {
			p.Scope = scope
		}
		p.Confidence = 0.5
		m, err := st.Create(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	old := mk(store.CreateParams{Type: memory.TypeProject, Content: "Rate limit is 60 requests per minute."})
	cur := mk(store.CreateParams{Type: memory.TypeProject, Content: "Rate limit is 100 requests per minute per key.", Supersedes: &old.ID})
	sum := mk(store.CreateParams{Type: memory.TypeEpisodic, Content: "Session summary: raised the rate limit.", SourceSession: "s-1"})
	fact := mk(store.CreateParams{Type: memory.TypeCodebase, Content: "The limiter lives in `Allow` in internal/ratelimit/bucket.go.",
		Refs:  []store.RefTarget{{Path: "internal/ratelimit/bucket.go"}, {Path: "internal/ratelimit/bucket.go", Symbol: "Allow"}},
		Attrs: map[string]any{"extracted_from": sum.ID}})
	if err := st.AddEdge(ctx, fact.ID, "derived_from", sum.ID, "kenfold-extractor"); err != nil {
		t.Fatal(err)
	}
	mk(store.CreateParams{Type: memory.TypePreference, Scope: "user", Content: "Answer in Korean.", Status: memory.StatusProposed})
	exp := time.Now().Add(48 * time.Hour)
	mk(store.CreateParams{Type: memory.TypeTemporary, Content: "Deploy freeze until Monday.", ExpiresAt: &exp})
	gone := mk(store.CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "An obsolete fact."})
	if _, err := st.SoftDelete(ctx, gone.ID, "wrong", "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyRefChecks(ctx, scope, strings.Repeat("c", 40), []store.RefCheck{{Path: "internal/ratelimit/bucket.go", Symbol: "Allow", Found: true, Hash: "sym:x"}}); err != nil {
		t.Fatal(err)
	}
	// snapshot renders every table the archive covers as text, in a stable order.
	snapshot := func() string {
		t.Helper()
		var b strings.Builder
		for _, q := range []string{
			`SELECT concat_ws('|', id, type, scope, content, attrs::text, source_agent, source_session, trust, confidence, status,
			        supersedes, valid_from, valid_to, expires_at, created_at, updated_at) FROM memory ORDER BY id`,
			`SELECT concat_ws('|', src, relation, dst, weight, source_agent, created_at) FROM memory_edge ORDER BY src, relation, dst`,
			`SELECT concat_ws('|', memory_id, scope, path, symbol, state, resolved_path, anchor_hash, anchor_commit, checked_commit, checked_at, created_at)
			 FROM memory_ref ORDER BY memory_id, path, symbol`,
		} {
			rows, err := pool.Query(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString(strings.Join(lines, "\n") + "\n---\n")
		}
		return b.String()
	}
	before := snapshot()

	dir := t.TempDir()
	file := filepath.Join(dir, "backup.jsonl")
	env := map[string]string{"KENFOLD_DATABASE_URL": url}
	_, stderr, err := runCLI(t, env, "export", "--out", file)
	if err != nil || !strings.Contains(stderr, "Exported 7 memories, 1 edges, 2 code references") {
		t.Fatalf("export: %v %s", err, stderr)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("archive mode = %v, %v", fi.Mode().Perm(), err)
	}
	if _, _, err := runCLI(t, env, "export", "--out", file); err == nil {
		t.Error("export overwrote an existing file")
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "embedding") {
		t.Error("the archive contains embeddings")
	}

	out, _, err := runCLI(t, env, "import", "--dry-run", file)
	if err != nil || !strings.Contains(out, "7 memories, 1 edges, 2 code references. Nothing was written.") {
		t.Fatalf("dry run: %v %s", err, out)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
		t.Fatal(err)
	}
	out, _, err = runCLI(t, env, "import", file)
	if err != nil || !strings.Contains(out, "Imported 7 memories, 1 edges, 2 code references.") {
		t.Fatalf("import: %v %s", err, out)
	}
	if after := snapshot(); after != before {
		t.Errorf("round trip changed the data:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// Importing again changes nothing.
	out, _, err = runCLI(t, env, "import", file)
	if err != nil || !strings.Contains(out, "Imported 0 memories") || !strings.Contains(out, "7 memories were already here") {
		t.Errorf("second import: %v %s", err, out)
	}
	if after := snapshot(); after != before {
		t.Error("a second import changed the data")
	}
	// Imported memories are searchable (full-text; embeddings come from the
	// backfill), and the superseded one stays retired.
	res, err := st.Search(ctx, store.SearchParams{Query: "rate limit", Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range res {
		found[r.ID] = true
	}
	if !found[cur.ID] || found[old.ID] {
		t.Errorf("search after import: current %v, superseded %v", found[cur.ID], found[old.ID])
	}

	// Scope and status filters.
	scoped := filepath.Join(dir, "scoped.jsonl")
	if _, stderr, err := runCLI(t, env, "export", "--out", scoped, "--scope", "github.com/acme/archive", "--active"); err != nil ||
		!strings.Contains(stderr, "Exported 4 memories, 1 edges, 2 code references") {
		t.Errorf("scoped export: %v %s", err, stderr)
	}

	// Refused archives write nothing.
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	secret := "ghp_" + strings.Repeat("Q7x", 12)
	for name, body := range map[string]string{
		"truncated":   strings.Join(lines[:len(lines)-2], "\n"),
		"not kenfold": `{"format":"other/1"}`,
		"secret":      lines[0] + "\n" + strings.Replace(lines[1], `"content":"`, `"content":"token `+secret+` `, 1) + "\n" + strings.Join(lines[2:], "\n"),
		"metadata secret": lines[0] + "\n" + strings.Join(lines[1:len(lines)-1], "\n") + "\n" +
			`{"memory":{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","type":"semantic","scope":"user","content":"A safe fact.","source_agent":"codex","trust":"agent","status":"active","attrs":{"nested":[{"next_steps":["` + secret + `"]}]}}}` + "\n" +
			`{"end":{"memories":8,"edges":1,"refs":2}}`,
		"array credential": lines[0] + "\n" + strings.Join(lines[1:len(lines)-1], "\n") + "\n" +
			`{"memory":{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","type":"semantic","scope":"user","content":"A safe fact.","source_agent":"codex","trust":"agent","status":"active","attrs":{"api_key":["a9Q2v7R4z6P1t8M3"]}}}` + "\n" +
			`{"end":{"memories":8,"edges":1,"refs":2}}`,
		"object credential": lines[0] + "\n" + strings.Join(lines[1:len(lines)-1], "\n") + "\n" +
			`{"memory":{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","type":"semantic","scope":"user","content":"A safe fact.","source_agent":"codex","trust":"agent","status":"active","attrs":{"api_key":{"value":"a9Q2v7R4z6P1t8M3"}}}}` + "\n" +
			`{"end":{"memories":8,"edges":1,"refs":2}}`,
		"unknown key": lines[0] + "\n" + `{"memory":{"id":"x"},"surprise":1}`,
	} {
		f := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".jsonl")
		if err := os.WriteFile(f, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
			t.Fatal(err)
		}
		if out, _, err := runCLI(t, env, "import", f); err == nil {
			t.Errorf("%s archive imported: %s", name, out)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM memory`).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s archive wrote %d memories", name, n)
		}
	}
}
