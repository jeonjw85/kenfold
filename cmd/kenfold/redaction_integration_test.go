package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/mcpserver"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

// TestSecretRedactionIntegration checks what leaves Kenfold for model APIs and
// what reaches audit metadata through MCP, dashboard forms, and CLI commands.
// It TRUNCATES memory, api_key, and oauth_owner; use a throwaway database.
func TestSecretRedactionIntegration(t *testing.T) {
	dbURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}

	type modelInput struct{ path, text string }
	inputs := make(chan modelInput, 8)
	vec := make([]float32, store.EmbeddingDim)
	vec[0] = 1
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input     []string `json:"input"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/embeddings":
			if len(req.Input) != 1 {
				http.Error(w, "expected one query", http.StatusBadRequest)
				return
			}
			inputs <- modelInput{r.URL.Path, req.Input[0]}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vec}}})
		case "/v1/rerank":
			inputs <- modelInput{r.URL.Path, req.Query}
			results := make([]map[string]any, len(req.Documents))
			for i := range results {
				results[i] = map[string]any{"index": i, "relevance_score": 0.9}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	cfg := config.Config{
		DatabaseURL: dbURL, AllowedHosts: config.DefaultAllowedHosts, Auth: config.AuthAPIKey,
		Embed:  config.Embed{URL: provider.URL + "/v1", Model: "redaction-test"},
		Rerank: config.Rerank{URL: provider.URL + "/v1", Model: "redaction-test"},
	}
	rt, err := newRuntime(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := rt.pool.Exec(ctx, `TRUNCATE memory, api_key, oauth_owner CASCADE`); err != nil {
		t.Fatal(err)
	}
	const password = "redaction dashboard owner password"
	if err := rt.oauthDB.SetOwnerPassword(ctx, password); err != nil {
		t.Fatal(err)
	}
	key, _, err := rt.keys.Create(ctx, "redaction-test")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpHandler(cfg, rt, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	cs, err := agentSession(ctx, srv.URL, "redaction-test", key)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	b := newBrowser(t, srv.URL)
	if st, _, _ := b.do(http.MethodPost, "/dashboard/login", url.Values{"password": {password}}, nil); st != http.StatusSeeOther {
		t.Fatalf("dashboard login: %d", st)
	}
	if st, _ := b.get("/dashboard/memories"); st != http.StatusOK {
		t.Fatalf("load dashboard CSRF token: %d", st)
	}
	seed, err := rt.store.Create(ctx, store.CreateParams{
		Type: memory.TypeSemantic, Scope: "user", Content: "Shared database note.", SourceAgent: "test",
		Trust: memory.TrustAgent, Status: memory.StatusActive, Embedding: vec, EmbeddingModel: "redaction-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "ghp_" + strings.Repeat("aB3", 12)
	const marker = "[REDACTED:github-token]"
	prefix := "shared " + strings.Repeat("한", 2000-utf8.RuneCountInString("shared ")-25)
	rerankPrefix := "shared " + strings.Repeat("한", 500-utf8.RuneCountInString("shared ")-25)
	for _, tc := range []struct{ name, query, want string }{
		{"ordinary", "shared database note", "shared database note"},
		{"whole token", "shared " + token, "shared " + marker},
		{"token across limit", prefix + token, prefix + marker},
		{"token across rerank limit", rerankPrefix + token, rerankPrefix + marker},
		{"ordinary over limit", strings.Repeat("한", 2100), strings.Repeat("한", 2000)},
		{"placeholder", "shared API_KEY=<your-token>", "shared API_KEY=<your-token>"},
	} {
		for _, source := range []string{"recall", "get_context", "dashboard"} {
			t.Run("search/"+source+"/"+tc.name, func(t *testing.T) {
				switch source {
				case "recall":
					out := callTool[mcpserver.RecallOutput](t, cs, source, map[string]any{"query": tc.query})
					if findMemory(out.Memories, seed.ID) == nil {
						t.Error("search lost the matching memory")
					}
				case "get_context":
					out := callTool[mcpserver.GetContextOutput](t, cs, source, map[string]any{"task": tc.query})
					if findMemory(out.Relevant, seed.ID) == nil {
						t.Error("task search lost the matching memory")
					}
				case "dashboard":
					if st, body := b.get("/dashboard/memories?q=" + url.QueryEscape(tc.query)); st != http.StatusOK || !strings.Contains(body, seed.Content) {
						t.Fatalf("dashboard search did not return the matching memory: %d", st)
					}
				}
				for range 2 {
					select {
					case got := <-inputs:
						want := tc.want
						// The rerank API has its own, smaller query limit.
						if got.path == "/v1/rerank" && utf8.RuneCountInString(want) > 500 {
							want = string([]rune(want)[:500])
						}
						if got.text != want {
							t.Errorf("%s received an incorrectly sanitized query (length %d, want %d)", got.path, utf8.RuneCountInString(got.text), utf8.RuneCountInString(want))
						}
					case <-time.After(time.Second):
						t.Fatal("search did not reach both model endpoints")
					}
				}
			})
		}
	}

	for _, source := range []struct {
		name, action string
		limit        int
	}{
		{"MCP", "forget", 1000},
		{"dashboard", "forget", 500},
		{"dashboard", "reject", 500},
		{"CLI", "forget", 1000},
		{"CLI", "reject", 1000},
	} {
		prefix := strings.Repeat("사", source.limit-25)
		for _, tc := range []struct{ name, reason, want string }{
			{"ordinary", "outdated deployment note", "outdated deployment note"},
			{"whole token", "exposed " + token, "exposed " + marker},
			{"token across limit", prefix + token, prefix + marker},
		} {
			t.Run("audit/"+source.name+"/"+source.action+"/"+tc.name, func(t *testing.T) {
				status := memory.StatusActive
				if source.action == "reject" {
					status = memory.StatusProposed
				}
				m, err := rt.store.Create(ctx, store.CreateParams{Type: memory.TypeSemantic, Scope: "user", Content: "A note to retire.",
					SourceAgent: "test", Trust: memory.TrustAgent, Status: status})
				if err != nil {
					t.Fatal(err)
				}
				switch source.name {
				case "MCP":
					callTool[mcpserver.ForgetOutput](t, cs, "forget", map[string]any{"id": m.ID, "reason": tc.reason})
				case "dashboard":
					path := "/dashboard/memories/" + m.ID + "/forget"
					if source.action == "reject" {
						path = "/dashboard/review/" + m.ID + "/reject"
					}
					if st, _ := b.post(path, url.Values{"reason": {tc.reason}}); st != http.StatusSeeOther {
						t.Fatalf("dashboard %s: %d", source.action, st)
					}
				case "CLI":
					if _, _, err := runCLI(t, map[string]string{"KENFOLD_DATABASE_URL": dbURL}, "memory", source.action, m.ID, "--reason", tc.reason); err != nil {
						t.Fatal(err)
					}
				}
				got, err := rt.store.Get(ctx, m.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Status != memory.StatusDeleted {
					t.Errorf("memory status = %s, want deleted", got.Status)
				}
				if got.Attrs["forget_reason"] != tc.want {
					t.Error("stored audit reason was not correctly redacted before truncation")
				}
			})
		}
	}
}
