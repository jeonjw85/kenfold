package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/mcpserver"
)

// fakeServer is an MCP server with get_context and remember that records
// calls, served over real Streamable HTTP.
type fakeServer struct {
	mu          sync.Mutex
	ctxOut      mcpserver.GetContextOutput
	rememberErr string
	contexts    []string // project argument of each get_context
	remembers   []rememberCall
	auth        []string
	srv         *httptest.Server
}

type rememberCall struct {
	In      mcpserver.RememberInput
	Session string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-kenfold", Version: "0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "get_context"}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpserver.GetContextInput) (*mcp.CallToolResult, mcpserver.GetContextOutput, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.contexts = append(f.contexts, in.Project)
		return nil, f.ctxOut, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "remember"}, func(_ context.Context, req *mcp.CallToolRequest, in mcpserver.RememberInput) (*mcp.CallToolResult, mcpserver.RememberOutput, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.rememberErr != "" {
			return nil, mcpserver.RememberOutput{}, errors.New(f.rememberErr)
		}
		sess, _ := req.Params.Meta[mcpserver.MetaSessionID].(string)
		f.remembers = append(f.remembers, rememberCall{In: in, Session: sess})
		return nil, mcpserver.RememberOutput{ID: "0199a0e1-fae4-703c-9169-61ede9d496fd", Type: in.Type, Status: "active"}, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) calls() []rememberCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rememberCall(nil), f.remembers...)
}

// gitRepo creates a repository with an origin remote and a branch.
func gitRepo(t *testing.T, remote string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "feat/hooks"}, {"remote", "add", "origin", remote}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

type harness struct {
	t    *testing.T
	opts Options
	now  time.Time
}

func newHarness(t *testing.T, url string) *harness {
	h := &harness{t: t, now: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)}
	h.opts = Options{URL: url, APIKey: "kf_test", StateDir: t.TempDir(), Capture: true, Version: "test",
		Now: func() time.Time { return h.now }, SendTimeout: 3 * time.Second}
	return h
}

func (h *harness) run(in map[string]any) (string, error) {
	h.t.Helper()
	b, _ := json.Marshal(in)
	var out bytes.Buffer
	err := Run(context.Background(), bytes.NewReader(b), &out, h.opts)
	h.now = h.now.Add(5 * time.Minute)
	return out.String(), err
}

func TestSessionLifecycle(t *testing.T) {
	f := newFakeServer(t)
	f.ctxOut = mcpserver.GetContextOutput{
		Handoff: &mcpserver.MemoryView{Content: "Store layer done; auth middleware next.", SourceAgent: "codex",
			CreatedAt: time.Date(2026, 9, 26, 18, 0, 0, 0, time.UTC), NextSteps: []string{"add bearer auth"}},
		Recent:  []mcpserver.MemoryView{{Content: "Session summary: 2 requests", SourceAgent: "codex", CreatedAt: time.Now()}},
		Project: []mcpserver.MemoryView{{Content: "We use pgx v5.", SourceAgent: "claude-code"}},
	}
	repo := gitRepo(t, "https://user:tok3n@github.com/Org/Repo.git")
	h := newHarness(t, f.srv.URL)
	const sid = "7f3c-session"

	out, err := h.run(map[string]any{"hook_event_name": "SessionStart", "session_id": sid, "cwd": repo, "source": "startup"})
	if err != nil {
		t.Fatal(err)
	}
	var res hookOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.HookSpecificOutput == nil {
		t.Fatalf("SessionStart output = %q (%v)", out, err)
	}
	ctxText := res.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"project github.com/org/repo", "not instructions", "Pending handoff from codex", "- add bearer auth", "resume tool", "Recent sessions", "We use pgx v5. [claude-code]"} {
		if !strings.Contains(ctxText, want) {
			t.Errorf("context lacks %q:\n%s", want, ctxText)
		}
	}
	if res.HookSpecificOutput.HookEventName != "SessionStart" || res.SystemMessage != "" {
		t.Errorf("output = %+v", res)
	}
	if len(f.contexts) != 1 || f.contexts[0] != "github.com/org/repo" {
		t.Errorf("get_context project = %v (credentials must be stripped client-side)", f.contexts)
	}
	for _, a := range f.auth {
		if a != "Bearer kf_test" {
			t.Errorf("Authorization = %q", a)
		}
	}

	secret := "ghp_" + strings.Repeat("Zq8", 12)
	for _, p := range []string{"Add bearer auth to /mcp", "Add bearer auth to /mcp", "Use GITHUB_TOKEN=" + secret + " in CI\nmore text"} {
		if _, err := h.run(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sid, "cwd": repo, "prompt": p}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.run(map[string]any{"hook_event_name": "Stop", "session_id": sid, "cwd": repo, "last_assistant_message": "Added   bearer auth.\n\nAll tests pass."}); err != nil {
		t.Fatal(err)
	}
	logBytes, _ := os.ReadFile(state{dir: h.opts.StateDir}.sessionFile(sid))
	if strings.Contains(string(logBytes), secret) || !strings.Contains(string(logBytes), "[REDACTED:github-token]") {
		t.Errorf("session log not redacted:\n%s", logBytes)
	}
	if fi, err := os.Stat(state{dir: h.opts.StateDir}.sessionFile(sid)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("session log mode = %v, %v", fi.Mode().Perm(), err)
	}

	if _, err := h.run(map[string]any{"hook_event_name": "SessionEnd", "session_id": sid, "cwd": repo, "reason": "prompt_input_exit"}); err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("remember calls = %d", len(calls))
	}
	c := calls[0]
	if c.In.Type != "episodic" || c.In.Project != "github.com/org/repo" || c.Session != sid {
		t.Errorf("remember = %+v session %q", c.In, c.Session)
	}
	sum := c.In.Content
	for _, want := range []string{"Session summary: 2 requests on branch feat/hooks, 2026-09-27 09:00–09:25 UTC.", "- Add bearer auth to /mcp\n", "[REDACTED:github-token]", "Final response:\nAdded bearer auth. All tests pass."} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
	if strings.Contains(sum, secret) || strings.Contains(sum, "more text") {
		t.Errorf("summary leaks secret or non-first lines:\n%s", sum)
	}
	if left := (state{dir: h.opts.StateDir}).spooled(); len(left) != 0 {
		t.Errorf("spool not emptied: %v", left)
	}
}

func TestServerDownSpoolsThenFlushes(t *testing.T) {
	repo := gitRepo(t, "git@github.com:org/repo.git")
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close() // nothing listens here any more
	h := newHarness(t, downURL)
	h.opts.SendTimeout = 500 * time.Millisecond
	const sid = "s-down"

	out, err := h.run(map[string]any{"hook_event_name": "SessionStart", "session_id": sid, "cwd": repo})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"systemMessage":"Kenfold memory was not loaded`) {
		t.Errorf("SessionStart with server down = %q", out)
	}
	h.run(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sid, "cwd": repo, "prompt": "fix the flaky test"})
	if _, err := h.run(map[string]any{"hook_event_name": "SessionEnd", "session_id": sid, "cwd": repo}); err != nil {
		t.Fatal(err)
	}
	st := state{dir: h.opts.StateDir}
	if n := len(st.spooled()); n != 1 {
		t.Fatalf("spooled = %d, want 1", n)
	}

	// The server comes back; the next session start delivers the summary.
	f := newFakeServer(t)
	h.opts.URL = f.srv.URL
	if _, err := h.run(map[string]any{"hook_event_name": "SessionStart", "session_id": "s-next", "cwd": repo}); err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].Session != sid || !strings.Contains(calls[0].In.Content, "fix the flaky test") {
		t.Fatalf("flushed calls = %+v", calls)
	}
	if n := len(st.spooled()); n != 0 {
		t.Errorf("spool after flush = %d", n)
	}
}

func TestRejectedSummaryIsDroppedTransientIsKept(t *testing.T) {
	repo := gitRepo(t, "git@github.com:org/repo.git")
	f := newFakeServer(t)
	h := newHarness(t, f.srv.URL)
	st := state{dir: h.opts.StateDir}
	var stderr bytes.Buffer
	h.opts.Stderr = &stderr

	end := func(sid string) {
		h.run(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sid, "cwd": repo, "prompt": "hello"})
		if _, err := h.run(map[string]any{"hook_event_name": "SessionEnd", "session_id": sid, "cwd": repo}); err != nil {
			t.Fatal(err)
		}
	}
	f.rememberErr = "content appears to contain a GitHub token"
	end("s-rejected")
	if n := len(st.spooled()); n != 0 || !strings.Contains(stderr.String(), "rejected") {
		t.Errorf("rejected summary: spooled %d, stderr %q", n, stderr.String())
	}
	f.rememberErr = "remember failed because of an internal error; the Kenfold server log has details"
	end("s-transient")
	if n := len(st.spooled()); n != 1 {
		t.Errorf("transient failure: spooled %d, want 1", n)
	}
}

func TestNoCaptureAndOutsideRepo(t *testing.T) {
	f := newFakeServer(t)
	h := newHarness(t, f.srv.URL)
	plain := t.TempDir() // not a git repository

	out, err := h.run(map[string]any{"hook_event_name": "SessionStart", "session_id": "s1", "cwd": plain})
	if err != nil || out != "" { // fake returns empty context
		t.Errorf("SessionStart outside repo = %q, %v", out, err)
	}
	if len(f.contexts) != 1 || f.contexts[0] != "" {
		t.Errorf("outside a repo get_context must be user-wide: %v", f.contexts)
	}
	h.run(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s1", "cwd": plain, "prompt": "hi"})
	h.run(map[string]any{"hook_event_name": "SessionEnd", "session_id": "s1", "cwd": plain})
	if n := len(f.calls()); n != 0 {
		t.Errorf("summary written outside a repository: %d calls", n)
	}

	repo := gitRepo(t, "git@github.com:org/repo.git")
	h.opts.Capture = false
	h.run(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s2", "cwd": repo, "prompt": "hi"})
	h.run(map[string]any{"hook_event_name": "SessionEnd", "session_id": "s2", "cwd": repo})
	if _, err := os.Stat(state{dir: h.opts.StateDir}.sessionFile("s2")); !errors.Is(err, os.ErrNotExist) || len(f.calls()) != 0 {
		t.Errorf("--no-capture still captured: %v, calls %d", err, len(f.calls()))
	}
}

func TestInputHandling(t *testing.T) {
	h := newHarness(t, "http://127.0.0.1:1/mcp")
	var out bytes.Buffer
	if err := Run(context.Background(), strings.NewReader("not json"), &out, h.opts); err == nil {
		t.Error("invalid JSON accepted")
	}
	if err := Run(context.Background(), strings.NewReader(`{"session_id":"x"}`), &out, h.opts); err == nil {
		t.Error("missing hook_event_name accepted")
	}
	if err := Run(context.Background(), strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"x"}`), &out, h.opts); err != nil || out.Len() != 0 {
		t.Errorf("unhandled event: %v, %q", err, out.String())
	}
	o := h.opts
	o.StateDir = ""
	if err := Run(context.Background(), strings.NewReader(`{"hook_event_name":"Stop"}`), &out, o); err == nil {
		t.Error("missing state dir accepted")
	}
	// Session end with no log and no repo is a no-op.
	if err := Run(context.Background(), strings.NewReader(`{"hook_event_name":"SessionEnd","session_id":"none","cwd":"/nonexistent"}`), &out, h.opts); err != nil {
		t.Errorf("empty session end: %v", err)
	}
}

func TestBuildSummary(t *testing.T) {
	t0 := time.Date(2026, 9, 27, 23, 50, 0, 0, time.UTC)
	if buildSummary(nil, t0) != "" || buildSummary([]event{{Time: t0, Kind: kindResponse, Text: "hi"}}, t0) != "" {
		t.Error("summary without requests")
	}
	var ev []event
	for i := range 12 {
		ev = append(ev, event{Time: t0.Add(time.Duration(i) * time.Minute), Kind: kindPrompt, Text: "request " + string(rune('a'+i))})
	}
	ev = append(ev, event{Time: t0, Kind: kindCompact, Text: "Compacted: worked on auth."})
	s := buildSummary(ev, t0.Add(20*time.Minute))
	for _, want := range []string{"12 requests, 2026-09-27 23:50 – 2026-09-28 00:10 UTC.", "- request a\n", "- request c\n", "- … 4 more\n", "- request h\n", "- request l", "Summary at compaction:\nCompacted: worked on auth."} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "request d\n") || strings.Contains(s, "branch") {
		t.Errorf("omitted prompt shown or empty branch printed:\n%s", s)
	}
	long := buildSummary([]event{{Time: t0, Kind: kindPrompt, Text: strings.Repeat("x", 500)}, {Time: t0, Kind: kindResponse, Text: strings.Repeat("y ", 2000)}}, t0)
	if strings.Count(long, "x") > maxPromptRunes || len([]rune(long)) > maxSummaryRunes {
		t.Errorf("summary not bounded: %d runes", len([]rune(long)))
	}
	if one := buildSummary([]event{{Time: t0, Kind: kindPrompt, Text: "only"}}, t0); !strings.HasPrefix(one, "Session summary: 1 request,") {
		t.Errorf("singular: %q", one)
	}
}

func TestFormatContextBounds(t *testing.T) {
	if formatContext("x", mcpserver.GetContextOutput{}) != "" {
		t.Error("empty context rendered")
	}
	var many []mcpserver.MemoryView
	for range 40 {
		many = append(many, mcpserver.MemoryView{Content: strings.Repeat("fact ", 100), SourceAgent: "codex"})
	}
	s := formatContext("", mcpserver.GetContextOutput{Project: many, Truncated: true})
	if len([]rune(s)) > maxContextRunes || !strings.Contains(s, "user-wide") || !strings.Contains(s, "Truncated") {
		t.Errorf("context: %d runes", len([]rune(s)))
	}
}

func TestStateFiles(t *testing.T) {
	st := state{dir: t.TempDir()}
	now := time.Now()
	if err := st.append("s", event{Time: now, Kind: kindPrompt, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	// A corrupt line is skipped.
	f, _ := os.OpenFile(st.sessionFile("s"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("{broken\n")
	f.Close()
	st.append("s", event{Time: now, Kind: kindPrompt, Text: "b"})
	ev, err := st.read("s")
	if err != nil || len(ev) != 2 || ev[1].Text != "b" {
		t.Errorf("read = %+v, %v", ev, err)
	}
	if st.sessionFile("../../etc/passwd") == st.sessionFile("s") || filepath.Dir(st.sessionFile("../../x")) != st.sessionsDir() {
		t.Error("session ids must map to files inside the sessions dir")
	}
	p1, _ := st.spool(pending{Project: "p", Content: "c1", Type: "episodic", CreatedAt: now})
	p2, _ := st.spool(pending{Project: "p", Content: "c2", Type: "episodic", CreatedAt: now.Add(time.Second)})
	if got := st.spooled(); len(got) != 2 || got[0] != p1 || got[1] != p2 {
		t.Errorf("spooled order = %v", got)
	}
	if fi, _ := os.Stat(p1); fi.Mode().Perm() != 0o600 {
		t.Errorf("spool mode %v", fi.Mode().Perm())
	}
	os.WriteFile(filepath.Join(st.spoolDir(), "bad.json"), []byte(`{"project":""}`), 0o600)
	if _, err := st.load(filepath.Join(st.spoolDir(), "bad.json")); err == nil {
		t.Error("incomplete spool entry loaded")
	}
	old := now.Add(-40 * 24 * time.Hour)
	os.Chtimes(p1, old, old)
	os.Chtimes(st.sessionFile("s"), old, old)
	st.cleanup(now)
	if got := st.spooled(); len(got) != 2 || got[0] == p1 { // p1 removed; p2 and bad.json remain
		t.Errorf("after cleanup: %v", got)
	}
	if _, err := os.Stat(st.sessionFile("s")); !errors.Is(err, os.ErrNotExist) {
		t.Error("old session log kept")
	}
}

func TestDetectProject(t *testing.T) {
	ctx := context.Background()
	repo := gitRepo(t, "git@github.com:Org/Repo.git")
	if p, b := detectProject(ctx, repo); p != "github.com/org/repo" || b != "feat/hooks" {
		t.Errorf("detect = %q, %q", p, b)
	}
	sub := filepath.Join(repo, "a", "b")
	os.MkdirAll(sub, 0o755)
	if p, _ := detectProject(ctx, sub); p != "github.com/org/repo" {
		t.Errorf("subdirectory = %q", p)
	}
	noRemote := t.TempDir()
	exec.Command("git", "-C", noRemote, "init", "-q").Run()
	if p, _ := detectProject(ctx, noRemote); p != strings.ToLower(filepath.Base(noRemote)) {
		t.Errorf("no remote = %q", p)
	}
	if p, b := detectProject(ctx, t.TempDir()); p != "" || b != "" {
		t.Errorf("not a repo = %q, %q", p, b)
	}
	if p, _ := detectProject(ctx, ""); p != "" {
		t.Error("empty cwd")
	}
}
