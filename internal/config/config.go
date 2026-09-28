// Package config loads Kenfold configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/kenfold/kenfold/internal/memory"
)

// Defaults target a local setup where Postgres runs from compose.yaml.
const (
	DefaultHTTPAddr    = "127.0.0.1:7077"
	DefaultDatabaseURL = "postgres://kenfold:kenfold@127.0.0.1:54329/kenfold?sslmode=disable"
	DefaultEmbedModel  = "bge-m3"
	// DefaultSearchMaxDistance is the cosine distance cutoff for vector matches.
	DefaultSearchMaxDistance = 0.55
)

// Auth modes for the HTTP server.
const (
	// AuthAPIKey requires "Authorization: Bearer kf_..." on /mcp (default).
	AuthAPIKey = "apikey"
	// AuthNone disables authentication. Only for loopback-only development.
	AuthNone = "none"
)

type Config struct {
	// HTTPAddr is the listen address for `kenfold serve`.
	HTTPAddr string
	// DatabaseURL is a PostgreSQL connection string (pgvector required).
	DatabaseURL string
	// AutoMigrate applies pending migrations on `kenfold serve` startup.
	AutoMigrate bool
	// AllowedHosts is the Host header allowlist for /mcp (DNS-rebinding defense).
	// Ports are ignored. Set to your public hostname when deploying remotely.
	AllowedHosts []string
	LogLevel     slog.Level

	// Auth is AuthAPIKey or AuthNone.
	Auth string
	// Agent is the agent name recorded for `kenfold mcp` (stdio) sessions. When
	// empty, the MCP client's self-reported name is used.
	Agent string

	// Embed configures the embedding provider; disabled when Embed.URL is empty.
	Embed Embed
	// SearchMaxDistance is the cosine distance cutoff for vector matches, in (0, 2].
	SearchMaxDistance float64

	// Chat configures the chat model used for extraction and classification;
	// disabled when Chat.URL is empty.
	Chat Chat
	// Extract runs the background extractor (default: on when Chat is configured).
	Extract bool
	// ExtractPolicy is "propose" (review everything, default) or "auto".
	ExtractPolicy string
	// Classify uses the chat model to type memories stored without a type
	// (default: on when Chat is configured).
	Classify bool

	// Rerank configures a cross-encoder reranker for search; disabled when
	// Rerank.URL is empty.
	Rerank Rerank

	// PublicURL is the base URL remote clients reach Kenfold at (e.g.
	// https://kenfold.example.com behind a TLS proxy or tunnel). Its host is
	// added to AllowedHosts. Required for OAuth.
	PublicURL string
	// OAuth enables the built-in authorization server for remote clients
	// (default: on when PublicURL is set).
	OAuth bool
	// OAuthDCR enables dynamic client registration (default: on with OAuth).
	OAuthDCR bool
}

// Rerank configures a /rerank endpoint (llama-server, Jina, Cohere, Voyage).
type Rerank struct {
	URL    string // e.g. http://127.0.0.1:8080/v1 for llama-server
	Model  string
	APIKey string // never logged
}

// Enabled reports whether a reranker is configured.
func (r Rerank) Enabled() bool { return r.URL != "" }

// DefaultRerankModel is the reranker Kenfold's compose file runs.
const DefaultRerankModel = "bge-reranker-v2-m3"

// Chat configures an OpenAI-compatible chat completions endpoint.
type Chat struct {
	URL       string // e.g. http://127.0.0.1:11434/v1 for Ollama
	Model     string
	APIKey    string // never logged
	Reasoning string // reasoning_effort: "none" (default), a level, or "omit"
}

// Enabled reports whether a chat model is configured.
func (c Chat) Enabled() bool { return c.URL != "" }

// Chat defaults.
const (
	DefaultChatModel = "qwen3.5:4b"
	ExtractPropose   = "propose"
	ExtractAuto      = "auto"
)

// Embed configures an OpenAI-compatible embeddings endpoint.
type Embed struct {
	URL            string // e.g. http://127.0.0.1:11434/v1 for Ollama
	Model          string
	Name           string // recorded model name when Model is a local alias (default: Model)
	APIKey         string // never logged
	SendDimensions bool   // send the "dimensions" parameter (for models not natively 1024-d)
}

// Enabled reports whether embeddings are configured.
func (e Embed) Enabled() bool { return e.URL != "" }

// DefaultAllowedHosts accepts loopback names only.
var DefaultAllowedHosts = []string{"localhost", "127.0.0.1", "::1"}

// Load reads configuration from the process environment.
func Load() (Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads configuration using getenv, which makes it testable.
func LoadFrom(getenv func(string) string) (Config, error) {
	c := Config{
		HTTPAddr:          envOr(getenv, "KENFOLD_HTTP_ADDR", DefaultHTTPAddr),
		DatabaseURL:       envOr(getenv, "KENFOLD_DATABASE_URL", DefaultDatabaseURL),
		AllowedHosts:      slices.Clone(DefaultAllowedHosts),
		Auth:              strings.ToLower(envOr(getenv, "KENFOLD_AUTH", AuthAPIKey)),
		Agent:             strings.TrimSpace(getenv("KENFOLD_AGENT")),
		SearchMaxDistance: DefaultSearchMaxDistance,
		Embed: Embed{
			URL:    strings.TrimSpace(getenv("KENFOLD_EMBED_URL")),
			Model:  envOr(getenv, "KENFOLD_EMBED_MODEL", DefaultEmbedModel),
			Name:   strings.TrimSpace(getenv("KENFOLD_EMBED_NAME")),
			APIKey: getenv("KENFOLD_EMBED_API_KEY"),
		},
		Chat: Chat{
			URL:       strings.TrimSpace(getenv("KENFOLD_CHAT_URL")),
			Model:     envOr(getenv, "KENFOLD_CHAT_MODEL", DefaultChatModel),
			APIKey:    getenv("KENFOLD_CHAT_API_KEY"),
			Reasoning: strings.ToLower(envOr(getenv, "KENFOLD_CHAT_REASONING", "none")),
		},
		ExtractPolicy: strings.ToLower(envOr(getenv, "KENFOLD_EXTRACT_POLICY", ExtractPropose)),
		Rerank: Rerank{
			URL:    strings.TrimSpace(getenv("KENFOLD_RERANK_URL")),
			Model:  envOr(getenv, "KENFOLD_RERANK_MODEL", DefaultRerankModel),
			APIKey: getenv("KENFOLD_RERANK_API_KEY"),
		},
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

	var err error
	if c.AutoMigrate, err = boolEnv(getenv, "KENFOLD_AUTO_MIGRATE"); err != nil {
		return Config{}, err
	}
	if c.Embed.SendDimensions, err = boolEnv(getenv, "KENFOLD_EMBED_DIMENSIONS"); err != nil {
		return Config{}, err
	}

	if v := getenv("KENFOLD_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Config{}, fmt.Errorf("KENFOLD_LOG_LEVEL: %w", err)
		}
	}

	switch c.Auth {
	case AuthAPIKey, AuthNone:
	default:
		return Config{}, fmt.Errorf("KENFOLD_AUTH: %q is not one of %s, %s", c.Auth, AuthAPIKey, AuthNone)
	}

	if c.Agent != "" && !memory.ValidAgent(c.Agent) {
		return Config{}, fmt.Errorf("KENFOLD_AGENT: %q is not a valid agent name (lowercase letters, digits, '.', '_', '-'; e.g. claude-code)", c.Agent)
	}

	if c.Embed.URL != "" {
		u, err := url.Parse(c.Embed.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("KENFOLD_EMBED_URL: %q is not an http(s) URL", c.Embed.URL)
		}
	}

	if v := getenv("KENFOLD_SEARCH_MAX_DISTANCE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f > 0 && f <= 2) {
			return Config{}, fmt.Errorf("KENFOLD_SEARCH_MAX_DISTANCE: %q must be a number in (0, 2]", v)
		}
		c.SearchMaxDistance = f
	}

	if v := strings.TrimSpace(getenv("KENFOLD_PUBLIC_URL")); v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return Config{}, fmt.Errorf("KENFOLD_PUBLIC_URL: %q must be a base URL such as https://kenfold.example.com (no path)", v)
		}
		if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
			return Config{}, fmt.Errorf("KENFOLD_PUBLIC_URL: %q must use https; OAuth tokens and the owner password must not cross the network in the clear", v)
		}
		c.PublicURL = u.Scheme + "://" + strings.ToLower(u.Host)
		if h := strings.ToLower(u.Hostname()); !slices.Contains(c.AllowedHosts, h) {
			c.AllowedHosts = append(c.AllowedHosts, h)
		}
	}
	for _, s := range []struct {
		key string
		dst *bool
		def bool
	}{{"KENFOLD_OAUTH", &c.OAuth, c.PublicURL != ""}, {"KENFOLD_OAUTH_DCR", &c.OAuthDCR, true}} {
		*s.dst = s.def
		if v := getenv(s.key); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return Config{}, fmt.Errorf("%s: %q is not a boolean", s.key, v)
			}
			*s.dst = b
		}
	}
	if c.OAuth && c.PublicURL == "" {
		return Config{}, fmt.Errorf("KENFOLD_OAUTH=true needs KENFOLD_PUBLIC_URL (the https URL clients reach Kenfold at)")
	}
	if c.OAuth && c.Auth == AuthNone {
		return Config{}, fmt.Errorf("KENFOLD_OAUTH needs KENFOLD_AUTH=apikey")
	}
	c.OAuthDCR = c.OAuthDCR && c.OAuth

	if c.Rerank.URL != "" {
		u, err := url.Parse(c.Rerank.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("KENFOLD_RERANK_URL: %q is not an http(s) URL", c.Rerank.URL)
		}
	}

	if c.Chat.URL != "" {
		u, err := url.Parse(c.Chat.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("KENFOLD_CHAT_URL: %q is not an http(s) URL", c.Chat.URL)
		}
	}
	switch c.Chat.Reasoning {
	case "none", "omit", "minimal", "low", "medium", "high":
	default:
		return Config{}, fmt.Errorf("KENFOLD_CHAT_REASONING: %q is not one of none, low, medium, high, omit", c.Chat.Reasoning)
	}
	switch c.ExtractPolicy {
	case ExtractPropose, ExtractAuto:
	default:
		return Config{}, fmt.Errorf("KENFOLD_EXTRACT_POLICY: %q is not one of %s, %s", c.ExtractPolicy, ExtractPropose, ExtractAuto)
	}
	// Extraction and classification default to on when a chat model is configured.
	for _, s := range []struct {
		key string
		dst *bool
	}{{"KENFOLD_EXTRACT", &c.Extract}, {"KENFOLD_CLASSIFY", &c.Classify}} {
		*s.dst = c.Chat.Enabled()
		if strings.TrimSpace(getenv(s.key)) != "" {
			b, err := boolEnv(getenv, s.key)
			if err != nil {
				return Config{}, err
			}
			if b && !c.Chat.Enabled() {
				return Config{}, fmt.Errorf("%s=true needs a chat model: set KENFOLD_CHAT_URL", s.key)
			}
			*s.dst = b
		}
	}
	return c, nil
}

func envOr(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}

func boolEnv(getenv func(string) string, key string) (bool, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func loopbackHost(h string) bool {
	h = strings.Trim(strings.ToLower(h), "[]")
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
