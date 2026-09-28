package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/mcpserver"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

var loopback = []string{"localhost", "127.0.0.1", "::1"}

func newTestServer(t *testing.T, db Pinger, opts Options) *httptest.Server {
	t.Helper()
	return newTestServerWith(t, mcpserver.New("test", mcpserver.Deps{}), db, opts)
}

func newTestServerWith(t *testing.T, s *mcp.Server, db Pinger, opts Options) *httptest.Server {
	t.Helper()
	if opts.AllowedHosts == nil {
		opts.AllowedHosts = loopback
	}
	ts := httptest.NewServer(New(s, db, slog.New(slog.DiscardHandler), opts))
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

// bearer adds an Authorization header to every request.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: url + MCPPath, DisableStandaloneSSE: true, MaxRetries: -1}
	if token != "" {
		tr.HTTPClient = &http.Client{Transport: bearer{token}}
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(context.Background(), tr, nil)
	if err == nil {
		t.Cleanup(func() { cs.Close() })
	}
	return cs, err
}

// fakeVerifier accepts "good-token" as agent "codex" and fails "db-down" with a
// non-auth error.
func fakeVerifier(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	switch token {
	case "good-token":
		return &auth.TokenInfo{UserID: "key-1", Extra: map[string]any{"kenfold.agent": "codex"}}, nil
	case "db-down":
		return nil, errors.New("verification temporarily unavailable")
	}
	return nil, auth.ErrInvalidToken
}

func postMCP(t *testing.T, url, host, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+MCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if host != "" {
		req.Host = host
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestHealthAndReadiness(t *testing.T) {
	up := newTestServer(t, fakeDB{}, Options{Verifier: fakeVerifier})
	if code, _ := get(t, up.URL+"/healthz"); code != http.StatusOK {
		t.Errorf("healthz = %d", code)
	}
	if code, _ := get(t, up.URL+"/readyz"); code != http.StatusOK {
		t.Errorf("readyz (db up) = %d; probes must not require auth", code)
	}

	down := newTestServer(t, fakeDB{err: errors.New("connection refused")}, Options{})
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
	ts := newTestServer(t, fakeDB{}, Options{})
	cs, err := connect(t, ts.URL, "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := cs.InitializeResult().ServerInfo.Name; got != "kenfold" {
		t.Errorf("server name = %q", got)
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(res.Tools) != 6 {
		t.Errorf("got %d tools, want 6", len(res.Tools))
	}
}

func TestMCPRequiresBearerToken(t *testing.T) {
	ts := newTestServer(t, fakeDB{}, Options{Verifier: fakeVerifier})
	for name, c := range map[string]struct {
		host, token string
		want        int
	}{
		"no token":         {"", "", http.StatusUnauthorized},
		"wrong token":      {"", "nope", http.StatusUnauthorized},
		"verifier failure": {"", "db-down", http.StatusInternalServerError},
		"good token":       {"", "good-token", http.StatusOK},
		// Host allowlist runs before auth: rebinding attempts never reach the verifier.
		"foreign host": {"evil.example", "good-token", http.StatusForbidden},
	} {
		if got := postMCP(t, ts.URL, c.host, c.token); got != c.want {
			t.Errorf("%s: status %d, want %d", name, got, c.want)
		}
	}

	if _, err := connect(t, ts.URL, ""); err == nil {
		t.Error("client without token connected")
	}
	cs, err := connect(t, ts.URL, "good-token")
	if err != nil {
		t.Fatalf("connect with token: %v", err)
	}
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Errorf("tools/list with token: %v", err)
	}
}

// TestTokenInfoReachesTools checks the plumbing the whole agent-identity model
// depends on: the verified token is visible to tool handlers over stateless HTTP.
func TestTokenInfoReachesTools(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "probe", Version: "0"}, nil)
	type out struct {
		Agent string `json:"agent"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "whoami"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, out, error) {
		if req.Extra == nil || req.Extra.TokenInfo == nil {
			return nil, out{}, errors.New("no token info")
		}
		a, _ := req.Extra.TokenInfo.Extra["kenfold.agent"].(string)
		return nil, out{Agent: a}, nil
	})
	ts := newTestServerWith(t, s, fakeDB{}, Options{Verifier: fakeVerifier})
	cs, err := connect(t, ts.URL, "good-token")
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("whoami: %v %+v", err, res)
	}
	if got := res.StructuredContent.(map[string]any)["agent"]; got != "codex" {
		t.Errorf("agent seen by tool = %v, want codex", got)
	}
}

func TestMCPRejectsCrossOrigin(t *testing.T) {
	ts := newTestServer(t, fakeDB{}, Options{})
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
	ts := newTestServer(t, fakeDB{}, Options{})
	for host, forbidden := range map[string]bool{
		"evil.example":        true, // DNS rebinding
		"evil.example:7077":   true,
		"localhost.evil.test": true,
		"localhost:7077":      false,
		"[::1]:7077":          false,
	} {
		if got := postMCP(t, ts.URL, host, ""); (got == http.StatusForbidden) != forbidden {
			t.Errorf("Host %q: status %d, want forbidden=%v", host, got, forbidden)
		}
	}
}

func TestHealthIgnoresHostAllowlist(t *testing.T) {
	// Probes from orchestrators use arbitrary Host headers; /healthz must still answer.
	ts := newTestServer(t, fakeDB{}, Options{})
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

// A native `kenfold serve` on 127.0.0.1 behind a tunnel on the same machine
// (cloudflared, tailscale funnel) receives the public Host over loopback.
// Kenfold's own allowlist decides; the SDK's loopback-only check must not
// reject it.
func TestAllowedPublicHostOverLoopback(t *testing.T) {
	ts := newTestServer(t, fakeDB{}, Options{AllowedHosts: append([]string{"kenfold.example.com"}, loopback...)})
	if got := postMCP(t, ts.URL, "kenfold.example.com", ""); got == http.StatusForbidden {
		t.Errorf("allowed public Host over loopback: status %d", got)
	}
	if got := postMCP(t, ts.URL, "evil.example", ""); got != http.StatusForbidden {
		t.Errorf("foreign Host over loopback: status %d", got)
	}
}
