package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/migrations"
)

// TestScanIntegration covers `kenfold scan` for memories stored before the
// secret filter existed. It TRUNCATES memory; use a throwaway database.
func TestScanIntegration(t *testing.T) {
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

	// Secrets assembled at runtime; inserted with SQL, as the filter would reject them.
	token := "ghp_" + strings.Repeat("Qx7", 12)
	password := "Zr9-" + strings.Repeat("kP4", 4)
	var leakyID, stepsID, cleanID string
	if err := pool.QueryRow(ctx, `INSERT INTO memory (type, scope, content, source_agent, status)
		VALUES ('project', 'project:ex/scan', $1, 'old-agent', 'deleted') RETURNING id`,
		"CI deploys with "+token).Scan(&leakyID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO memory (type, scope, content, source_agent, attrs, expires_at)
		VALUES ('temporary', 'project:ex/scan', 'handoff', 'old-agent', $1, now() + interval '1 day') RETURNING id`,
		map[string]any{"kind": "handoff", "next_steps": []string{"log in to postgres://app:" + password + "@db/app"}}).Scan(&stepsID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO memory (type, scope, content, source_agent)
		VALUES ('semantic', 'user', 'nothing secret here', 'old-agent') RETURNING id`).Scan(&cleanID); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{"KENFOLD_DATABASE_URL": url}
	out, errOut, err := runCLI(t, env, "scan")
	if !errors.Is(err, errSecretsFound) {
		t.Fatalf("scan err = %v (stderr %q)", err, errOut)
	}
	for _, id := range []string{leakyID, stepsID} {
		if !strings.Contains(out, id) {
			t.Errorf("scan output misses %s:\n%s", id, out)
		}
	}
	if strings.Contains(out, cleanID) {
		t.Errorf("clean memory reported:\n%s", out)
	}
	if strings.Contains(out+errOut, token) || strings.Contains(out+errOut, password) {
		t.Fatal("scan output echoes a secret")
	}
	if !strings.Contains(out, "GitHub token") || !strings.Contains(out, "password in URL") || !strings.Contains(out, "attrs") {
		t.Errorf("scan output lacks labels/fields:\n%s", out)
	}

	if _, errOut, err := runCLI(t, env, "scan", "--redact"); err != nil || !strings.Contains(errOut, "Redacted 2 memories") {
		t.Fatalf("scan --redact: %v, %q", err, errOut)
	}
	var content, steps string
	if err := pool.QueryRow(ctx, `SELECT content FROM memory WHERE id = $1`, leakyID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT attrs->'next_steps'->>0 FROM memory WHERE id = $1`, stepsID).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	if content != "CI deploys with [REDACTED:github-token]" || strings.Contains(steps, password) || !strings.Contains(steps, "[REDACTED:url-credentials]") {
		t.Errorf("after redact: content %q, step %q", content, steps)
	}
	var kind string
	if err := pool.QueryRow(ctx, `SELECT attrs->>'kind' FROM memory WHERE id = $1`, stepsID).Scan(&kind); err != nil || kind != "handoff" {
		t.Errorf("redaction lost other attrs: kind=%q, %v", kind, err)
	}
	if _, errOut, err := runCLI(t, env, "scan"); err != nil || !strings.Contains(errOut, "No secrets found") {
		t.Errorf("rescan: %v, %q", err, errOut)
	}
	if _, _, err := runCLI(t, env, "scan", "extra"); !errors.Is(err, errUsage) {
		t.Errorf("scan extra: %v", err)
	}
}
