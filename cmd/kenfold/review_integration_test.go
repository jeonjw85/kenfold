package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

// fakeChatServer is an OpenAI-compatible chat endpoint that returns answer.
func fakeChatServer(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": answer}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestReviewIntegration covers `kenfold extract run` and the review commands.
// It TRUNCATES memory; use a throwaway database.
func TestReviewIntegration(t *testing.T) {
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
	scope := "project:github.com/o/r"
	mk := func(typ memory.Type, content string, status memory.Status) store.Memory {
		m, err := st.Create(ctx, store.CreateParams{Type: typ, Scope: scope, Content: content, SourceAgent: "codex",
			SourceSession: "s-1", Trust: memory.TrustAgent, Confidence: 0.5, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	oldFriday := mk(memory.TypeProject, "We deploy on Fridays after the release review.", memory.StatusActive)
	mk(memory.TypeEpisodic, "Session summary: 3 requests.\nRequests:\n- From now on we never deploy on Fridays\n- Use goose for migrations\n- Add a README badge\nFinal response: Done.", memory.StatusActive)

	answer, _ := json.Marshal(map[string]any{"items": []any{
		map[string]any{"category": "project_rule", "content": "We never deploy on Fridays after the release review.", "evidence": "we never deploy on Fridays", "basis": "stated"},
		map[string]any{"category": "project_rule", "content": "Database migrations are managed with goose.", "evidence": "Use goose for migrations", "basis": "stated"},
		map[string]any{"category": "code_fact", "content": "The README has a status badge now.", "evidence": "Add a README badge", "basis": "inferred"},
	}})
	chat := fakeChatServer(t, string(answer))
	env := map[string]string{"KENFOLD_DATABASE_URL": url, "KENFOLD_CHAT_URL": chat.URL + "/v1"}

	out, errOut, err := runCLI(t, env, "extract", "run")
	if err != nil || strings.Count(out, "proposed") != 3 || !strings.Contains(errOut, "Processed 1 summaries (0 failed); stored 3 memories") {
		t.Fatalf("extract run: %v\nout:\n%s\nerr:\n%s", err, out, errOut)
	}
	if out, _, err := runCLI(t, env, "extract", "status"); err != nil || !strings.Contains(out, "0 pending, 0 running, 1 done, 0 failed; 3 memories extracted") {
		t.Errorf("extract status: %q, %v", out, err)
	}

	// Interactive review, oldest first: replace the Friday decision (#1), approve
	// goose, reject the badge.
	var rout, rerr bytes.Buffer
	getenv := func(k string) string { return env[k] }
	if err := run(ctx, []string{"memory", "review"}, getenv, strings.NewReader("x\n1\na\nr\n"), &rout, &rerr); err != nil {
		t.Fatalf("review: %v\n%s", err, rerr.String())
	}
	for _, want := range []string{"[1/3] project", "extracted by " + config.DefaultChatModel + " from a codex session", "evidence: “we never deploy on Fridays”",
		"similar #1 (active, codex): We deploy on Fridays", "[N] approve and replace similar #N", "Approved 2, rejected 1, skipped 0."} {
		if !strings.Contains(rout.String(), want) {
			t.Errorf("review output lacks %q:\n%s", want, rout.String())
		}
	}
	count := func(q string, args ...any) int {
		var n int
		pool.QueryRow(ctx, q, args...).Scan(&n)
		return n
	}
	if o, _ := st.Get(ctx, oldFriday.ID); o.Status != memory.StatusSuperseded {
		t.Errorf("old Friday decision = %s, want superseded", o.Status)
	}
	if n := count(`SELECT count(*) FROM memory WHERE status = 'active' AND trust = 'user' AND source_agent = 'kenfold-extractor'`); n != 2 {
		t.Errorf("approved extracted memories = %d, want 2", n)
	}
	if n := count(`SELECT count(*) FROM memory WHERE status = 'deleted' AND content LIKE '%badge%'`); n != 1 {
		t.Errorf("rejected = %d", n)
	}
	if out, _, _ := runCLI(t, env, "memory", "review"); !strings.Contains(out, "Nothing to review") {
		t.Errorf("second review: %q", out)
	}

	// Non-interactive approve/reject with several ids and partial failure.
	p1 := mk(memory.TypePreference, "The user prefers short answers.", memory.StatusProposed)
	p2 := mk(memory.TypePreference, "The user prefers tabs over spaces.", memory.StatusProposed)
	out, errOut, err = runCLI(t, env, "memory", "approve", p1.ID, oldFriday.ID)
	if err == nil || !strings.Contains(out, "approved "+p1.ID) || !strings.Contains(errOut, oldFriday.ID+": not awaiting review") {
		t.Errorf("approve partial: %v\n%s\n%s", err, out, errOut)
	}
	if out, _, err := runCLI(t, env, "memory", "reject", p2.ID, "--reason", "wrong"); err != nil || !strings.Contains(out, "rejected "+p2.ID) {
		t.Errorf("reject: %v %s", err, out)
	}
	if m, _ := st.Get(ctx, p2.ID); m.Status != memory.StatusDeleted || m.Attrs["forget_reason"] != "wrong" || m.Attrs["forgotten_by"] != cliAgent {
		t.Errorf("rejected attrs = %v", m.Attrs)
	}

	// Without a chat model, extract run explains what to set.
	if _, _, err := runCLI(t, map[string]string{"KENFOLD_DATABASE_URL": url}, "extract", "run"); err == nil || !strings.Contains(err.Error(), "KENFOLD_CHAT_URL") {
		t.Errorf("extract run without chat: %v", err)
	}
}
