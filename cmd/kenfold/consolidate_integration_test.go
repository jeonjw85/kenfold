package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

// consolidationChat is an OpenAI-compatible chat server that answers with
// answer(schema name, user prompt).
func consolidationChat(t *testing.T, answer func(schema, prompt string) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			ResponseFormat struct {
				JSONSchema struct {
					Name string `json:"name"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		content := answer(req.ResponseFormat.JSONSchema.Name, req.Messages[len(req.Messages)-1].Content)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestConsolidationIntegration runs consolidation end to end with a fake
// model: `kenfold consolidate run` makes proposals; the dashboard shows,
// applies, and rejects them; the CLI lists and applies them. It TRUNCATES
// memory, api_key, and the oauth tables.
func TestConsolidationIntegration(t *testing.T) {
	dbURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	var judged atomic.Int32
	model := consolidationChat(t, func(schema, prompt string) string {
		if schema == "session_digest" {
			return `{"digest":"- Built the importer in internal/store/archive.go.\n- Kept the code refs in the archive."}`
		}
		judged.Add(1)
		if strings.Contains(prompt, "port") {
			return `{"relation":"conflict","keep":"A","reason":"They name different ports."}`
		}
		// The more detailed memory is kept, wherever it is shown.
		if _, b, _ := strings.Cut(prompt, "Memory B ("); strings.Contains(b, "in this repository") {
			return `{"relation":"same","keep":"B","reason":"B says what A says."}`
		}
		return `{"relation":"same","keep":"A","reason":"B says what A says."}`
	})
	env := map[string]string{"KENFOLD_DATABASE_URL": dbURL, "KENFOLD_CHAT_URL": model.URL + "/v1"}
	cfg, err := config.LoadFrom(func(k string) string { return env[k] })
	if err != nil || !cfg.Consolidate {
		t.Fatalf("config: consolidate %v, %v", cfg.Consolidate, err)
	}
	rt, err := newRuntime(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := rt.pool.Exec(ctx, `TRUNCATE memory, api_key, oauth_client, oauth_owner CASCADE`); err != nil {
		t.Fatal(err)
	}

	const scope = "project:github.com/acme/consol"
	mk := func(typ memory.Type, content string) store.Memory {
		t.Helper()
		m, err := rt.store.Create(ctx, store.CreateParams{Type: typ, Scope: scope, Content: content, SourceAgent: "codex",
			Trust: memory.TrustAgent, Confidence: 0.9, Status: memory.StatusActive})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	dupA := mk(memory.TypeProject, "Use pnpm, not npm, to install packages.")
	dupB := mk(memory.TypeProject, "Use pnpm, not npm, to install packages in this repository.")
	portA := mk(memory.TypeProject, "The API server listens on port 8080 in development.")
	portB := mk(memory.TypeProject, "The API server listens on port 9090 in development.")
	for i := range 7 {
		s := mk(memory.TypeEpisodic, "Session summary: worked on the importer in internal/store/archive.go, step "+strconv.Itoa(i)+".")
		if _, err := rt.pool.Exec(ctx, `UPDATE memory SET created_at = now() - interval '60 days' + $2 * interval '1 hour' WHERE id = $1`, s.ID, i); err != nil {
			t.Fatal(err)
		}
	}

	out, errOut, err := runCLI(t, env, "consolidate", "run")
	if err != nil || !strings.Contains(out, "Judged 2 pairs; 3 new proposals") || judged.Load() != 4 { // two readings per pair
		t.Fatalf("consolidate run: %v (judged %d)\nout:\n%s\nerr:\n%s", err, judged.Load(), out, errOut)
	}
	out, _, err = runCLI(t, env, "consolidate", "list")
	for _, want := range []string{"duplicate in " + scope, "conflict in " + scope, "digest in " + scope, "keep   " + dupB.ID, "retire " + dupA.ID,
		"keep   " + portB.ID, "digest: Digest of 7 sessions"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("consolidate list lacks %q (%v):\n%s", want, err, out)
		}
	}
	if out, _, err := runCLI(t, env, "consolidate", "status"); err != nil || !strings.Contains(out, "3 pending, 0 applied") {
		t.Errorf("consolidate status: %v\n%s", err, out)
	}
	ids := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if m := regexp.MustCompile(`^([0-9a-f-]{36})  (\w+) in `).FindStringSubmatch(line); m != nil {
			ids[m[2]] = m[1]
		}
	}
	if len(ids) != 3 {
		t.Fatalf("proposal ids = %v", ids)
	}

	// The dashboard shows the proposals, applies one, and rejects one.
	const password = "a consolidation owner password"
	if err := rt.oauthDB.SetOwnerPassword(ctx, password); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpHandler(cfg, rt, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	b := newBrowser(t, srv.URL)
	if st, _, _ := b.do(http.MethodPost, "/dashboard/login", url.Values{"password": {password}}, nil); st != http.StatusSeeOther {
		t.Fatalf("login: %d", st)
	}
	st, body := b.get("/dashboard/consolidation")
	for _, want := range []string{`href="/dashboard/consolidation" aria-current="page"`, "Retire the duplicate", "Retire the older one", "Replace with the digest",
		"B says what A says.", "Digest of 7 sessions", `<span class="count" aria-label="3 waiting">3</span>`} {
		if st != 200 || !strings.Contains(body, want) {
			t.Errorf("consolidation page (%d) lacks %q", st, want)
		}
	}
	if _, body := b.get("/dashboard/"); !strings.Contains(body, `<span class="big">3</span> consolidation proposals`) {
		t.Error("overview lacks the proposal count")
	}
	if st, h := b.post("/dashboard/consolidation/"+ids["duplicate"]+"/apply", url.Values{"back": {"/dashboard/consolidation"}}); st != http.StatusSeeOther || h.Get("Location") != "/dashboard/consolidation" {
		t.Errorf("apply: %d %q", st, h.Get("Location"))
	}
	if m, _ := rt.store.Get(ctx, dupA.ID); m.Status != memory.StatusSuperseded {
		t.Errorf("retired duplicate status = %s", m.Status)
	}
	if m, _ := rt.store.Get(ctx, dupB.ID); m.Status != memory.StatusActive {
		t.Errorf("kept duplicate status = %s", m.Status)
	}
	if _, body := b.get("/dashboard/consolidation"); !strings.Contains(body, "Retired the duplicate; it is kept as history.") || strings.Contains(body, "B says what A says.") {
		t.Error("no confirmation after apply, or the proposal is still listed")
	}
	if st, _ := b.post("/dashboard/consolidation/"+ids["conflict"]+"/reject", nil); st != http.StatusSeeOther {
		t.Errorf("reject: %d", st)
	}
	for _, m := range []store.Memory{portA, portB} {
		if got, _ := rt.store.Get(ctx, m.ID); got.Status != memory.StatusActive {
			t.Errorf("rejected conflict changed a memory: %s", got.Status)
		}
	}
	if st, _ := b.post("/dashboard/consolidation/"+ids["conflict"]+"/apply", nil); st != http.StatusSeeOther {
		t.Errorf("apply after reject: %d", st)
	} else if _, body := b.get("/dashboard/consolidation"); !strings.Contains(body, "Not applied: that proposal was already decided") {
		t.Error("applying a rejected proposal was not refused")
	}
	if _, body := b.get("/dashboard/memories/" + dupB.ID); !strings.Contains(body, "replaces →") {
		t.Error("the kept memory does not link the retired one")
	}

	// The CLI applies the digest; nothing is proposed again.
	if out, _, err := runCLI(t, env, "consolidate", "apply", ids["digest"]); err != nil || !strings.Contains(out, "Replaced 7 session summaries with a digest") {
		t.Errorf("consolidate apply: %v\n%s", err, out)
	}
	if _, errOut, err := runCLI(t, env, "consolidate", "apply", ids["digest"]); err == nil || !strings.Contains(errOut, "already decided") {
		t.Errorf("second apply: %v\n%s", err, errOut)
	}
	var digests int
	if err := rt.pool.QueryRow(ctx, `SELECT count(*) FROM memory WHERE status = 'active' AND type = 'episodic' AND attrs->>'kind' = 'digest'`).Scan(&digests); err != nil || digests != 1 {
		t.Errorf("active digests = %d, %v", digests, err)
	}
	judged.Store(0)
	if out, _, err := runCLI(t, env, "consolidate", "run"); err != nil || !strings.Contains(out, "Judged 0 pairs; 0 new proposals") || judged.Load() != 0 {
		t.Errorf("second run: %v (judged %d)\n%s", err, judged.Load(), out)
	}
	if _, _, err := runCLI(t, map[string]string{"KENFOLD_DATABASE_URL": dbURL}, "consolidate", "run"); err == nil || !strings.Contains(err.Error(), "KENFOLD_CHAT_URL") {
		t.Errorf("run without a chat model: %v", err)
	}
}
