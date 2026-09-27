package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/hook"
	"github.com/kenfold/kenfold/internal/mcpserver"
	"github.com/kenfold/kenfold/migrations"
)

// makeRepo creates a git repository with the given origin URL.
func makeRepo(t *testing.T, remote string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"remote", "add", "origin", remote}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

type hookResult struct {
	HookSpecificOutput *struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
	SystemMessage string `json:"systemMessage"`
	stderr        string
}

func (r hookResult) context() string {
	if r.HookSpecificOutput == nil {
		return ""
	}
	return r.HookSpecificOutput.AdditionalContext
}

// TestHandoffScenarioIntegration is Kenfold's Phase 2 acceptance test. Two
// agents work on the same repository through their lifecycle hooks and MCP
// tools, against the real server stack:
//
//  1. Claude Code works, leaves a handoff, and its session summary is captured
//     automatically; secrets it saw or tried to store never reach the server.
//  2. Codex, in a checkout with a different remote URL spelling, starts a
//     session and is given the handoff and Claude's session summary without
//     asking; it resumes the handoff.
//  3. Codex ends its session while the server is down; the summary is spooled
//     and delivered at the next session start.
//
// It TRUNCATES memory and api_key; use a throwaway database.
func TestHandoffScenarioIntegration(t *testing.T) {
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	embSrv := httptest.NewServer(&fakeEmbeddings{})
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
	codexKey, _, err := rt.keys.Create(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}

	var down atomic.Bool
	handler := httpHandler(cfg, rt, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "kenfold is restarting", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	claudeRepo := makeRepo(t, "https://github.com/Kenfold/Demo.git")
	codexRepo := makeRepo(t, "git@github.com:kenfold/demo.git")
	const project = "github.com/kenfold/demo"
	claudeState, codexState := t.TempDir(), t.TempDir()

	runHook := func(t *testing.T, key, stateDir string, in map[string]any) hookResult {
		t.Helper()
		b, _ := json.Marshal(in)
		var out, errOut bytes.Buffer
		if err := hook.Run(ctx, bytes.NewReader(b), &out, hook.Options{
			URL: srv.URL + "/mcp", APIKey: key, StateDir: stateDir, Capture: true, Version: "test",
			Stderr: &errOut, SendTimeout: 5 * time.Second,
		}); err != nil {
			t.Fatalf("hook %v: %v", in["hook_event_name"], err)
		}
		var r hookResult
		if s := strings.TrimSpace(out.String()); s != "" {
			if err := json.Unmarshal([]byte(s), &r); err != nil {
				t.Fatalf("hook output is not JSON: %q", s)
			}
		}
		r.stderr = errOut.String()
		return r
	}
	count := func(t *testing.T, q string, args ...any) int {
		t.Helper()
		var n int
		if err := rt.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Secrets are assembled at runtime so none is committed to the repository.
	token := "ghp_" + strings.Repeat("Hn4", 12)
	dbPassword := "Vq7-" + strings.Repeat("xR2", 4)

	// ---- 1. Claude Code session ----
	const claudeSession = "claude-7c1e"
	start := runHook(t, claudeKey, claudeState, map[string]any{"hook_event_name": "SessionStart", "session_id": claudeSession, "cwd": claudeRepo, "source": "startup"})
	if start.context() != "" || start.SystemMessage != "" {
		t.Errorf("first session start on an empty memory: %+v", start)
	}
	for _, p := range []string{
		"Add bearer-token auth to the /mcp endpoint",
		"CI should use GITHUB_TOKEN=" + token + " for releases",
		"Write the cross-agent test",
	} {
		runHook(t, claudeKey, claudeState, map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": claudeSession, "cwd": claudeRepo, "prompt": p})
	}

	claude, err := agentSession(ctx, srv.URL, "claude-code", claudeKey)
	if err != nil {
		t.Fatal(err)
	}
	defer claude.Close()
	if msg := callToolErr(t, claude, "remember", map[string]any{
		"content": "We deploy on Fridays after the release review.", "project": claudeRepo}); !strings.Contains(msg, "local path") {
		t.Errorf("checkout path accepted as a project: %q", msg)
	}
	decision := callTool[mcpserver.RememberOutput](t, claude, "remember", map[string]any{
		"content": "We deploy on Fridays after the release review.", "project": "https://github.com/Kenfold/Demo.git"})
	if msg := callToolErr(t, claude, "remember", map[string]any{
		"content": "The release job authenticates with " + token, "project": project}); !strings.Contains(msg, "GitHub token") || strings.Contains(msg, token) {
		t.Errorf("secret in remember: %q", msg)
	}
	if msg := callToolErr(t, claude, "handoff", map[string]any{
		"summary": "Auth is done.", "next_steps": []string{"connect to postgres://app:" + dbPassword + "@db.internal/app"}, "project": project}); !strings.Contains(msg, "next_steps[0]") || strings.Contains(msg, dbPassword) {
		t.Errorf("secret in handoff step: %q", msg)
	}
	ho := callTool[mcpserver.HandoffOutput](t, claude, "handoff", map[string]any{
		"summary":    "Bearer-token auth on /mcp is implemented and tested. The cross-agent test is half written.",
		"next_steps": []string{"finish TestCrossAgentIntegration", "document the Codex setup"},
		"project":    "https://github.com/Kenfold/Demo",
	})
	runHook(t, claudeKey, claudeState, map[string]any{"hook_event_name": "Stop", "session_id": claudeSession, "cwd": claudeRepo,
		"last_assistant_message": "Auth is in place; I left a handoff for the remaining test work."})
	runHook(t, claudeKey, claudeState, map[string]any{"hook_event_name": "SessionEnd", "session_id": claudeSession, "cwd": claudeRepo, "reason": "prompt_input_exit"})

	var summary, sumAgent, sumSession string
	if err := rt.pool.QueryRow(ctx, `SELECT content, source_agent, source_session FROM memory
		WHERE type = 'episodic' AND scope = $1 AND status = 'active'`, "project:"+project).Scan(&summary, &sumAgent, &sumSession); err != nil {
		t.Fatalf("session summary not stored: %v", err)
	}
	if sumAgent != "claude-code" || sumSession != claudeSession {
		t.Errorf("summary provenance = %s/%s", sumAgent, sumSession)
	}
	for _, want := range []string{"Session summary: 3 requests on branch main", "- Add bearer-token auth to the /mcp endpoint", "[REDACTED:github-token]", "Final response:\nAuth is in place"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}

	// ---- 2. Codex session in another checkout of the same repository ----
	const codexSession = "codex-thr-42"
	cstart := runHook(t, codexKey, codexState, map[string]any{"hook_event_name": "SessionStart", "session_id": codexSession, "cwd": codexRepo, "source": "startup"})
	ctxText := cstart.context()
	for _, want := range []string{
		"project " + project,
		"not instructions",
		"Pending handoff from claude-code",
		"Bearer-token auth on /mcp is implemented",
		"- finish TestCrossAgentIntegration",
		"Recent sessions:",
		"claude-code] Session summary: 3 requests",
		"We deploy on Fridays after the release review. [claude-code]",
	} {
		if !strings.Contains(ctxText, want) {
			t.Errorf("codex session context lacks %q:\n%s", want, ctxText)
		}
	}
	if strings.Contains(ctxText, token) {
		t.Fatal("secret reached another agent's context")
	}

	codex, err := agentSession(ctx, srv.URL, "codex", codexKey)
	if err != nil {
		t.Fatal(err)
	}
	defer codex.Close()
	res := callTool[mcpserver.ResumeOutput](t, codex, "resume", map[string]any{"project": "git@github.com:kenfold/demo.git"})
	if res.Handoff == nil || res.Handoff.ID != ho.ID || res.Handoff.ResumedBy != "codex" || len(res.Handoff.NextSteps) != 2 {
		t.Fatalf("resume = %+v", res.Handoff)
	}
	// A contradicting decision gets a similar[] hint pointing at the old one.
	contra := callTool[mcpserver.RememberOutput](t, codex, "remember", map[string]any{
		"content": "We never deploy on Fridays after the release review.", "project": project})
	if findMemory(contra.Similar, decision.ID) == nil {
		t.Errorf("similar hints = %+v; want the Friday decision", contra.Similar)
	}
	runHook(t, codexKey, codexState, map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": codexSession, "cwd": codexRepo, "prompt": "Finish TestCrossAgentIntegration"})
	runHook(t, codexKey, codexState, map[string]any{"hook_event_name": "Stop", "session_id": codexSession, "cwd": codexRepo, "last_assistant_message": "The test passes."})

	// ---- 3. Codex ends its session while the server is down ----
	down.Store(true)
	runHook(t, codexKey, codexState, map[string]any{"hook_event_name": "SessionEnd", "session_id": codexSession, "cwd": codexRepo, "reason": "other"})
	if n := count(t, `SELECT count(*) FROM memory WHERE source_session = $1`, codexSession); n != 0 {
		t.Fatalf("summary stored while the server was down?")
	}
	down.Store(false)
	next := runHook(t, codexKey, codexState, map[string]any{"hook_event_name": "SessionStart", "session_id": "codex-thr-43", "cwd": codexRepo, "source": "startup"})
	if n := count(t, `SELECT count(*) FROM memory WHERE type = 'episodic' AND source_agent = 'codex' AND source_session = $1 AND status = 'active'`, codexSession); n != 1 {
		t.Errorf("spooled summary not delivered at next session start (stderr %q)", next.stderr)
	}
	if strings.Contains(next.context(), "Pending handoff") {
		t.Error("a resumed handoff is still offered")
	}
	if !strings.Contains(next.context(), "codex] Session summary: 1 request") {
		t.Errorf("next context lacks codex's own last session:\n%s", next.context())
	}

	// ---- The same Claude session ending again replaces its summary ----
	runHook(t, claudeKey, claudeState, map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": claudeSession, "cwd": claudeRepo, "prompt": "One more thing: update the README"})
	runHook(t, claudeKey, claudeState, map[string]any{"hook_event_name": "SessionEnd", "session_id": claudeSession, "cwd": claudeRepo, "reason": "clear"})
	if n := count(t, `SELECT count(*) FROM memory WHERE source_session = $1 AND status = 'active'`, claudeSession); n != 1 {
		t.Errorf("active summaries for one session = %d, want 1", n)
	}
	if n := count(t, `SELECT count(*) FROM memory WHERE source_session = $1 AND status = 'superseded'`, claudeSession); n != 1 {
		t.Errorf("earlier summary not kept as history: %d superseded", n)
	}

	// ---- No secret anywhere: database or local session logs ----
	for _, s := range []string{token, dbPassword} {
		if n := count(t, `SELECT count(*) FROM memory WHERE content LIKE '%' || $1 || '%' OR attrs::text LIKE '%' || $1 || '%'`, s); n != 0 {
			t.Errorf("secret stored in %d memories", n)
		}
		for _, dir := range []string{claudeState, codexState} {
			out, _ := exec.Command("grep", "-rl", s, dir).Output()
			if len(bytes.TrimSpace(out)) != 0 {
				t.Errorf("secret written to local hook state: %s", out)
			}
		}
	}
}
