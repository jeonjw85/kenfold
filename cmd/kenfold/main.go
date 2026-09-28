// Command kenfold is the Kenfold memory server.
//
//	kenfold serve            HTTP server: /mcp (Streamable HTTP), /healthz, /readyz
//	kenfold mcp              MCP over stdio, for clients that spawn a subprocess
//	kenfold migrate [cmd]    database migrations: up (default) | down | status
//	kenfold key ...          manage API keys (one per agent)
//	kenfold memory ...       review memories (list, approve proposed ones, forget)
//	kenfold refs ...         check code referenced by memories (sync in a repository, status)
//	kenfold reindex          embed memories that have no embedding for the configured model
//	kenfold version          print version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kenfold/kenfold/internal/buildinfo"
	"github.com/kenfold/kenfold/internal/config"
)

const usage = `Kenfold: one memory for all your AI agents.

Usage:
  kenfold serve                        run the HTTP server (MCP at /mcp)
  kenfold mcp                          run the MCP server over stdio
  kenfold migrate [up|down|status]     manage the database schema

  kenfold key create <agent>           create an API key for an agent (e.g. claude-code, codex)
  kenfold key list [--all]             list API keys (--all includes revoked keys)
  kenfold key revoke <id|prefix>       revoke an API key

  kenfold memory list [flags]          list memories (--status, --type, --scope, --stale, --limit)
  kenfold memory review [--scope S]    go through proposed memories: approve, reject, or replace
  kenfold memory approve <id>... [--replaces ID]
                                       approve proposed memories (optionally replacing an active one)
  kenfold memory reject <id>... [--reason R]
                                       reject proposed memories
  kenfold memory forget <id> [--reason R]
                                       soft-delete a memory

  kenfold extract status               progress of memory extraction from session summaries
  kenfold extract run [--limit N]      extract memories now (needs KENFOLD_CHAT_URL)

  kenfold refs sync [--dir D] [--key-file F] [--quiet]
                                       check the code memories refer to against the repository's HEAD
  kenfold refs status [--scope S]      code reference states and memories that may be outdated

  kenfold reindex                      embed memories missing an embedding for the configured model
  kenfold scan [--redact]              find (and remove) secrets stored before the secret filter

  kenfold hook [--key-file F] [--no-capture] [--no-refs]
                                       lifecycle hook for Claude Code and Codex (reads the event on stdin)
  kenfold hook config <claude-code|codex> [--key-file F]
                                       print the hooks configuration for a client
  kenfold version

Environment:
  KENFOLD_HTTP_ADDR            listen address             (default ` + config.DefaultHTTPAddr + `)
  KENFOLD_DATABASE_URL         PostgreSQL URL             (default: local compose database)
  KENFOLD_AUTO_MIGRATE         migrate on serve           (default false)
  KENFOLD_ALLOWED_HOSTS        Host allowlist for /mcp    (default localhost,127.0.0.1,::1)
  KENFOLD_AUTH                 apikey | none              (default apikey)
  KENFOLD_AGENT                agent name for stdio sessions, e.g. claude-code
  KENFOLD_EMBED_URL            OpenAI-compatible embeddings base URL, e.g. http://127.0.0.1:11434/v1
                               (unset: full-text search only)
  KENFOLD_EMBED_MODEL          embedding model            (default ` + config.DefaultEmbedModel + `, must be 1024-dimensional)
  KENFOLD_EMBED_NAME           model name recorded with vectors, if KENFOLD_EMBED_MODEL is a local alias
  KENFOLD_EMBED_API_KEY        API key for the embeddings provider
  KENFOLD_EMBED_DIMENSIONS     send dimensions=1024 to the provider (default false)
  KENFOLD_SEARCH_MAX_DISTANCE  cosine distance cutoff for vector matches (default 0.55)
  KENFOLD_RERANK_URL           rerank API base URL (llama-server, Jina, Cohere, Voyage), e.g. http://127.0.0.1:8080/v1
                               (unset: no reranking)
  KENFOLD_RERANK_MODEL         reranker model             (default ` + config.DefaultRerankModel + `)
  KENFOLD_RERANK_API_KEY       API key for the rerank provider
  KENFOLD_CHAT_URL             OpenAI-compatible chat base URL for extraction and classification
                               (unset: no model-based extraction)
  KENFOLD_CHAT_MODEL           chat model                 (default ` + config.DefaultChatModel + `)
  KENFOLD_CHAT_API_KEY         API key for the chat provider
  KENFOLD_CHAT_REASONING       reasoning_effort sent to the model (default none; omit to leave it out)
  KENFOLD_EXTRACT              extract memories from session summaries (default: on with a chat model)
  KENFOLD_EXTRACT_POLICY       propose (review everything) | auto   (default propose)
  KENFOLD_CLASSIFY             classify memories stored without a type (default: on with a chat model)
  KENFOLD_LOG_LEVEL            debug|info|warn|error      (default info)

Hook and refs sync environment:
  KENFOLD_URL                  MCP endpoint               (default http://127.0.0.1:7077/mcp)
  KENFOLD_API_KEY              the agent's API key, unless --key-file or --key-env is given
  KENFOLD_STATE_DIR            local session logs and spool (default ~/.local/state/kenfold)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "kenfold:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// errUsage marks command-line mistakes (exit status 2).
var errUsage = errors.New("usage")

func usageErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s (see `kenfold help`)", errUsage, fmt.Sprintf(format, args...))
}

// cli carries what every command needs.
type cli struct {
	cfg    config.Config
	logger *slog.Logger
	in     io.Reader // interactive input (stdin)
	out    io.Writer // command output (stdout)
	errOut io.Writer // diagnostics (stderr)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return usageErr("missing command")
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "kenfold %s (commit %s, built %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return nil
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return nil
	case "hook":
		// Runs inside Claude Code / Codex: independent of server configuration.
		return hookCmd(ctx, rest, getenv, stdin, stdout, stderr)
	case "refs":
		if len(rest) > 0 && rest[0] == "sync" {
			// Runs in the repository, talks to the server's API: no database.
			return refsSync(ctx, rest[1:], getenv, stdout, stderr)
		}
	}

	cfg, err := config.LoadFrom(getenv)
	if err != nil {
		return err
	}
	// Logs always go to stderr: in stdio mode stdout carries the MCP protocol.
	c := &cli{
		cfg:    cfg,
		logger: slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel})),
		in:     stdin,
		out:    stdout,
		errOut: stderr,
	}

	switch cmd {
	case "serve":
		if len(rest) > 0 {
			return usageErr("serve takes no arguments")
		}
		return c.serve(ctx)
	case "mcp":
		if len(rest) > 0 {
			return usageErr("mcp takes no arguments")
		}
		return c.serveStdio(ctx)
	case "migrate":
		return c.migrate(ctx, rest)
	case "key":
		return c.key(ctx, rest)
	case "memory":
		return c.memory(ctx, rest)
	case "extract":
		return c.extract(ctx, rest)
	case "refs":
		return c.refs(ctx, rest)
	case "reindex":
		if len(rest) > 0 {
			return usageErr("reindex takes no arguments")
		}
		return c.reindex(ctx)
	case "scan":
		return c.scan(ctx, rest)
	default:
		fmt.Fprint(stderr, usage)
		return usageErr("unknown command %q", cmd)
	}
}

// parseArgs parses flags that may appear before, between, or after positional
// arguments (Go's flag package stops at the first positional argument).
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageErr("%s: %v", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}
