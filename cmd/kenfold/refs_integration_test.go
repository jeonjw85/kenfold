package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/mcpserver"
	"github.com/kenfold/kenfold/migrations"
)

// TestCodeRefsIntegration is the code-invalidation acceptance test: an agent
// stores a memory about a function; `kenfold refs sync` in the repository
// anchors it; after a commit removes the function, the next sync marks the
// memory stale and every read path says so. It TRUNCATES memory and api_key.
func TestCodeRefsIntegration(t *testing.T) {
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DatabaseURL: url, AllowedHosts: config.DefaultAllowedHosts, Auth: config.AuthAPIKey, SearchMaxDistance: config.DefaultSearchMaxDistance}
	rt, err := newRuntime(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := rt.pool.Exec(ctx, `TRUNCATE memory, api_key CASCADE`); err != nil {
		t.Fatal(err)
	}
	key, _, err := rt.keys.Create(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpHandler(cfg, rt, slog.New(slog.DiscardHandler)))
	defer srv.Close()

	repo := makeRepo(t, "git@github.com:acme/payments.git")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(path, content string) {
		t.Helper()
		p := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/webhook/handler.go", "package webhook\n\nfunc dedupePayload(b []byte) string { return \"h\" }\n\nfunc Handle() {}\n")
	git("add", "-A")
	git("commit", "-q", "-m", "webhooks")

	cs, err := agentSession(ctx, srv.URL, "codex", key)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	const project = "github.com/acme/payments"
	stored := callTool[mcpserver.RememberOutput](t, cs, "remember", map[string]any{
		"content": "Webhook deliveries are deduplicated by `dedupePayload` in internal/webhook/handler.go.", "type": "codebase", "project": project})
	recallOne := func() mcpserver.MemoryView {
		t.Helper()
		out := callTool[mcpserver.RecallOutput](t, cs, "recall", map[string]any{"query": "webhook deliveries deduplicated", "project": project})
		m := findMemory(out.Memories, stored.ID)
		if m == nil {
			t.Fatalf("recall did not return the memory: %+v", out.Memories)
		}
		return *m
	}
	states := func(m mcpserver.MemoryView) map[string]string {
		out := map[string]string{}
		for _, r := range m.CodeRefs {
			out[r.Path+"#"+r.Symbol] = r.State
		}
		return out
	}
	if m := recallOne(); m.Stale || len(m.CodeRefs) != 2 || states(m)["internal/webhook/handler.go#dedupePayload"] != "pending" {
		t.Fatalf("after write: stale %v refs %+v", m.Stale, m.CodeRefs)
	}

	env := map[string]string{"KENFOLD_URL": srv.URL + "/mcp", "KENFOLD_API_KEY": key}
	out, stderr, err := runCLI(t, env, "refs", "sync", "--dir", filepath.Join(repo, "internal"))
	if err != nil || !strings.Contains(out, "Checked 2 of 2 references of "+project) || !strings.Contains(out, "2 current") {
		t.Fatalf("first sync: %v\n%s%s", err, out, stderr)
	}
	if m := recallOne(); m.Stale || states(m)["internal/webhook/handler.go#dedupePayload"] != "current" || m.CodeRefs[0].CheckedCommit == "" {
		t.Errorf("after anchoring: stale %v refs %+v", m.Stale, m.CodeRefs)
	}

	// An unrelated edit to the file changes the file, not the function: not stale.
	write("internal/webhook/handler.go", "package webhook\n\nfunc dedupePayload(b []byte) string { return \"h\" }\n\nfunc Handle() { log() }\n\nfunc log() {}\n")
	git("commit", "-q", "-am", "logging")
	if out, _, err := runCLI(t, env, "refs", "sync", "--dir", repo); err != nil || !strings.Contains(out, "1 current, 1 changed") {
		t.Fatalf("second sync: %v %s", err, out)
	}
	if m := recallOne(); m.Stale {
		t.Errorf("a changed file alone made the memory stale: %+v", m.CodeRefs)
	}

	// Removing the function makes the memory stale everywhere.
	write("internal/webhook/handler.go", "package webhook\n\nfunc Handle() { dedupeByEventID() }\n")
	git("commit", "-q", "-am", "dedupe by event id")
	if out, _, err := runCLI(t, env, "refs", "sync", "--dir", repo, "--quiet"); err != nil || out != "" {
		t.Fatalf("quiet sync: %v %q", err, out)
	}
	if m := recallOne(); !m.Stale || states(m)["internal/webhook/handler.go#dedupePayload"] != "missing" {
		t.Errorf("after removal: stale %v refs %+v", m.Stale, m.CodeRefs)
	}
	dbEnv := map[string]string{"KENFOLD_DATABASE_URL": url}
	out, _, err = runCLI(t, dbEnv, "refs", "status", "--scope", project)
	if err != nil || !strings.Contains(out, "1 missing") || !strings.Contains(out, stored.ID) || !strings.Contains(out, "dedupePayload in internal/webhook/handler.go missing at") {
		t.Errorf("refs status: %v\n%s", err, out)
	}
	if out, _, err := runCLI(t, dbEnv, "memory", "list", "--stale"); err != nil || !strings.Contains(out, stored.ID) {
		t.Errorf("memory list --stale: %v\n%s", err, out)
	}

	// The API is protected like /mcp.
	req := func(method, path, host, token string) int {
		t.Helper()
		r, _ := http.NewRequest(method, srv.URL+path, nil)
		if host != "" {
			r.Host = host
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		method, path, host, token string
		want                      int
	}{
		{"GET", "/api/v1/refs?project=" + project, "", "", http.StatusUnauthorized},
		{"GET", "/api/v1/refs?project=" + project, "", "kf_" + strings.Repeat("x", 30), http.StatusUnauthorized},
		{"GET", "/api/v1/refs?project=" + project, "evil.example", key, http.StatusForbidden},
		{"GET", "/api/v1/refs", "", key, http.StatusBadRequest},
		{"GET", "/api/v1/refs?project=/Users/me/repo", "", key, http.StatusBadRequest},
		{"POST", "/api/v1/refs/check", "", key, http.StatusBadRequest},
		{"GET", "/api/v1/nope", "", key, http.StatusNotFound},
		{"GET", "/api/v1/refs?project=" + project, "", key, http.StatusOK},
	} {
		if got := req(c.method, c.path, c.host, c.token); got != c.want {
			t.Errorf("%s %s (host %q, token %v) = %d, want %d", c.method, c.path, c.host, c.token != "", got, c.want)
		}
	}
}
