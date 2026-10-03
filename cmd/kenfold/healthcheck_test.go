package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/readyz" || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected health probe: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Location", "/readyz")
				w.WriteHeader(status)
			}))
			defer srv.Close()
			addr := strings.TrimPrefix(srv.URL, "http://")
			_, port, _ := net.SplitHostPort(addr)
			for _, listen := range []string{addr, ":" + port, "0.0.0.0:" + port} {
				out, _, err := runCLI(t, map[string]string{"KENFOLD_HTTP_ADDR": listen}, "healthcheck")
				if status == http.StatusOK {
					if err != nil || out != "ready\n" {
						t.Fatalf("healthy %s: %q %v", listen, out, err)
					}
				} else if err == nil || out != "" {
					t.Fatalf("unhealthy %s: %q %v", listen, out, err)
				}
			}
		})
	}
	if _, _, err := runCLI(t, map[string]string{"KENFOLD_HTTP_ADDR": "invalid"}, "healthcheck"); err == nil {
		t.Fatal("invalid listener accepted")
	}
}

func TestHealthcheckCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out strings.Builder
	err := run(ctx, []string{"healthcheck"}, func(key string) string {
		if key == "KENFOLD_HTTP_ADDR" {
			return strings.TrimPrefix(srv.URL, "http://")
		}
		return ""
	}, strings.NewReader(""), &out, &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled probe: %v", err)
	}
}
