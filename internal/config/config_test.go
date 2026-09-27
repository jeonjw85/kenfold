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
}

func TestLoadOverrides(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{
		"KENFOLD_HTTP_ADDR":     "0.0.0.0:9000",
		"KENFOLD_DATABASE_URL":  "postgres://x@db/y",
		"KENFOLD_AUTO_MIGRATE":  "true",
		"KENFOLD_ALLOWED_HOSTS": " mem.example.com , localhost,",
		"KENFOLD_LOG_LEVEL":     "debug",
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
}

func TestLoadInvalid(t *testing.T) {
	for _, m := range []map[string]string{
		{"KENFOLD_AUTO_MIGRATE": "maybe"},
		{"KENFOLD_LOG_LEVEL": "loud"},
		{"KENFOLD_ALLOWED_HOSTS": " , "},
	} {
		if _, err := LoadFrom(env(m)); err == nil {
			t.Errorf("LoadFrom(%v) succeeded; want error", m)
		}
	}
}
