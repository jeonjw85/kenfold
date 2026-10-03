package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	pw := "correct horse battery"
	h, err := HashPassword(pw)
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash = %q, %v", h, err)
	}
	if ok, err := CheckPassword(h, pw); !ok || err != nil {
		t.Errorf("correct password rejected: %v", err)
	}
	if ok, _ := CheckPassword(h, pw+"x"); ok {
		t.Error("wrong password accepted")
	}
	if h2, _ := HashPassword(pw); h2 == h {
		t.Error("salt is not random")
	}
	if _, err := HashPassword("short"); err != ErrWeakPassword {
		t.Errorf("short password: %v", err)
	}
	twelve, eleven := "비밀번호는열두글자이상임", "비밀번호는열두글자이상"
	if _, err := HashPassword(twelve); err != nil {
		t.Errorf("12 Korean characters (runes, not bytes): %v", err)
	}
	if _, err := HashPassword(eleven); err != ErrWeakPassword {
		t.Errorf("11 Korean characters (33 bytes) accepted: %v", err)
	}
	for _, bad := range []string{"", "$bcrypt$x", "$argon2id$v=19$m=0,t=3,p=2$AAAA$AAAA", "$argon2id$v=18$m=65536,t=3,p=2$AAAA$AAAA"} {
		if _, err := CheckPassword(bad, pw); err == nil {
			t.Errorf("CheckPassword(%q) accepted a malformed hash", bad)
		}
	}
}

func TestRedirectURIs(t *testing.T) {
	for uri, ok := range map[string]bool{
		"https://chatgpt.com/connector_platform_oauth_redirect": true,
		"https://claude.ai/api/mcp/auth_callback":               true,
		"http://127.0.0.1:33418/callback":                       true,
		"http://localhost/cb":                                   true,
		"http://[::1]:8080/cb":                                  true,
		"http://example.com/cb":                                 false,
		"https://example.com/cb#frag":                           false,
		"https://user:pw@example.com/cb":                        false,
		"myapp://callback":                                      false,
		"javascript:alert(1)":                                   false,
		"/relative":                                             false,
		"":                                                      false,
	} {
		if err := validRedirectURI(uri); (err == nil) != ok {
			t.Errorf("validRedirectURI(%q) = %v", uri, err)
		}
	}
	reg := []string{"https://app.example/cb", "http://127.0.0.1/callback"}
	for req, ok := range map[string]bool{
		"https://app.example/cb":                   true,
		"https://app.example/cb/":                  false,
		"https://app.example/cb?x=1":               false,
		"http://127.0.0.1:54321/callback":          true, // loopback: any port
		"http://127.0.0.1:54321/other":             false,
		"http://localhost:54321/callback":          false, // host must match
		"https://127.0.0.1:54321/callback":         false,
		"http://user@127.0.0.1:54321/callback":     false,
		"http://127.0.0.1:54321/callback#fragment": false,
		"http://127.0.0.1:54321/%63allback":        false,
	} {
		if got := redirectMatches(reg, req); got != ok {
			t.Errorf("redirectMatches(%q) = %v", req, got)
		}
	}
}

func TestPKCE(t *testing.T) {
	verifier := strings.Repeat("a", 43)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if !pkceMatches(verifier, challenge) {
		t.Error("valid verifier rejected")
	}
	if pkceMatches(strings.Repeat("b", 43), challenge) {
		t.Error("wrong verifier accepted")
	}
	if pkceMatches("short", challenge) || pkceMatches(verifier+"!", challenge) || pkceMatches(verifier, "plain") {
		t.Error("malformed input accepted")
	}
}

func TestCanonicalResource(t *testing.T) {
	for in, want := range map[string]string{
		"https://Kenfold.Example.com/mcp":     "https://kenfold.example.com/mcp",
		"https://kenfold.example.com:443/mcp": "https://kenfold.example.com/mcp",
		"https://kenfold.example.com/mcp/":    "https://kenfold.example.com/mcp",
		"http://127.0.0.1:7077/mcp":           "http://127.0.0.1:7077/mcp",
		"https://x.example/mcp#f":             "",
		"kenfold.example.com/mcp":             "",
		"ftp://x/mcp":                         "",
	} {
		if got := canonicalResource(in); got != want {
			t.Errorf("canonicalResource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCIMDURL(t *testing.T) {
	for id, ok := range map[string]bool{
		"https://chatgpt.com/oauth/client.json": true,
		"https://example.com/client":            true,
		"https://example.com":                   false, // no path
		"https://example.com/":                  false,
		"http://example.com/client":             false,
		"https://example.com:8443/client":       false,
		"https://user@example.com/client":       false,
		"https://example.com/a/../client":       false,
		"https://example.com/client#x":          false,
		"https://127.0.0.1/client":              false,
		"https://[::1]/client":                  false,
		"https://10.0.0.8/client":               false,
		"https://169.254.169.254/latest":        false,
		"kfd_opaque":                            false,
	} {
		if got := IsCIMDClientID(id); got != ok {
			t.Errorf("IsCIMDClientID(%q) = %v", id, got)
		}
	}
	for addr, public := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"127.0.0.1": false, "169.254.169.254": false, "100.64.0.1": false, "::1": false, "fd00::1": false,
		"fe80::1": false, "::ffff:127.0.0.1": false, "0.0.0.0": false, "198.18.0.1": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != public {
			t.Errorf("publicAddr(%s) = %v", addr, got)
		}
	}
}

func TestCIMDFetch(t *testing.T) {
	var body string
	var status = http.StatusOK
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/client.json", http.StatusFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	f := NewCIMDFetcher(true)
	f.client.Transport = srv.Client().Transport // trust the test certificate
	f.client.CheckRedirect = NewCIMDFetcher(true).client.CheckRedirect
	ctx := context.Background()
	id := srv.URL + "/client.json"

	body = `{"client_id":"` + id + `","client_name":"Test","redirect_uris":["https://app.example/cb"],"grant_types":["authorization_code","refresh_token"],"token_endpoint_auth_method":"none"}`
	m, err := f.fetch(ctx, id, id)
	if err != nil || m.ClientName != "Test" || m.RedirectURIs[0] != "https://app.example/cb" {
		t.Fatalf("fetch = %+v, %v", m, err)
	}
	for name, b := range map[string]string{
		"client_id mismatch": `{"client_id":"https://other.example/c","redirect_uris":["https://app.example/cb"]}`,
		"no redirect":        `{"client_id":"` + id + `","redirect_uris":[]}`,
		"bad redirect":       `{"client_id":"` + id + `","redirect_uris":["http://evil.example/cb"]}`,
		"secret client":      `{"client_id":"` + id + `","redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`,
		"not json":           `nope`,
		"too large":          `{"client_id":"` + id + `","x":"` + strings.Repeat("a", cimdMaxBytes) + `"}`,
	} {
		body = b
		if _, err := f.fetch(ctx, id, id); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	status = http.StatusNotFound
	if _, err := f.fetch(ctx, id, id); err == nil {
		t.Error("404 accepted")
	}
	status = http.StatusOK
	if _, err := f.fetch(ctx, srv.URL+"/redirect", srv.URL+"/redirect"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("redirect followed: %v", err)
	}

	// The production fetcher refuses loopback at dial time, whatever the URL says.
	guarded := NewCIMDFetcher(false)
	if _, err := guarded.fetch(ctx, id, id); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Errorf("guarded fetch of a loopback server: %v", err)
	}
}

func TestSuggestAgent(t *testing.T) {
	for _, c := range []struct{ name, id, want string }{
		{"ChatGPT", "https://chatgpt.com/oauth/client.json", "chatgpt"},
		{"", "https://chatgpt.com/x", "chatgpt"},
		{"Claude", "kfd_x", "claude-ai"},
		{"MCP Inspector", "kfd_x", "mcp-inspector"},
		{"Kenfold Extractor", "kfd_x", "remote-client"},
		{"!!!", "kfd_x", "remote-client"},
		{"", "kfd_x", "remote-client"},
		{strings.Repeat("a", 80), "kfd_x", strings.Repeat("a", 40)},
	} {
		if got := suggestAgent(c.name, c.id); got != c.want {
			t.Errorf("suggestAgent(%q, %q) = %q, want %q", c.name, c.id, got, c.want)
		}
	}
}
