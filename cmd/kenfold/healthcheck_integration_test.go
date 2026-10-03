package main

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/migrations"
)

func TestHealthcheckIntegration(t *testing.T) {
	databaseURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DatabaseURL: databaseURL, AllowedHosts: config.DefaultAllowedHosts, Auth: config.AuthAPIKey}
	rt, err := newRuntime(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	srv := httptest.NewServer(httpHandler(cfg, rt, slog.New(slog.DiscardHandler)))
	defer srv.Close()
	env := map[string]string{"KENFOLD_HTTP_ADDR": strings.TrimPrefix(srv.URL, "http://"), "KENFOLD_DATABASE_URL": databaseURL}
	if out, _, err := runCLI(t, env, "healthcheck"); err != nil || out != "ready\n" {
		t.Fatalf("current schema: %q %v", out, err)
	}
	p, err := migrations.Provider(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	defer func() {
		if _, err := migrations.Up(ctx, databaseURL); err != nil {
			t.Errorf("restore migrations: %v", err)
		}
	}()
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if out, _, err := runCLI(t, env, "healthcheck"); err == nil || out != "" {
		t.Fatalf("pending schema reported ready: %q %v", out, err)
	}
	if _, err := migrations.Up(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	if out, _, err := runCLI(t, env, "healthcheck"); err != nil || out != "ready\n" {
		t.Fatalf("recovered schema: %q %v", out, err)
	}
}
