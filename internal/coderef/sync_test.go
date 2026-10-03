package coderef

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCallRejectsCrossOriginAuthenticatedRedirect(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	// A caller-supplied redirect policy must not bypass the origin check.
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	err := call(context.Background(), hc, http.MethodGet, source.URL, "kf_redirect-secret", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("cross-origin redirect was not refused: %v", err)
	}
	if received.Load() != 0 {
		t.Errorf("redirect target received %d requests", received.Load())
	}
}

func TestCallAllowsSameOriginAuthenticatedRedirect(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/finish", http.StatusTemporaryRedirect)
			return
		}
		received.Add(1)
		if r.Header.Get("Authorization") != "Bearer kf_redirect-secret" {
			t.Errorf("same-origin redirect lost Authorization: %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := call(context.Background(), srv.Client(), http.MethodGet, srv.URL+"/start", "kf_redirect-secret", nil, nil); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 {
		t.Errorf("received %d final requests, want 1", received.Load())
	}
}
