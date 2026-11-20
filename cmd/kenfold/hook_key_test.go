package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestHookTrimsAPIKeyFromEnvironment checks that the session hook sends the
// key without surrounding whitespace. A value from a .env file or a systemd
// EnvironmentFile often ends with a newline; HTTP forbids control characters
// in a header value, so the request would fail and the hook would silently
// record nothing while retrying the spool forever.
func TestHookTrimsAPIKeyFromEnvironment(t *testing.T) {
	var mu sync.Mutex
	var auth string
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		requests++
		mu.Unlock()
		http.Error(w, "not now", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	const key = "kf_example_key"
	env := map[string]string{
		"KENFOLD_API_KEY":   "  " + key + "\n",
		"KENFOLD_STATE_DIR": t.TempDir(),
	}
	getenv := func(k string) string { return env[k] }
	var out, errOut strings.Builder
	if err := hookCmd(context.Background(), []string{"--url", srv.URL + "/mcp"}, getenv,
		strings.NewReader(`{"session_id":"s-1","hook_event_name":"SessionStart","cwd":"/tmp","source":"startup"}`),
		&out, &errOut); err != nil {
		t.Fatalf("hook failed (it must fail open): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if requests == 0 {
		t.Fatalf("the hook never contacted the server; stderr=%q", errOut.String())
	}
	if auth != "Bearer "+key {
		t.Errorf("Authorization = %q, want %q (the key was not trimmed)", auth, "Bearer "+key)
	}
}
