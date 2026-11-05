package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kenfold/kenfold/migrations"
)

// TestHelpFlagPerSubcommand checks that asking a command for help is not an
// error and shows its flags.
func TestHelpFlagPerSubcommand(t *testing.T) {
	for _, args := range [][]string{
		{"key", "create", "--help"},
		{"export", "-h"},
		{"memory", "list", "--help"},
		{"refs", "sync", "-h"},
		{"migrate", "-h"},
	} {
		out, _, err := runCLI(t, nil, args...)
		if err != nil {
			t.Errorf("kenfold %v: err = %v, want success", args, err)
			continue
		}
		if !strings.Contains(out, "Usage: kenfold") {
			t.Errorf("kenfold %v: output = %q, want a usage line", args, out)
		}
	}

	// Commands without flags still answer help instead of rejecting an argument.
	for _, cmd := range []string{"serve", "mcp", "healthcheck", "reindex"} {
		out, _, err := runCLI(t, nil, cmd, "--help")
		if err != nil || !strings.Contains(out, "Usage:") {
			t.Errorf("kenfold %s --help: %q %v", cmd, out, err)
		}
	}

	// A real flag error is still a usage error.
	if _, _, err := runCLI(t, nil, "key", "list", "--bogus"); !errors.Is(err, errUsage) {
		t.Errorf("unknown flag err = %v, want usage", err)
	}
}

// TestMigrateYesFlag checks that --yes is accepted only for `migrate down`.
func TestMigrateYesFlag(t *testing.T) {
	for _, args := range [][]string{
		{"migrate", "up", "--yes"},
		{"migrate", "status", "--yes"},
		{"migrate", "down", "--yes", "extra"},
	} {
		if _, _, err := runCLI(t, nil, args...); !errors.Is(err, errUsage) {
			t.Errorf("kenfold %v: err = %v, want a usage error", args, err)
		}
	}
}

// TestUpdateCheckBadValue checks that a typo in KENFOLD_UPDATE_CHECK is
// reported instead of silently disabling the check.
func TestUpdateCheckBadValue(t *testing.T) {
	_, stderr, err := runCLI(t, map[string]string{"KENFOLD_UPDATE_CHECK": "flase"}, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(stderr, "KENFOLD_UPDATE_CHECK") {
		t.Errorf("stderr = %q, want a KENFOLD_UPDATE_CHECK diagnostic", stderr)
	}
}

// TestHealthcheckReason checks that a failed readiness check reports why the
// server is not ready, which is the only diagnostic inside the distroless
// image.
func TestHealthcheckReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"status":"database unavailable"}`+"\n")
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	_, port, _ := net.SplitHostPort(addr)

	_, _, err := runCLI(t, map[string]string{"KENFOLD_HTTP_ADDR": ":" + port}, "healthcheck")
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("healthcheck err = %v, want the readiness reason", err)
	}
}

// TestRefsStatusUserScope checks that `refs status --scope user` fails with a
// clear message instead of silently querying a project named "user".
func TestRefsStatusUserScope(t *testing.T) {
	_, _, err := runCLI(t, nil, "refs", "status", "--scope", "user")
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "a project is required") {
		t.Fatalf("refs status --scope user: err = %v", err)
	}
}

// TestOffReasonMessages checks the diagnostics printed when a background
// worker is disabled, distinguishing a missing model from an explicit switch.
func TestOffReasonMessages(t *testing.T) {
	if got := consolidationOffReason(false, true); !strings.Contains(got, "KENFOLD_CHAT_URL") {
		t.Errorf("no chat model: %q", got)
	}
	if got := consolidationOffReason(true, false); !strings.Contains(got, "KENFOLD_CONSOLIDATE=false") {
		t.Errorf("disabled: %q", got)
	}
	if got := consolidationOffReason(true, true); got != "" {
		t.Errorf("enabled: %q", got)
	}

	if got := extractionOffReason(false, true); !strings.Contains(got, "KENFOLD_CHAT_URL") {
		t.Errorf("no chat model: %q", got)
	}
	if got := extractionOffReason(true, false); !strings.Contains(got, "KENFOLD_EXTRACT=false") {
		t.Errorf("disabled: %q", got)
	}
	if got := extractionOffReason(true, true); got != "" {
		t.Errorf("enabled: %q", got)
	}
}

// TestSkippedReindexMessage checks that reindex does not point at logs it
// never writes.
func TestSkippedReindexMessage(t *testing.T) {
	got := skippedBackfillMessage(3)
	if !strings.Contains(got, "3") || strings.Contains(got, "see logs") {
		t.Errorf("message = %q", got)
	}
}

// TestDownImpact checks that the preview names the objects a down migration
// removes, taken from its SQL.
func TestDownImpact(t *testing.T) {
	sql := `-- +goose Up
CREATE TABLE t (id int);

-- +goose Down
DROP TABLE memory_edge;
DROP TABLE memory;
DROP INDEX memory_dedupe_idx;
ALTER TABLE memory DROP COLUMN embedding_model;
DROP FUNCTION kenfold_touch_updated_at();
`
	got := downImpact(sql)
	for _, want := range []string{"table memory_edge", "table memory", "index memory_dedupe_idx", "column memory.embedding_model", "function kenfold_touch_updated_at()"} {
		if !slices.Contains(got, want) {
			t.Errorf("downImpact = %v, want %q", got, want)
		}
	}
	if len(got) != 5 {
		t.Errorf("downImpact = %v, want exactly the destructive statements", got)
	}
	if downImpact("-- +goose Up\nSELECT 1;\n") != nil {
		t.Error("a migration without a Down section must have no impact")
	}
}

// TestMigrateDownPreviewIntegration checks that `migrate down` refuses to run
// without --yes, names the migration it would roll back, and leaves the schema
// untouched. It restores the schema afterwards.
func TestMigrateDownPreviewIntegration(t *testing.T) {
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := migrations.Up(ctx, url); err != nil {
			t.Errorf("restore migrations: %v", err)
		}
	}()
	provider, err := migrations.Provider(url)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	before, err := provider.GetDBVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}

	env := map[string]string{"KENFOLD_DATABASE_URL": url}
	out, _, err := runCLI(t, env, "migrate", "down")
	if !errors.Is(err, errNeedsConfirmation) {
		t.Fatalf("migrate down without --yes: err = %v, want confirmation required", err)
	}
	if !strings.Contains(out, "Would roll back") || !strings.Contains(out, ".sql") {
		t.Errorf("preview = %q, want the migration path", out)
	}
	after, err := provider.GetDBVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("schema version changed from %d to %d without --yes", before, after)
	}

	if _, _, err := runCLI(t, env, "migrate", "down", "--yes"); err != nil {
		t.Fatalf("migrate down --yes: %v", err)
	}
	rolled, err := provider.GetDBVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rolled >= before {
		t.Errorf("schema version after --yes = %d, want < %d", rolled, before)
	}
}
