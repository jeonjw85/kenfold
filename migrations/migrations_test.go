package migrations

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestMigrationsIntegration runs against a real PostgreSQL+pgvector database.
// It is opt-in because it resets the schema:
//
//	KENFOLD_TEST_DATABASE_URL=postgres://... go test ./migrations/
//
// `make test-integration` points it at a throwaway database in the compose Postgres.
func TestMigrationsIntegration(t *testing.T) {
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	p, err := Provider(url)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Start from scratch so up and down are both exercised.
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := CheckCurrent(ctx, url); !errors.Is(err, ErrPending) {
		t.Errorf("CheckCurrent on empty schema = %v, want ErrPending", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := CheckCurrent(ctx, url); err != nil {
		t.Errorf("CheckCurrent after up = %v", err)
	}

	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	exec := func(q string, args ...any) error { _, err := db.ExecContext(ctx, q, args...); return err }

	// A valid memory with an embedding round-trips.
	var id string
	err = db.QueryRowContext(ctx, `
		INSERT INTO memory (type, scope, content, source_agent, embedding)
		VALUES ('project', 'project:github.com/kenfold/kenfold', 'Use pgx v5 with sqlc', 'test',
		        array_fill(0.1, ARRAY[1024])::vector)
		RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	// Constraints must reject invalid rows.
	for name, q := range map[string]string{
		"unknown type":        `INSERT INTO memory (type, scope, content, source_agent) VALUES ('working', 'user', 'x', 't')`,
		"temporary w/o ttl":   `INSERT INTO memory (type, scope, content, source_agent) VALUES ('temporary', 'user', 'x', 't')`,
		"empty content":       `INSERT INTO memory (type, scope, content, source_agent) VALUES ('semantic', 'user', '', 't')`,
		"confidence > 1":      `INSERT INTO memory (type, scope, content, source_agent, confidence) VALUES ('semantic', 'user', 'x', 't', 1.5)`,
		"inverted valid time": `INSERT INTO memory (type, scope, content, source_agent, valid_from, valid_to) VALUES ('semantic', 'user', 'x', 't', now(), now() - interval '1 day')`,
	} {
		if err := exec(q); err == nil {
			t.Errorf("%s: insert succeeded; want constraint violation", name)
		}
	}

	// Edges: self-loops rejected, cascade on delete.
	var id2 string
	if err := db.QueryRowContext(ctx, `INSERT INTO memory (type, scope, content, source_agent) VALUES ('semantic', 'user', 'y', 't') RETURNING id`).Scan(&id2); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO memory_edge (src, dst, relation, source_agent) VALUES ($1, $1, 'relates_to', 't')`, id); err == nil {
		t.Error("self-loop edge accepted")
	}
	if err := exec(`INSERT INTO memory_edge (src, dst, relation, source_agent) VALUES ($1, $2, 'relates_to', 't')`, id, id2); err != nil {
		t.Fatalf("insert edge: %v", err)
	}
	if err := exec(`DELETE FROM memory WHERE id = $1`, id2); err != nil {
		t.Fatal(err)
	}
	var edges int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM memory_edge`).Scan(&edges); err != nil || edges != 0 {
		t.Errorf("edges after cascade = %d, %v; want 0", edges, err)
	}

	// updated_at trigger fires.
	var touched bool
	if err := db.QueryRowContext(ctx, `UPDATE memory SET confidence = 0.9 WHERE id = $1 RETURNING updated_at > created_at`, id).Scan(&touched); err != nil || !touched {
		t.Errorf("updated_at not bumped: %v, %v", touched, err)
	}

	// 00002: embedding provenance column exists and is nullable.
	if err := exec(`UPDATE memory SET embedding_model = 'bge-m3' WHERE id = $1`, id); err != nil {
		t.Errorf("set embedding_model: %v", err)
	}

	// 00002: api_key accepts a valid key and rejects invalid ones.
	validHash := `'\x` + strings.Repeat("ab", 32) + `'::bytea`
	if err := exec(`INSERT INTO api_key (agent, prefix, key_hash) VALUES ('claude-code', 'kf_abcd', ` + validHash + `)`); err != nil {
		t.Fatalf("insert api_key: %v", err)
	}
	for name, q := range map[string]string{
		"duplicate hash":    `INSERT INTO api_key (agent, prefix, key_hash) VALUES ('codex', 'kf_x', ` + validHash + `)`,
		"short hash":        `INSERT INTO api_key (agent, prefix, key_hash) VALUES ('codex', 'kf_x', '\x0102'::bytea)`,
		"uppercase agent":   `INSERT INTO api_key (agent, prefix, key_hash) VALUES ('Codex', 'kf_x', '\x` + strings.Repeat("cd", 32) + `'::bytea)`,
		"agent with spaces": `INSERT INTO api_key (agent, prefix, key_hash) VALUES ('my agent', 'kf_x', '\x` + strings.Repeat("ef", 32) + `'::bytea)`,
		"empty prefix":      `INSERT INTO api_key (agent, prefix, key_hash) VALUES ('codex', '', '\x` + strings.Repeat("01", 32) + `'::bytea)`,
	} {
		if err := exec(q); err == nil {
			t.Errorf("api_key %s: insert succeeded; want constraint violation", name)
		}
	}

	// 00003: content_key normalizes case, whitespace, and trailing punctuation.
	var key string
	if err := db.QueryRowContext(ctx, `INSERT INTO memory (type, scope, content, source_agent)
		VALUES ('semantic', 'user', E'  We  use\tPGX v5.  ', 't') RETURNING content_key`).Scan(&key); err != nil {
		t.Fatalf("content_key: %v", err)
	}
	if key != "we use pgx v5" {
		t.Errorf("content_key = %q", key)
	}
	var sim float64
	if err := db.QueryRowContext(ctx, `SELECT similarity('deploy on fridays', 'we deploy on fridays')`).Scan(&sim); err != nil || sim <= 0 {
		t.Errorf("pg_trgm similarity = %v, %v", sim, err)
	}

	// 00004: extraction rows reference memories and cascade on delete.
	var epi string
	if err := db.QueryRowContext(ctx, `INSERT INTO memory (type, scope, content, source_agent) VALUES ('episodic', 'user', 'session', 't') RETURNING id`).Scan(&epi); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO extraction (source_id, status, attempts) VALUES ($1, 'running', 1)`, epi); err != nil {
		t.Fatalf("insert extraction: %v", err)
	}
	if err := exec(`INSERT INTO extraction (source_id, status) VALUES ($1, 'queued')`, id); err == nil {
		t.Error("unknown extraction status accepted")
	}
	if err := exec(`DELETE FROM memory WHERE id = $1`, epi); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM extraction`).Scan(&jobs); err != nil || jobs != 0 {
		t.Errorf("extraction rows after cascade = %d, %v", jobs, err)
	}

	// 00005: code references need a project scope and a target; anchors are all or nothing; they cascade.
	var code string
	if err := db.QueryRowContext(ctx, `INSERT INTO memory (type, scope, content, source_agent) VALUES ('codebase', 'project:ex/r', 'x in a.go', 't') RETURNING id`).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO memory_ref (memory_id, scope, path) VALUES ($1, 'project:ex/r', 'a.go')`, code); err != nil {
		t.Fatalf("insert ref: %v", err)
	}
	for name, q := range map[string]string{
		"user scope":     `INSERT INTO memory_ref (memory_id, scope, path) VALUES ($1, 'user', 'b.go')`,
		"no target":      `INSERT INTO memory_ref (memory_id, scope) VALUES ($1, 'project:ex/r')`,
		"bad state":      `INSERT INTO memory_ref (memory_id, scope, path, state) VALUES ($1, 'project:ex/r', 'c.go', 'stale')`,
		"partial anchor": `INSERT INTO memory_ref (memory_id, scope, path, anchor_hash) VALUES ($1, 'project:ex/r', 'd.go', 'h')`,
		"duplicate":      `INSERT INTO memory_ref (memory_id, scope, path) VALUES ($1, 'project:ex/r', 'a.go')`,
	} {
		if err := exec(q, code); err == nil {
			t.Errorf("memory_ref %s accepted", name)
		}
	}
	if err := exec(`DELETE FROM memory WHERE id = $1`, code); err != nil {
		t.Fatal(err)
	}
	var refs int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM memory_ref`).Scan(&refs); err != nil || refs != 0 {
		t.Errorf("memory_ref rows after cascade = %d, %v", refs, err)
	}

	// 00006: one owner row; grants and tokens cascade from clients.
	if err := exec(`INSERT INTO oauth_owner (id, password_hash) VALUES (2, 'x')`); err == nil {
		t.Error("a second owner row was accepted")
	}
	if err := exec(`INSERT INTO oauth_client (client_id, kind, redirect_uris) VALUES ('c1', 'cimd', '{https://a.example/cb}')`); err != nil {
		t.Fatalf("insert client: %v", err)
	}
	if err := exec(`INSERT INTO oauth_client (client_id, kind, redirect_uris) VALUES ('c2', 'cimd', '{}')`); err == nil {
		t.Error("client without redirect URIs accepted")
	}
	var gid string
	if err := db.QueryRowContext(ctx, `INSERT INTO oauth_grant (client_id, agent, scopes, resource) VALUES ('c1', 'chatgpt', '{memory:read}', 'https://k.example/mcp') RETURNING id`).Scan(&gid); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	if err := exec(`INSERT INTO oauth_grant (client_id, agent, scopes, resource) VALUES ('c1', 'Bad Agent', '{memory:read}', 'r')`); err == nil {
		t.Error("invalid agent name accepted")
	}
	if err := exec(`INSERT INTO oauth_token (token_hash, kind, grant_id, scopes, expires_at) VALUES (decode(repeat('ab', 32), 'hex'), 'access', $1, '{memory:read}', now())`, gid); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	if err := exec(`INSERT INTO oauth_token (token_hash, kind, grant_id, scopes, expires_at) VALUES (decode('ab', 'hex'), 'access', $1, '{memory:read}', now())`, gid); err == nil {
		t.Error("short token hash accepted")
	}
	if err := exec(`DELETE FROM oauth_client WHERE client_id = 'c1'`); err != nil {
		t.Fatal(err)
	}
	var oauthRows int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM oauth_grant) + (SELECT count(*) FROM oauth_token)`).Scan(&oauthRows); err != nil || oauthRows != 0 {
		t.Errorf("oauth rows after cascade = %d, %v", oauthRows, err)
	}

	// 00007: a member set is judged once; kinds carry what they need.
	var c1, c2, c3 string
	for _, dst := range []*string{&c1, &c2, &c3} {
		if err := db.QueryRowContext(ctx, `INSERT INTO memory (type, scope, content, source_agent) VALUES ('project', 'project:x', 'consolidation member ' || gen_random_uuid(), 't') RETURNING id::text`).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	pair := func(a, b string) string { return "{" + min(a, b) + "," + max(a, b) + "}" }
	if err := exec(`INSERT INTO consolidation (kind, scope, member_ids, keep_id, model) VALUES ('duplicate', 'project:x', $1, $2, 'm')`, pair(c1, c2), c1); err != nil {
		t.Fatalf("insert proposal: %v", err)
	}
	for name, q := range map[string][]any{
		"the same member set twice":   {`INSERT INTO consolidation (kind, scope, member_ids, model, status) VALUES ('distinct', 'project:x', $1, 'm', 'dismissed')`, pair(c1, c2)},
		"a conflict without keep_id":  {`INSERT INTO consolidation (kind, scope, member_ids, model) VALUES ('conflict', 'project:x', $1, 'm')`, pair(c1, c3)},
		"keep_id outside the members": {`INSERT INTO consolidation (kind, scope, member_ids, keep_id, model) VALUES ('conflict', 'project:x', $1, $2, 'm')`, pair(c1, c3), c2},
		"a pending distinct pair":     {`INSERT INTO consolidation (kind, scope, member_ids, model) VALUES ('distinct', 'project:x', $1, 'm')`, pair(c1, c3)},
		"a digest without content":    {`INSERT INTO consolidation (kind, scope, member_ids, model) VALUES ('digest', 'project:x', $1, 'm')`, pair(c1, c3)},
		"a one-member set":            {`INSERT INTO consolidation (kind, scope, member_ids, model, status) VALUES ('distinct', 'project:x', $1, 'm', 'dismissed')`, "{" + c3 + "}"},
	} {
		if err := exec(q[0].(string), q[1:]...); err == nil {
			t.Errorf("accepted %s", name)
		}
	}

	// Down must fully revert.
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE tablename IN ('memory', 'memory_edge', 'api_key', 'extraction', 'memory_ref', 'oauth_owner', 'oauth_client', 'oauth_grant', 'oauth_code', 'oauth_token', 'consolidation')`).Scan(&tables); err != nil || tables != 0 {
		t.Errorf("tables after down = %d, %v; want 0", tables, err)
	}
	// Leave the schema migrated for manual inspection.
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("re-up: %v", err)
	}
}
