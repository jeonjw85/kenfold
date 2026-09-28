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
		"KENFOLD_EMBED_NAME":          "te3l",
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
	want := Embed{URL: "http://ollama:11434/v1", Model: "text-embedding-3-large", Name: "te3l", APIKey: "sk-secret", SendDimensions: true}
	if c.Embed != want || !c.Embed.Enabled() || c.SearchMaxDistance != 0.4 {
		t.Errorf("embed = %+v, max distance %v", c.Embed, c.SearchMaxDistance)
	}
}

func TestChatConfig(t *testing.T) {
	c, err := LoadFrom(env(nil))
	if err != nil || c.Chat.Enabled() || c.Extract || c.Classify || c.Consolidate || c.Chat.Model != DefaultChatModel || c.Chat.Reasoning != "none" || c.ExtractPolicy != ExtractPropose {
		t.Errorf("defaults: %+v %v", c.Chat, err)
	}
	// A chat URL turns extraction and classification on by default.
	c, err = LoadFrom(env(map[string]string{"KENFOLD_CHAT_URL": "http://ollama:11434/v1"}))
	if err != nil || !c.Extract || !c.Classify || !c.Consolidate {
		t.Errorf("enabled defaults: extract %v classify %v consolidate %v %v", c.Extract, c.Classify, c.Consolidate, err)
	}
	c, err = LoadFrom(env(map[string]string{
		"KENFOLD_CHAT_URL": "http://ollama:11434/v1", "KENFOLD_CHAT_MODEL": "kenfold-extract", "KENFOLD_CHAT_API_KEY": "sk-x",
		"KENFOLD_CHAT_REASONING": "OMIT", "KENFOLD_EXTRACT": "false", "KENFOLD_CLASSIFY": "true", "KENFOLD_CONSOLIDATE": "0", "KENFOLD_EXTRACT_POLICY": "Auto",
	}))
	if err != nil || c.Extract || !c.Classify || c.Consolidate || c.Chat.Model != "kenfold-extract" || c.Chat.APIKey != "sk-x" || c.Chat.Reasoning != "omit" || c.ExtractPolicy != ExtractAuto {
		t.Errorf("overrides: %+v extract %v classify %v policy %s %v", c.Chat, c.Extract, c.Classify, c.ExtractPolicy, err)
	}
	// Explicitly disabling without a chat model is fine.
	if _, err := LoadFrom(env(map[string]string{"KENFOLD_EXTRACT": "false"})); err != nil {
		t.Errorf("EXTRACT=false without chat: %v", err)
	}
}

func TestRerankConfig(t *testing.T) {
	c, err := LoadFrom(env(nil))
	if err != nil || c.Rerank.Enabled() || c.Rerank.Model != DefaultRerankModel {
		t.Errorf("defaults: %+v %v", c.Rerank, err)
	}
	c, err = LoadFrom(env(map[string]string{"KENFOLD_RERANK_URL": " http://reranker:8080/v1 ", "KENFOLD_RERANK_MODEL": "jina-reranker-v2-base-multilingual", "KENFOLD_RERANK_API_KEY": "k"}))
	if err != nil || !c.Rerank.Enabled() || c.Rerank.URL != "http://reranker:8080/v1" || c.Rerank.Model != "jina-reranker-v2-base-multilingual" || c.Rerank.APIKey != "k" {
		t.Errorf("overrides: %+v %v", c.Rerank, err)
	}
}

func TestPublicURLAndOAuth(t *testing.T) {
	c, err := LoadFrom(env(nil))
	if err != nil || c.OAuth || c.OAuthDCR || c.PublicURL != "" {
		t.Errorf("defaults: oauth %v dcr %v url %q %v", c.OAuth, c.OAuthDCR, c.PublicURL, err)
	}
	c, err = LoadFrom(env(map[string]string{"KENFOLD_PUBLIC_URL": "https://Kenfold.Example.com/"}))
	if err != nil || !c.OAuth || !c.OAuthDCR || c.PublicURL != "https://kenfold.example.com" || !slices.Contains(c.AllowedHosts, "kenfold.example.com") {
		t.Errorf("public URL: %+v %v", c, err)
	}
	c, err = LoadFrom(env(map[string]string{"KENFOLD_PUBLIC_URL": "https://kenfold.example.com", "KENFOLD_OAUTH_DCR": "false", "KENFOLD_ALLOWED_HOSTS": "localhost"}))
	if err != nil || !c.OAuth || c.OAuthDCR || !slices.Equal(c.AllowedHosts, []string{"localhost", "kenfold.example.com"}) {
		t.Errorf("dcr off: %+v %v", c, err)
	}
	c, err = LoadFrom(env(map[string]string{"KENFOLD_PUBLIC_URL": "https://kenfold.example.com", "KENFOLD_OAUTH": "false"}))
	if err != nil || c.OAuth || c.OAuthDCR {
		t.Errorf("oauth off: %+v %v", c, err)
	}
	if c, err := LoadFrom(env(map[string]string{"KENFOLD_PUBLIC_URL": "http://127.0.0.1:7077"})); err != nil || !c.OAuth {
		t.Errorf("loopback http (local testing): %v", err)
	}
}

func TestDashboardConfig(t *testing.T) {
	if c, err := LoadFrom(env(nil)); err != nil || c.Dashboard != DashboardLocal {
		t.Errorf("default: %q %v", c.Dashboard, err)
	}
	if c, err := LoadFrom(env(map[string]string{"KENFOLD_DASHBOARD": "OFF"})); err != nil || c.Dashboard != DashboardOff {
		t.Errorf("off: %q %v", c.Dashboard, err)
	}
	if c, err := LoadFrom(env(map[string]string{"KENFOLD_DASHBOARD": "remote", "KENFOLD_PUBLIC_URL": "https://k.example.com"})); err != nil || c.Dashboard != DashboardRemote {
		t.Errorf("remote: %q %v", c.Dashboard, err)
	}
}

func TestLoadInvalid(t *testing.T) {
	for _, m := range []map[string]string{
		{"KENFOLD_DASHBOARD": "public"},
		{"KENFOLD_DASHBOARD": "remote"},
		{"KENFOLD_DASHBOARD": "remote", "KENFOLD_PUBLIC_URL": "http://127.0.0.1:7077"},
		{"KENFOLD_PUBLIC_URL": "http://kenfold.example.com"}, // no TLS on a network address
		{"KENFOLD_PUBLIC_URL": "https://kenfold.example.com/memory"},
		{"KENFOLD_PUBLIC_URL": "https://user:pw@kenfold.example.com"},
		{"KENFOLD_PUBLIC_URL": "kenfold.example.com"},
		{"KENFOLD_OAUTH": "true"},
		{"KENFOLD_PUBLIC_URL": "https://kenfold.example.com", "KENFOLD_AUTH": "none"},
		{"KENFOLD_PUBLIC_URL": "https://kenfold.example.com", "KENFOLD_OAUTH_DCR": "maybe"},
		{"KENFOLD_RERANK_URL": "reranker:8080"},
		{"KENFOLD_CHAT_URL": "ollama:11434"},
		{"KENFOLD_CHAT_URL": "http://x/v1", "KENFOLD_CHAT_REASONING": "maximum"},
		{"KENFOLD_EXTRACT_POLICY": "yolo"},
		{"KENFOLD_EXTRACT": "true"},     // no chat model
		{"KENFOLD_CLASSIFY": "true"},    // no chat model
		{"KENFOLD_CONSOLIDATE": "true"}, // no chat model
		{"KENFOLD_CHAT_URL": "http://x/v1", "KENFOLD_EXTRACT": "perhaps"},
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
