package main

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/mcpserver"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

// fakeEmbeddings is an OpenAI-compatible /v1/embeddings server producing
// deterministic bag-of-words vectors. Setting fail simulates an outage.
type fakeEmbeddings struct{ fail atomic.Bool }

func (f *fakeEmbeddings) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.fail.Load() {
		http.Error(w, "embedding backend down", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Input []string `json:"input"`
	}
	if r.URL.Path != "/v1/embeddings" || json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	data := make([]item, len(req.Input))
	for i, text := range req.Input {
		data[i] = item{Index: i, Embedding: bagOfWords(text)}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// synonyms lets the fake embedder model what real embedding models do and
// full-text search cannot: match different words with the same meaning.
var synonyms = map[string]string{"postgres": "postgresql", "db": "database", "library": "driver", "mapper": "orm"}

func bagOfWords(text string) []float32 {
	v := make([]float32, store.EmbeddingDim)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if s, ok := synonyms[w]; ok {
			w = s
		}
		h := fnv.New32a()
		h.Write([]byte(w))
		v[h.Sum32()%store.EmbeddingDim]++
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	if n == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / math.Sqrt(n))
	}
	return v
}

// dot is the cosine similarity of two unit vectors.
func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// agentSession connects to Kenfold over Streamable HTTP like a real client.
func agentSession(ctx context.Context, url, clientName, token string) (*mcp.ClientSession, error) {
	tr := &mcp.StreamableClientTransport{Endpoint: url + "/mcp", DisableStandaloneSSE: true, MaxRetries: -1}
	if token != "" {
		tr.HTTPClient = &http.Client{Transport: bearerRT{token}, Timeout: 30 * time.Second}
	}
	return mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "1.0"}, nil).Connect(ctx, tr, nil)
}

func callTool[Out any](t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) Out {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %s", name, res.Content[0].(*mcp.TextContent).Text)
	}
	var out Out
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: decode %s: %v", name, b, err)
	}
	return out
}

func callToolErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error %v; want a tool error", name, err)
	}
	if !res.IsError {
		t.Fatalf("%s: succeeded; want a tool error", name)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func findMemory(ms []mcpserver.MemoryView, id string) *mcpserver.MemoryView {
	for i := range ms {
		if ms[i].ID == id {
			return &ms[i]
		}
	}
	return nil
}

// TestCrossAgentIntegration is Kenfold's Phase 1 acceptance test: a decision
// remembered by Claude Code is recalled by Codex, over authenticated HTTP,
// through the same handler stack `kenfold serve` runs. It TRUNCATES memory and
// api_key, so run it only against a throwaway database:
//
//	KENFOLD_TEST_DATABASE_URL=postgres://... go test -run Integration ./cmd/kenfold/
func TestCrossAgentIntegration(t *testing.T) {
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, url); err != nil {
		t.Fatal(err)
	}

	emb := &fakeEmbeddings{}
	embSrv := httptest.NewServer(emb)
	defer embSrv.Close()

	cfg := config.Config{
		DatabaseURL:       url,
		AllowedHosts:      config.DefaultAllowedHosts,
		Auth:              config.AuthAPIKey,
		Embed:             config.Embed{URL: embSrv.URL + "/v1", Model: "fake-bow"},
		SearchMaxDistance: config.DefaultSearchMaxDistance,
	}
	rt, err := newRuntime(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := rt.pool.Exec(ctx, `TRUNCATE memory, api_key CASCADE`); err != nil {
		t.Fatal(err)
	}

	claudeKey, _, err := rt.keys.Create(ctx, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	codexKey, codexRec, err := rt.keys.Create(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(httpHandler(cfg, rt, slog.New(slog.DiscardHandler)))
	defer srv.Close()

	embeddingModel := func(id string) *string {
		t.Helper()
		var m *string
		if err := rt.pool.QueryRow(ctx, `SELECT embedding_model FROM memory WHERE id = $1`, id).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	// ---- Authentication ----
	if _, err := agentSession(ctx, srv.URL, "anon", ""); err == nil {
		t.Fatal("connected without an API key")
	}
	if _, err := agentSession(ctx, srv.URL, "anon", "kf_"+strings.Repeat("A", 43)); err == nil {
		t.Fatal("connected with an unknown API key")
	}

	// The client claims to be codex, but the key belongs to claude-code: the key wins.
	claude, err := agentSession(ctx, srv.URL, "codex-impostor", claudeKey)
	if err != nil {
		t.Fatalf("claude-code connect: %v", err)
	}
	defer claude.Close()
	codex, err := agentSession(ctx, srv.URL, "codex", codexKey)
	if err != nil {
		t.Fatalf("codex connect: %v", err)
	}
	defer codex.Close()

	// The two agents refer to the same repository by different remote URLs.
	const claudeRepo = "https://github.com/Kenfold/Kenfold.git"
	const codexRepo = "git@github.com:kenfold/kenfold.git"

	// ---- Claude Code remembers a decision and hands off ----
	decision := "We use pgx v5 as the PostgreSQL driver; do not add database/sql or an ORM."
	rem := callTool[mcpserver.RememberOutput](t, claude, "remember", map[string]any{"content": decision, "project": claudeRepo})
	if rem.Type != "project" || rem.Status != "active" || rem.Deduplicated {
		t.Fatalf("remember = %+v; want an active project memory", rem)
	}
	if m := embeddingModel(rem.ID); m == nil || *m != "fake-bow" {
		t.Errorf("memory not embedded through the HTTP embedder: %v", m)
	}
	again := callTool[mcpserver.RememberOutput](t, claude, "remember", map[string]any{"content": "  " + decision + " ", "project": claudeRepo})
	if !again.Deduplicated || again.ID != rem.ID {
		t.Errorf("remembering the same decision twice = %+v; want deduplicated %s", again, rem.ID)
	}

	ho := callTool[mcpserver.HandoffOutput](t, claude, "handoff", map[string]any{
		"summary":    "Implemented the store layer; the HTTP auth middleware is next.",
		"next_steps": []string{"add bearer auth to /mcp", "write the cross-agent test"},
		"project":    claudeRepo,
	})
	if d := time.Until(ho.ExpiresAt); d < 6*24*time.Hour || d > 7*24*time.Hour+time.Minute {
		t.Errorf("handoff expires in %v; want the 7-day default", d)
	}

	// ---- Codex recalls it ----
	rec := callTool[mcpserver.RecallOutput](t, codex, "recall", map[string]any{"query": "which PostgreSQL driver should I use?", "project": codexRepo})
	got := findMemory(rec.Memories, rem.ID)
	if got == nil {
		t.Fatalf("codex could not recall claude-code's decision; got %+v", rec.Memories)
	}
	if got.SourceAgent != "claude-code" || got.Scope != "project:github.com/kenfold/kenfold" || got.Content != decision || got.Score <= 0 {
		t.Errorf("recalled memory = %+v", got)
	}
	// Only the vector ranker can find a query that shares no full-text lexeme
	// with the memory. This proves queries are embedded through the HTTP stack,
	// not just writes. The preconditions keep the check honest if texts change.
	const synQuery = "postgres db library mapper"
	var lexical bool
	if err := rt.pool.QueryRow(ctx, `SELECT to_tsvector('simple', $1) @@ to_tsquery('simple', replace(plainto_tsquery('simple', $2)::text, '&', '|'))`,
		decision, synQuery).Scan(&lexical); err != nil || lexical {
		t.Fatalf("precondition: %q must not match the decision lexically (match=%v, err=%v)", synQuery, lexical, err)
	}
	if d := 1 - dot(bagOfWords(synQuery), bagOfWords(decision)); d >= cfg.SearchMaxDistance {
		t.Fatalf("precondition: cosine distance %.3f must be below the cutoff %.2f", d, cfg.SearchMaxDistance)
	}
	vecOnly := callTool[mcpserver.RecallOutput](t, codex, "recall", map[string]any{"query": synQuery, "project": codexRepo})
	if findMemory(vecOnly.Memories, rem.ID) == nil {
		t.Errorf("vector-only recall = %+v; want the decision (are query embeddings used?)", vecOnly.Memories)
	}

	ctx1 := callTool[mcpserver.GetContextOutput](t, codex, "get_context", map[string]any{"task": "add a database migration", "project": codexRepo})
	if findMemory(ctx1.Project, rem.ID) == nil {
		t.Errorf("get_context.project missing the decision: %+v", ctx1.Project)
	}
	if findMemory(ctx1.Relevant, rem.ID) != nil {
		t.Error("the decision appears twice in get_context (project and relevant)")
	}
	if ctx1.Handoff == nil || ctx1.Handoff.ID != ho.ID || ctx1.Handoff.SourceAgent != "claude-code" || len(ctx1.Handoff.NextSteps) != 2 {
		t.Fatalf("get_context.handoff = %+v", ctx1.Handoff)
	}

	res := callTool[mcpserver.ResumeOutput](t, codex, "resume", map[string]any{"project": codexRepo})
	if res.Handoff == nil || res.Handoff.ID != ho.ID || res.Handoff.ResumedBy != "codex" || res.Handoff.NextSteps[0] != "add bearer auth to /mcp" {
		t.Fatalf("resume = %+v", res.Handoff)
	}
	if ctx2 := callTool[mcpserver.GetContextOutput](t, codex, "get_context", map[string]any{"project": codexRepo}); ctx2.Handoff != nil {
		t.Error("a resumed handoff is still offered by get_context")
	}

	// ---- Codex supersedes the decision; history is kept ----
	newer := "We use pgx v5 with sqlc-generated queries; do not add an ORM."
	sup := callTool[mcpserver.RememberOutput](t, codex, "remember", map[string]any{"content": newer, "project": codexRepo, "supersedes": rem.ID})
	rec2 := callTool[mcpserver.RecallOutput](t, codex, "recall", map[string]any{"query": "PostgreSQL driver ORM", "project": claudeRepo})
	if findMemory(rec2.Memories, rem.ID) != nil || findMemory(rec2.Memories, sup.ID) == nil {
		t.Errorf("after supersede, recall = %+v; want only the new memory", rec2.Memories)
	}
	if old, err := rt.store.Get(ctx, rem.ID); err != nil || old.Status != "superseded" {
		t.Errorf("old decision status = %v, %v; want superseded (kept for history)", old.Status, err)
	}
	if msg := callToolErr(t, claude, "remember", map[string]any{"content": "x", "project": claudeRepo, "supersedes": rem.ID}); !strings.Contains(msg, "not active") {
		t.Errorf("superseding a superseded memory: %q", msg)
	}
	if msg := callToolErr(t, claude, "remember", map[string]any{"content": "x", "supersedes": sup.ID}); !strings.Contains(msg, "scope") {
		t.Errorf("superseding across scopes: %q", msg)
	}

	// ---- Preferences need the user's approval ----
	pref := callTool[mcpserver.RememberOutput](t, codex, "remember", map[string]any{"content": "The user prefers concise Korean explanations.", "type": "preference"})
	if pref.Status != "proposed" {
		t.Fatalf("preference status = %s; want proposed", pref.Status)
	}
	if c := callTool[mcpserver.GetContextOutput](t, claude, "get_context", map[string]any{}); findMemory(c.Preferences, pref.ID) != nil {
		t.Error("an unapproved preference was served")
	}
	if _, err := rt.store.Approve(ctx, pref.ID); err != nil { // what `kenfold memory approve` does
		t.Fatal(err)
	}
	c := callTool[mcpserver.GetContextOutput](t, claude, "get_context", map[string]any{"project": claudeRepo})
	if p := findMemory(c.Preferences, pref.ID); p == nil || p.Trust != "user" {
		t.Errorf("approved preference not served with trust=user: %+v", c.Preferences)
	}

	// ---- Embedding outage degrades to full-text search ----
	emb.fail.Store(true)
	outage := callTool[mcpserver.RememberOutput](t, claude, "remember", map[string]any{"content": "Redis is not used; caching happens in Postgres.", "project": claudeRepo})
	if m := embeddingModel(outage.ID); m != nil {
		t.Errorf("memory written during an outage has embedding_model %q", *m)
	}
	if r := callTool[mcpserver.RecallOutput](t, codex, "recall", map[string]any{"query": "is Redis used for caching", "project": codexRepo}); findMemory(r.Memories, outage.ID) == nil {
		t.Errorf("full-text fallback did not find the memory: %+v", r.Memories)
	}
	emb.fail.Store(false)
	if st, err := rt.store.Backfill(ctx, rt.embedder, backfillBatch); err != nil || st.Embedded < 1 {
		t.Errorf("backfill after outage = %+v, %v", st, err)
	}
	if m := embeddingModel(outage.ID); m == nil || *m != "fake-bow" {
		t.Errorf("backfill did not embed the outage memory: %v", m)
	}

	// ---- Forget records who forgot ----
	fg := callTool[mcpserver.ForgetOutput](t, codex, "forget", map[string]any{"id": outage.ID, "reason": "no longer true"})
	if fg.Status != "deleted" {
		t.Errorf("forget = %+v", fg)
	}
	if m, _ := rt.store.Get(ctx, outage.ID); m.Attrs["forgotten_by"] != "codex" || m.Attrs["forget_reason"] != "no longer true" {
		t.Errorf("forget attrs = %v", m.Attrs)
	}
	if msg := callToolErr(t, codex, "forget", map[string]any{"id": "not-a-uuid"}); !strings.Contains(msg, "not a memory id") {
		t.Errorf("forget with a bad id: %q", msg)
	}

	// ---- Provenance of every write matches the key, not the client name ----
	rows, err := rt.pool.Query(ctx, `SELECT DISTINCT source_agent FROM memory ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var agents []string
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		agents = append(agents, a)
	}
	if strings.Join(agents, ",") != "claude-code,codex" {
		t.Errorf("source agents = %v; want [claude-code codex]", agents)
	}

	// ---- Revocation takes effect on the next request ----
	if _, err := rt.keys.Revoke(ctx, codexRec.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := codex.CallTool(ctx, &mcp.CallToolParams{Name: "recall", Arguments: map[string]any{"query": "driver"}}); err == nil {
		t.Error("revoked key still works")
	}
	if _, err := claude.CallTool(ctx, &mcp.CallToolParams{Name: "recall", Arguments: map[string]any{"query": "driver"}}); err != nil {
		t.Errorf("revoking codex affected claude-code: %v", err)
	}
}
