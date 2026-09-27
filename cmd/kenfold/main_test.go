package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// unreachableDB points at a closed port so tests never touch a real database.
const unreachableDB = "postgres://kenfold:kenfold@127.0.0.1:1/kenfold?sslmode=disable&connect_timeout=2"

// TestStdioEndToEnd builds the real binary and talks to `kenfold mcp` the way
// Claude Code, Codex, or OpenCode would: as a subprocess over stdin/stdout. The
// database is unreachable, which must not prevent startup or tools/list.
func TestStdioEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "kenfold")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.Command(bin, "mcp")
	cmd.Env = append(os.Environ(), "KENFOLD_DATABASE_URL="+unreachableDB, "KENFOLD_EMBED_URL=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v\nstderr: %s", err, stderr.String())
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(res.Tools) != 6 {
		t.Errorf("got %d tools, want 6", len(res.Tools))
	}

	// With the database down, tools fail as tool errors without leaking details.
	call, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "recall", Arguments: map[string]any{"query": "anything"}})
	if err != nil {
		t.Fatalf("recall: protocol error %v", err)
	}
	if !call.IsError {
		t.Fatal("recall with database down: want isError")
	}
	if text := call.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "internal error") || strings.Contains(text, "127.0.0.1:1") {
		t.Errorf("error text = %q; want a generic internal error", text)
	}
}

func runCLI(t *testing.T, env map[string]string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		if k == "KENFOLD_DATABASE_URL" {
			return unreachableDB
		}
		return ""
	}
	err = run(context.Background(), args, getenv, strings.NewReader(""), &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestCLIUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"frobnicate"},
		{"serve", "extra"},
		{"mcp", "extra"},
		{"reindex", "extra"},
		{"migrate", "sideways"},
		{"migrate", "up", "down"},
		{"key"},
		{"key", "rotate"},
		{"key", "create"},
		{"key", "create", "a", "b"},
		{"key", "create", "Claude Code"},
		{"key", "list", "extra"},
		{"key", "list", "--bogus"},
		{"key", "revoke"},
		{"memory"},
		{"memory", "edit"},
		{"memory", "list", "--status", "bogus"},
		{"memory", "list", "--type", "working"},
		{"memory", "list", "--limit", "0"},
		{"memory", "list", "--limit", "501"},
		{"memory", "list", "--scope", "https://"},
		{"memory", "list", "positional"},
		{"memory", "approve"},
		{"memory", "approve", "not-a-uuid"},
		{"memory", "forget", "not-a-uuid"},
	} {
		_, _, err := runCLI(t, nil, args...)
		if !errors.Is(err, errUsage) {
			t.Errorf("kenfold %v: err = %v; want a usage error (and no database access)", args, err)
		}
	}
}

func TestCLIVersionAndHelp(t *testing.T) {
	out, _, err := runCLI(t, nil, "version")
	if err != nil || !strings.HasPrefix(out, "kenfold ") {
		t.Errorf("version = %q, %v", out, err)
	}
	out, _, err = runCLI(t, nil, "help")
	for _, want := range []string{"kenfold key create <agent>", "kenfold memory approve <id>", "KENFOLD_AUTH", "KENFOLD_EMBED_URL"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("help missing %q (err %v)", want, err)
		}
	}
}

func TestCLIConfigAndDatabaseErrors(t *testing.T) {
	if _, _, err := runCLI(t, map[string]string{"KENFOLD_AUTH": "bogus"}, "serve"); err == nil || !strings.Contains(err.Error(), "KENFOLD_AUTH") {
		t.Errorf("bad config err = %v", err)
	}
	if _, _, err := runCLI(t, nil, "reindex"); err == nil || !strings.Contains(err.Error(), "KENFOLD_EMBED_URL") {
		t.Errorf("reindex without embeddings err = %v", err)
	}
	// Admin commands report an unreachable database clearly instead of hanging.
	start := time.Now()
	_, _, err := runCLI(t, nil, "key", "list")
	if err == nil || !strings.Contains(err.Error(), "KENFOLD_DATABASE_URL") || errors.Is(err, errUsage) {
		t.Errorf("key list with database down: err = %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("unreachable database took %v to report", time.Since(start))
	}
}

func TestParseArgsInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	reason := fs.String("reason", "", "")
	all := fs.Bool("all", false, "")
	pos, err := parseArgs(fs, []string{"id-1", "--reason", "wrong fact", "id-2", "--all"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pos, []string{"id-1", "id-2"}) || *reason != "wrong fact" || !*all {
		t.Errorf("pos=%v reason=%q all=%v", pos, *reason, *all)
	}
	if _, err := parseArgs(flag.NewFlagSet("t", flag.ContinueOnError), []string{"--nope"}); !errors.Is(err, errUsage) {
		t.Errorf("unknown flag err = %v", err)
	}
}

func TestHookCLI(t *testing.T) {
	// config: valid JSON, absolute binary path, quoted key file, per-client events.
	keyDir := filepath.Join(t.TempDir(), "my keys")
	for client, want := range map[string][]string{
		"claude-code": {"SessionStart", "UserPromptSubmit", "Stop", "PostCompact", "SessionEnd"},
		"codex":       {"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"},
	} {
		out, _, err := runCLI(t, nil, "hook", "config", client, "--key-file", filepath.Join(keyDir, "k"))
		if err != nil {
			t.Fatalf("%s: %v", client, err)
		}
		var cfg struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Type, Command string
					Timeout       int
				}
			}
		}
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatalf("%s: config is not JSON: %v\n%s", client, err, out)
		}
		if len(cfg.Hooks) != len(want) {
			t.Errorf("%s: events = %v", client, cfg.Hooks)
		}
		for _, ev := range want {
			h := cfg.Hooks[ev][0].Hooks[0]
			if h.Type != "command" || !filepath.IsAbs(strings.Trim(strings.Fields(h.Command)[0], "'")) ||
				!strings.Contains(h.Command, " hook --key-file '"+filepath.Join(keyDir, "k")+"'") || h.Timeout <= 0 {
				t.Errorf("%s %s: %+v", client, ev, h)
			}
		}
		if client == "codex" && cfg.Hooks["SessionEnd"][0].Hooks[0].Timeout > 3 {
			t.Error("codex SessionEnd timeout exceeds its 3 s maximum")
		}
	}
	if _, _, err := runCLI(t, nil, "hook", "config", "cursor"); !errors.Is(err, errUsage) {
		t.Errorf("unknown client: %v", err)
	}

	// The hook fails open: bad input, unreachable server, missing key file all exit 0.
	env := map[string]string{"KENFOLD_STATE_DIR": t.TempDir(), "KENFOLD_URL": "http://127.0.0.1:1/mcp"}
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"hook"}, func(k string) string { return env[k] }, strings.NewReader("garbage"), &out, &errOut); err != nil {
		t.Errorf("bad input: %v", err)
	}
	if !strings.Contains(errOut.String(), "not JSON") {
		t.Errorf("stderr = %q", errOut.String())
	}
	out.Reset()
	start := `{"hook_event_name":"SessionStart","session_id":"s","cwd":"/"}`
	if err := run(context.Background(), []string{"hook"}, func(k string) string { return env[k] }, strings.NewReader(start), &out, &errOut); err != nil {
		t.Errorf("server down: %v", err)
	}
	if !strings.Contains(out.String(), "systemMessage") {
		t.Errorf("server down output = %q", out.String())
	}
	if err := run(context.Background(), []string{"hook", "--key-file", "/nonexistent/key"}, func(k string) string { return env[k] }, strings.NewReader(start), &out, &errOut); err != nil {
		t.Errorf("missing key file: %v", err)
	}
	if _, _, err := runCLI(t, nil, "hook", "positional"); !errors.Is(err, errUsage) {
		t.Errorf("positional arg: %v", err)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/kenfold": "/usr/local/bin/kenfold",
		"/Users/me/My Apps/kf":   "'/Users/me/My Apps/kf'",
		"it's":                   `'it'\''s'`,
		"":                       "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("a\n\tb\x1b[31m  c", 80); got != "a b [31m c" {
		t.Errorf("oneLine = %q", got)
	}
	// n counts the ellipsis; trailing space before it is dropped.
	if got := oneLine("한국어 메모리 내용입니다", 5); got != "한국어…" {
		t.Errorf("truncate = %q", got)
	}
	if got := oneLine("abcdef", 4); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
}
