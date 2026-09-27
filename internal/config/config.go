// Package config loads Kenfold configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Defaults target a local setup where Postgres runs from compose.yaml.
const (
	DefaultHTTPAddr    = "127.0.0.1:7077"
	DefaultDatabaseURL = "postgres://kenfold:kenfold@127.0.0.1:54329/kenfold?sslmode=disable"
)

type Config struct {
	// HTTPAddr is the listen address for `kenfold serve`.
	// Phase 0 has no authentication, so keep it on loopback.
	HTTPAddr string
	// DatabaseURL is a PostgreSQL connection string (pgvector required).
	DatabaseURL string
	// AutoMigrate applies pending migrations on `kenfold serve` startup.
	AutoMigrate bool
	// AllowedHosts is the Host header allowlist for /mcp (DNS-rebinding defense).
	// Ports are ignored. Set to your public hostname when deploying remotely.
	AllowedHosts []string
	LogLevel     slog.Level
}

// DefaultAllowedHosts accepts loopback names only.
var DefaultAllowedHosts = []string{"localhost", "127.0.0.1", "::1"}

// Load reads configuration from the process environment.
func Load() (Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads configuration using getenv, which makes it testable.
func LoadFrom(getenv func(string) string) (Config, error) {
	c := Config{
		HTTPAddr:     envOr(getenv, "KENFOLD_HTTP_ADDR", DefaultHTTPAddr),
		DatabaseURL:  envOr(getenv, "KENFOLD_DATABASE_URL", DefaultDatabaseURL),
		AllowedHosts: slices.Clone(DefaultAllowedHosts),
	}

	if v := getenv("KENFOLD_ALLOWED_HOSTS"); v != "" {
		c.AllowedHosts = nil
		for h := range strings.SplitSeq(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				c.AllowedHosts = append(c.AllowedHosts, h)
			}
		}
		if len(c.AllowedHosts) == 0 {
			return Config{}, fmt.Errorf("KENFOLD_ALLOWED_HOSTS: no hosts in %q", v)
		}
	}

	if v := getenv("KENFOLD_AUTO_MIGRATE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("KENFOLD_AUTO_MIGRATE: %w", err)
		}
		c.AutoMigrate = b
	}

	if v := getenv("KENFOLD_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Config{}, fmt.Errorf("KENFOLD_LOG_LEVEL: %w", err)
		}
	}
	return c, nil
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}
