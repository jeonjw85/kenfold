package config

import (
	"log/slog"
	"slices"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != DefaultHTTPAddr || c.DatabaseURL != DefaultDatabaseURL || c.AutoMigrate || c.LogLevel != slog.LevelInfo {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if !slices.Equal(c.AllowedHosts, DefaultAllowedHosts) {
		t.Errorf("AllowedHosts = %v, want %v", c.AllowedHosts, DefaultAllowedHosts)
	}
	if c.Auth != AuthAPIKey {
		t.Errorf("Auth = %q, want %q (secure by default)", c.Auth, AuthAPIKey)
	}
	if c.Agent != "" || c.Embed.Enabled() || c.Embed.Model != DefaultEmbedModel || c.Embed.SendDimensions {
		t.Errorf("unexpected embedding/agent defaults: %+v", c)
	}
	if c.SearchMaxDistance != DefaultSearchMaxDistance {
		t.Errorf("SearchMaxDistance = %v", c.SearchMaxDistance)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{
		"KENFOLD_HTTP_ADDR":           "0.0.0.0:9000",
		"KENFOLD_DATABASE_URL":        "postgres://x@db/y",
		"KENFOLD_AUTO_MIGRATE":        "true",
		"KENFOLD_ALLOWED_HOSTS":       " mem.example.com , localhost,",
		"KENFOLD_LOG_LEVEL":           "debug",
		"KENFOLD_AUTH":                "NONE",
		"KENFOLD_AGENT":               "claude-code",
		"KENFOLD_EMBED_URL":           "http://ollama:11434/v1",
		"KENFOLD_EMBED_MODEL":         "text-embedding-3-large",
		"KENFOLD_EMBED_API_KEY":       "sk-secret",
		"KENFOLD_EMBED_DIMENSIONS":    "true",
		"KENFOLD_SEARCH_MAX_DISTANCE": "0.4",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != "0.0.0.0:9000" || c.DatabaseURL != "postgres://x@db/y" || !c.AutoMigrate || c.LogLevel != slog.LevelDebug {
		t.Errorf("overrides not applied: %+v", c)
	}
	if want := []string{"mem.example.com", "localhost"}; !slices.Equal(c.AllowedHosts, want) {
		t.Errorf("AllowedHosts = %v, want %v", c.AllowedHosts, want)
	}
	if c.Auth != AuthNone || c.Agent != "claude-code" {
		t.Errorf("auth/agent = %q/%q", c.Auth, c.Agent)
	}
	want := Embed{URL: "http://ollama:11434/v1", Model: "text-embedding-3-large", APIKey: "sk-secret", SendDimensions: true}
	if c.Embed != want || !c.Embed.Enabled() || c.SearchMaxDistance != 0.4 {
		t.Errorf("embed = %+v, max distance %v", c.Embed, c.SearchMaxDistance)
	}
}

func TestLoadInvalid(t *testing.T) {
	for _, m := range []map[string]string{
		{"KENFOLD_AUTO_MIGRATE": "maybe"},
		{"KENFOLD_LOG_LEVEL": "loud"},
		{"KENFOLD_ALLOWED_HOSTS": " , "},
		{"KENFOLD_AUTH": "password"},
		{"KENFOLD_AGENT": "Claude Code"},
		{"KENFOLD_EMBED_URL": "ollama:11434"},
		{"KENFOLD_EMBED_URL": "ftp://x/v1"},
		{"KENFOLD_EMBED_DIMENSIONS": "sometimes"},
		{"KENFOLD_SEARCH_MAX_DISTANCE": "0"},
		{"KENFOLD_SEARCH_MAX_DISTANCE": "3"},
		{"KENFOLD_SEARCH_MAX_DISTANCE": "NaN"},
		{"KENFOLD_SEARCH_MAX_DISTANCE": "far"},
	} {
		if _, err := LoadFrom(env(m)); err == nil {
			t.Errorf("LoadFrom(%v) succeeded; want error", m)
		}
	}
}
