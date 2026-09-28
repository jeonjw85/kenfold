package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kenfold/kenfold/internal/buildinfo"
	"github.com/kenfold/kenfold/internal/hook"
)

// hookCmd runs `kenfold hook`. It is invoked by Claude Code or Codex with the
// event JSON on stdin. It always exits 0 so a Kenfold problem never blocks the
// agent; problems are reported on stderr (and at session start as a visible
// warning).
func hookCmd(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "config" {
		return hookConfig(args[1:], getenv, stdout)
	}
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	url := fs.String("url", envOr(getenv, "KENFOLD_URL", hook.DefaultURL), "Kenfold MCP endpoint")
	keyFile := fs.String("key-file", "", "file containing the agent's API key")
	keyEnv := fs.String("key-env", "KENFOLD_API_KEY", "environment variable holding the agent's API key")
	agent := fs.String("agent", "", "client name reported to the server (only used when the server runs without API keys)")
	noCapture := fs.Bool("no-capture", false, "load memory at session start, but do not record sessions")
	noRefs := fs.Bool("no-refs", false, "do not check the code that memories refer to at session start")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("hook takes flags only (or `hook config <claude-code|codex>`)")
	}

	key := getenv(*keyEnv)
	if *keyFile != "" {
		b, err := os.ReadFile(expandHome(*keyFile, getenv))
		if err != nil {
			fmt.Fprintf(stderr, "kenfold hook: read key file: %v\n", err)
			return nil
		}
		key = strings.TrimSpace(string(b))
	}
	stateDir := getenv("KENFOLD_STATE_DIR")
	if stateDir == "" {
		if stateDir, err = hook.DefaultStateDir(getenv); err != nil {
			fmt.Fprintf(stderr, "kenfold hook: %v\n", err)
			return nil
		}
	}
	err = hook.Run(ctx, stdin, stdout, hook.Options{
		URL:        *url,
		APIKey:     key,
		ClientName: *agent,
		StateDir:   stateDir,
		Capture:    !*noCapture,
		NoRefs:     *noRefs,
		Version:    buildinfo.Version,
		Stderr:     stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "kenfold hook: %v\n", err)
	}
	return nil // fail open
}

// hookConfig prints the hooks configuration for a client, using the absolute
// path of this binary so the hook works regardless of the client's PATH.
func hookConfig(args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hook config", flag.ContinueOnError)
	keyFile := fs.String("key-file", "", "file containing the agent's API key (recommended)")
	url := fs.String("url", "", "Kenfold MCP endpoint, if not "+hook.DefaultURL)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (pos[0] != "claude-code" && pos[0] != "codex") {
		return usageErr("hook config takes one client: claude-code or codex")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	cmd := shellQuote(exe) + " hook"
	if *keyFile != "" {
		abs, err := filepath.Abs(expandHome(*keyFile, getenv))
		if err != nil {
			return err
		}
		cmd += " --key-file " + shellQuote(abs)
	}
	if *url != "" {
		cmd += " --url " + shellQuote(*url)
	}

	type handler struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type group struct {
		Hooks []handler `json:"hooks"`
	}
	events := map[string][]group{}
	add := func(event string, timeout int) {
		events[event] = []group{{Hooks: []handler{{Type: "command", Command: cmd, Timeout: timeout}}}}
	}
	add("SessionStart", 10)
	add("UserPromptSubmit", 5)
	add("Stop", 5)
	if pos[0] == "claude-code" {
		add("PostCompact", 5) // Claude Code passes the compaction summary
		add("SessionEnd", 5)  // raises Claude Code's 1.5 s SessionEnd budget
	} else {
		add("SessionEnd", 3) // Codex allows at most 3 s
	}
	b, err := json.MarshalIndent(map[string]any{"hooks": events}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, string(b))
	return nil
}

// shellQuote quotes s for a POSIX shell (hooks run via sh -c).
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:=@", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func expandHome(p string, getenv func(string) string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home := getenv("HOME")
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

func envOr(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}
