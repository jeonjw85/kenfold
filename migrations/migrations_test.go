package migrations

import (
	"context"
	"database/sql"
	"os"
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
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
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

	// Down must fully revert.
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE tablename IN ('memory', 'memory_edge')`).Scan(&tables); err != nil || tables != 0 {
		t.Errorf("tables after down = %d, %v; want 0", tables, err)
	}
	// Leave the schema migrated for manual inspection.
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("re-up: %v", err)
	}
}
