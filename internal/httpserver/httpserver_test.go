package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/mcpserver"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func newTestServer(t *testing.T, db Pinger) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	ts := httptest.NewServer(New(mcpserver.New("test"), db, logger, []string{"localhost", "127.0.0.1", "::1"}))
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestHealthAndReadiness(t *testing.T) {
	up := newTestServer(t, fakeDB{})
	if code, _ := get(t, up.URL+"/healthz"); code != http.StatusOK {
		t.Errorf("healthz = %d", code)
	}
	if code, _ := get(t, up.URL+"/readyz"); code != http.StatusOK {
		t.Errorf("readyz (db up) = %d", code)
	}

	down := newTestServer(t, fakeDB{err: errors.New("connection refused")})
	if code, _ := get(t, down.URL+"/healthz"); code != http.StatusOK {
		t.Errorf("healthz must not depend on db, got %d", code)
	}
	code, body := get(t, down.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("readyz (db down) = %d", code)
	}
	if body != `{"status":"database unavailable"}`+"\n" {
		t.Errorf("readyz must not leak error details, body = %q", body)
	}
}

func TestMCPOverHTTP(t *testing.T) {
	ts := newTestServer(t, fakeDB{})
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + MCPPath,
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	if got := cs.InitializeResult().ServerInfo.Name; got != "kenfold" {
		t.Errorf("server name = %q", got)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(res.Tools) != 6 {
		t.Errorf("got %d tools, want 6", len(res.Tools))
	}
}

func TestMCPRejectsCrossOrigin(t *testing.T) {
	ts := newTestServer(t, fakeDB{})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+MCPPath, nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", resp.StatusCode)
	}
}

func TestMCPRejectsForeignHost(t *testing.T) {
	ts := newTestServer(t, fakeDB{})
	for host, want := range map[string]int{
		"evil.example":        http.StatusForbidden, // DNS rebinding
		"evil.example:7077":   http.StatusForbidden,
		"localhost.evil.test": http.StatusForbidden,
		"localhost:7077":      http.StatusBadRequest, // allowed; reaches MCP handler and fails on empty body
		"[::1]:7077":          http.StatusBadRequest,
	} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+MCPPath, nil)
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if (want == http.StatusForbidden) != (resp.StatusCode == http.StatusForbidden) {
			t.Errorf("Host %q: status %d, want forbidden=%v", host, resp.StatusCode, want == http.StatusForbidden)
		}
	}
}

func TestHealthIgnoresHostAllowlist(t *testing.T) {
	// Probes from orchestrators use arbitrary Host headers; /healthz must still answer.
	ts := newTestServer(t, fakeDB{})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
	req.Host = "kenfold:7077"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz with foreign Host = %d", resp.StatusCode)
	}
}
