package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/apikey"
	"github.com/kenfold/kenfold/internal/buildinfo"
	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/embed"
	"github.com/kenfold/kenfold/internal/extract"
	"github.com/kenfold/kenfold/internal/httpserver"
	"github.com/kenfold/kenfold/internal/mcpserver"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

const (
	// backfillInterval is how often `serve` embeds memories written while the
	// embedding provider was unavailable.
	backfillInterval = 5 * time.Minute
	backfillBatch    = 64
	// extractInterval is how often `serve` looks for new session summaries.
	extractInterval = time.Minute
	probeTimeout    = 15 * time.Second
	schemaTimeout   = 10 * time.Second
)

// runtime holds the long-lived dependencies shared by the servers.
type runtime struct {
	pool     *pgxpool.Pool
	store    *store.Store
	keys     *apikey.Store
	embedder *embed.Client // nil when embeddings are disabled
	chat     *chat.Client  // nil when no chat model is configured
}

func newRuntime(ctx context.Context, cfg config.Config) (*runtime, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("database pool: %w", err)
	}
	rt := &runtime{pool: pool, store: store.New(pool), keys: apikey.NewStore(pool)}
	if cfg.Embed.Enabled() {
		rt.embedder, err = embed.New(embed.Config{
			BaseURL:        cfg.Embed.URL,
			Model:          cfg.Embed.Model,
			Name:           cfg.Embed.Name,
			APIKey:         cfg.Embed.APIKey,
			Dim:            store.EmbeddingDim,
			SendDimensions: cfg.Embed.SendDimensions,
		})
		if err != nil {
			pool.Close()
			return nil, err
		}
	}
	if cfg.Chat.Enabled() {
		rt.chat, err = chat.New(chat.Config{
			BaseURL:   cfg.Chat.URL,
			Model:     cfg.Chat.Model,
			APIKey:    cfg.Chat.APIKey,
			Reasoning: cfg.Chat.Reasoning,
		})
		if err != nil {
			pool.Close()
			return nil, err
		}
	}
	return rt, nil
}

// classifier returns the memory-type classifier, or nil when disabled.
func (rt *runtime) classifier(cfg config.Config) mcpserver.Classifier {
	if rt.chat == nil || !cfg.Classify {
		return nil
	}
	return extract.Classifier{Chat: rt.chat}
}

// extractor returns the background extraction worker, or nil when disabled.
func (rt *runtime) extractor(cfg config.Config, logger *slog.Logger) *extract.Worker {
	if rt.chat == nil || !cfg.Extract {
		return nil
	}
	return &extract.Worker{Store: rt.store, Chat: rt.chat, Embedder: rt.embedderIface(), Logger: logger, Policy: cfg.ExtractPolicy}
}

func (rt *runtime) Close() { rt.pool.Close() }

// embedderIface avoids storing a typed-nil *embed.Client in an interface,
// which would make "embeddings disabled" look enabled.
func (rt *runtime) embedderIface() store.Embedder {
	if rt.embedder == nil {
		return nil
	}
	return rt.embedder
}

// httpHandler builds the full HTTP stack exactly as `serve` runs it. HTTP
// callers are identified by API key, never by KENFOLD_AGENT.
func httpHandler(cfg config.Config, rt *runtime, logger *slog.Logger) http.Handler {
	deps := mcpserver.Deps{
		Store:       rt.store,
		Embedder:    rt.embedderIface(),
		Classifier:  rt.classifier(cfg),
		Logger:      logger,
		MaxDistance: cfg.SearchMaxDistance,
	}
	opts := httpserver.Options{AllowedHosts: cfg.AllowedHosts}
	if cfg.Auth == config.AuthAPIKey {
		opts.Verifier = rt.keys.TokenVerifier(logger)
	}
	return httpserver.New(mcpserver.New(buildinfo.Version, deps), rt.pool, logger, opts)
}

func (c *cli) serve(ctx context.Context) error {
	if c.cfg.AutoMigrate {
		v, err := migrations.Up(ctx, c.cfg.DatabaseURL)
		if err != nil {
			return err
		}
		c.logger.Info("migrations applied", "version", v)
	} else if err := c.checkSchema(ctx); err != nil {
		if errors.Is(err, migrations.ErrPending) {
			return err
		}
		// Keep starting: /readyz reports the database as unavailable until it is reachable.
		c.logger.Warn("database not reachable at startup", "err", err)
	}

	rt, err := newRuntime(ctx, c.cfg)
	if err != nil {
		return err
	}
	defer rt.Close()

	c.logSecurityPosture(ctx, rt)

	var wg sync.WaitGroup
	bgCtx, stopBackground := context.WithCancel(ctx)
	defer func() { stopBackground(); wg.Wait() }()
	if rt.embedder != nil {
		c.logger.Info("embeddings enabled", "endpoint", rt.embedder.Endpoint(), "model", rt.embedder.Model())
		wg.Go(func() {
			probeCtx, cancel := context.WithTimeout(bgCtx, probeTimeout)
			err := rt.embedder.Probe(probeCtx)
			cancel()
			if err != nil && bgCtx.Err() == nil {
				c.logger.Warn("embedding provider check failed; using full-text search until it recovers", "err", err)
			}
			backfillLoop(bgCtx, rt.store, rt.embedder, c.logger, backfillInterval)
		})
	} else {
		c.logger.Info("embeddings disabled (KENFOLD_EMBED_URL unset); using full-text search only")
	}
	if rt.chat != nil {
		c.logger.Info("chat model configured", "endpoint", rt.chat.Endpoint(), "model", rt.chat.Model(),
			"extract", c.cfg.Extract, "extract_policy", c.cfg.ExtractPolicy, "classify", c.cfg.Classify)
	}
	if w := rt.extractor(c.cfg, c.logger); w != nil {
		wg.Go(func() { w.Loop(bgCtx, extractInterval) })
	}

	srv := &http.Server{
		Addr:              c.cfg.HTTPAddr,
		Handler:           httpHandler(c.cfg, rt, c.logger),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: MCP responses may be long-lived SSE streams.
	}
	errc := make(chan error, 1)
	go func() {
		c.logger.Info("kenfold listening", "addr", c.cfg.HTTPAddr, "mcp", httpserver.MCPPath, "auth", c.cfg.Auth, "version", buildinfo.Version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	c.logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// logSecurityPosture warns about configurations that expose memory.
func (c *cli) logSecurityPosture(ctx context.Context, rt *runtime) {
	if c.cfg.Auth == config.AuthNone {
		host, _, _ := net.SplitHostPort(c.cfg.HTTPAddr)
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			c.logger.Warn("authentication is disabled (KENFOLD_AUTH=none): any local process can read and write memory")
		} else {
			c.logger.Warn("authentication is disabled (KENFOLD_AUTH=none) and the server listens on a non-loopback address; make sure the port is not reachable from other machines", "addr", c.cfg.HTTPAddr)
		}
		return
	}
	keys, err := rt.keys.List(ctx, false)
	if err != nil {
		c.logger.Warn("could not list API keys", "err", err)
		return
	}
	if len(keys) == 0 {
		c.logger.Warn("no API keys exist, so every MCP request will be rejected; create one per agent with: kenfold key create claude-code")
	}
}

// backfillLoop embeds memories that lack an embedding for the current model,
// immediately and then every interval, until ctx is done.
func backfillLoop(ctx context.Context, st *store.Store, e store.Embedder, logger *slog.Logger, every time.Duration) {
	runOnce := func() {
		stats, err := st.Backfill(ctx, e, backfillBatch)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("embedding backfill failed; will retry", "err", err, "embedded", stats.Embedded)
			}
			return
		}
		if stats.Embedded > 0 || stats.Skipped > 0 {
			logger.Info("embedding backfill", "embedded", stats.Embedded, "skipped", stats.Skipped)
		}
	}
	runOnce()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runOnce()
		}
	}
}

// serveStdio runs MCP over stdin/stdout for clients that spawn Kenfold as a
// subprocess. It never migrates: several clients may start it concurrently.
func (c *cli) serveStdio(ctx context.Context) error {
	c.logger.Debug("kenfold mcp (stdio) starting", "version", buildinfo.Version)
	if err := c.checkSchema(ctx); err != nil {
		if errors.Is(err, migrations.ErrPending) {
			return err
		}
		c.logger.Warn("database not reachable at startup; tools will fail until it is", "err", err)
	}

	rt, err := newRuntime(ctx, c.cfg)
	if err != nil {
		return err
	}
	defer rt.Close()

	s := mcpserver.New(buildinfo.Version, mcpserver.Deps{
		Store:       rt.store,
		Embedder:    rt.embedderIface(),
		Logger:      c.logger,
		Agent:       c.cfg.Agent,
		Classifier:  rt.classifier(c.cfg),
		MaxDistance: c.cfg.SearchMaxDistance,
	})
	err = s.Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (c *cli) checkSchema(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, schemaTimeout)
	defer cancel()
	return migrations.CheckCurrent(ctx, c.cfg.DatabaseURL)
}

func (c *cli) migrate(ctx context.Context, args []string) error {
	sub := "up"
	switch len(args) {
	case 0:
	case 1:
		sub = args[0]
	default:
		return usageErr("migrate takes at most one argument")
	}
	switch sub {
	case "up", "down", "status":
	default:
		return usageErr("unknown migrate command %q (want up|down|status)", sub)
	}

	p, err := migrations.Provider(c.cfg.DatabaseURL)
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
			c.logger.Info("applied", "migration", r.Source.Path, "duration", r.Duration)
		}
		v, err := p.GetDBVersion(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(c.out, "schema version %d\n", v)
	case "down":
		r, err := p.Down(ctx)
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		fmt.Fprintf(c.out, "rolled back %s\n", r.Source.Path)
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
			fmt.Fprintf(c.out, "%-8s %-40s %s\n", s.State, s.Source.Path, applied)
		}
	}
	return nil
}
