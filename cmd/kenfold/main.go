// Command kenfold is the Kenfold memory server.
//
//	kenfold serve            HTTP server: /mcp (Streamable HTTP), /healthz, /readyz
//	kenfold mcp              MCP over stdio, for clients that spawn a subprocess
//	kenfold migrate [cmd]    database migrations: up (default) | down | status
//	kenfold version          print version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/buildinfo"
	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/httpserver"
	"github.com/kenfold/kenfold/internal/mcpserver"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

const usage = `Kenfold: one memory for all your AI agents.

Usage:
  kenfold serve                  run the HTTP server (MCP at /mcp)
  kenfold mcp                    run the MCP server over stdio
  kenfold migrate [up|down|status]
  kenfold version

Environment:
  KENFOLD_HTTP_ADDR      listen address        (default ` + config.DefaultHTTPAddr + `)
  KENFOLD_DATABASE_URL   PostgreSQL URL        (default: local compose database)
  KENFOLD_AUTO_MIGRATE   migrate on serve      (default false)
  KENFOLD_ALLOWED_HOSTS  Host allowlist for /mcp, comma-separated (default localhost,127.0.0.1,::1)
  KENFOLD_LOG_LEVEL      debug|info|warn|error (default info)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kenfold:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing command")
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "version", "--version", "-v":
		fmt.Printf("kenfold %s (commit %s, built %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Logs always go to stderr: in stdio mode stdout carries the MCP protocol.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	switch cmd {
	case "serve":
		return serve(ctx, cfg, logger)
	case "mcp":
		return serveStdio(ctx, cfg, logger)
	case "migrate":
		return migrate(ctx, cfg, logger, rest)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if cfg.AutoMigrate {
		v, err := migrations.Up(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		logger.Info("migrations applied", "version", v)
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database pool: %w", err)
	}
	defer pool.Close()

	mcpSrv := mcpserver.New(buildinfo.Version, store.New(pool))
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpserver.New(mcpSrv, pool, logger, cfg.AllowedHosts),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: MCP responses may be long-lived SSE streams.
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("kenfold listening", "addr", cfg.HTTPAddr, "mcp", httpserver.MCPPath, "version", buildinfo.Version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func serveStdio(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	logger.Debug("kenfold mcp (stdio) starting", "version", buildinfo.Version)

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database pool: %w", err)
	}
	defer pool.Close()

	err = mcpserver.New(buildinfo.Version, store.New(pool)).Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func migrate(ctx context.Context, cfg config.Config, logger *slog.Logger, args []string) error {
	sub := "up"
	if len(args) > 0 {
		sub = args[0]
	}

	p, err := migrations.Provider(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer p.Close()

	switch sub {
	case "up":
		results, err := p.Up(ctx)
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		for _, r := range results {
			logger.Info("applied", "migration", r.Source.Path, "duration", r.Duration)
		}
		v, err := p.GetDBVersion(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("schema version %d\n", v)
	case "down":
		r, err := p.Down(ctx)
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		fmt.Printf("rolled back %s\n", r.Source.Path)
	case "status":
		statuses, err := p.Status(ctx)
		if err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		for _, s := range statuses {
			applied := "-"
			if !s.AppliedAt.IsZero() {
				applied = s.AppliedAt.Format(time.RFC3339)
			}
			fmt.Printf("%-8s %-25s %s\n", s.State, s.Source.Path, applied)
		}
	default:
		return fmt.Errorf("unknown migrate command %q (want up|down|status)", sub)
	}
	return nil
}
