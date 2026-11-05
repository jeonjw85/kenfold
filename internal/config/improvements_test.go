package config

import (
	"strings"
	"testing"
)

// TestLoadTrimsValues checks that whitespace around env values (common in
// .env files) does not turn a valid value into a parse error.
func TestLoadTrimsValues(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{
		"KENFOLD_SEARCH_MAX_DISTANCE": " 0.4",
		"KENFOLD_LOG_LEVEL":           "debug ",
		"KENFOLD_OAUTH":               " true",
		"KENFOLD_OAUTH_DCR":           " false ",
		"KENFOLD_PUBLIC_URL":          " https://kenfold.example.com ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SearchMaxDistance != 0.4 || !c.OAuth || c.OAuthDCR || c.PublicURL != "https://kenfold.example.com" {
		t.Errorf("values not parsed after trimming: %+v", c)
	}
}

// TestReasoningErrorListsAllValues checks that the diagnostic names every
// accepted value, including "minimal".
func TestReasoningErrorListsAllValues(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{"KENFOLD_CHAT_URL": "http://x/v1", "KENFOLD_CHAT_REASONING": "maximum"}))
	if err == nil || !strings.Contains(err.Error(), "minimal") {
		t.Fatalf("err = %v, want every accepted value listed", err)
	}
}

// TestPublicURLErrorNamesTheProblem checks that each rejected URL shape gets a
// message naming what is wrong with it.
func TestPublicURLErrorNamesTheProblem(t *testing.T) {
	for value, want := range map[string]string{
		"https://kenfold.example.com/memory":   "path",
		"https://kenfold.example.com/?token=x": "query",
		"https://kenfold.example.com/#frag":    "fragment",
		"https://user:pw@kenfold.example.com":  "credentials",
		"kenfold.example.com":                  "http",
	} {
		_, err := LoadFrom(env(map[string]string{"KENFOLD_PUBLIC_URL": value}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("KENFOLD_PUBLIC_URL=%q: err = %v, want %q", value, err, want)
		}
	}
}

// TestAllowedHostsValidation checks that entries which can never match a Host
// header are rejected instead of silently locking the server out.
func TestAllowedHostsValidation(t *testing.T) {
	for _, value := range []string{
		"https://kenfold.example.com",
		"kenfold.example.com/path",
		"*.example.com",
		"kenfold .example.com",
	} {
		if _, err := LoadFrom(env(map[string]string{"KENFOLD_ALLOWED_HOSTS": value})); err == nil {
			t.Errorf("KENFOLD_ALLOWED_HOSTS=%q accepted; want an error", value)
		}
	}
	c, err := LoadFrom(env(map[string]string{"KENFOLD_ALLOWED_HOSTS": " kenfold.example.com , 10.0.0.5, ::1 "}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kenfold.example.com", "10.0.0.5", "::1"}
	if strings.Join(c.AllowedHosts, ",") != strings.Join(want, ",") {
		t.Errorf("AllowedHosts = %v, want %v", c.AllowedHosts, want)
	}
}
