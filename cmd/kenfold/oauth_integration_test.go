package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/mcpserver"
	"github.com/kenfold/kenfold/migrations"
)

// consentBot plays the owner in a browser: it opens the authorization URL,
// fills in the consent form, and returns the redirect it was sent to.
type consentBot struct {
	t        *testing.T
	password string
	access   string // "read" or "write"
	agent    string
	deny     bool
	// consentPage is the consent form shown; lastPage and lastCode are the
	// last response (for error assertions).
	consentPage string
	lastPage    string
	lastCode    int
}

var consentRE = regexp.MustCompile(`name="consent" value="([^"]+)"`)

func (b *consentBot) run(authURL string) (*url.URL, error) {
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(authURL)
	if err != nil {
		return nil, err
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	b.lastPage, b.lastCode = string(page), resp.StatusCode
	if resp.StatusCode == http.StatusFound {
		return url.Parse(resp.Header.Get("Location"))
	}
	m := consentRE.FindStringSubmatch(string(page))
	if m == nil {
		b.t.Fatalf("no consent form (status %d):\n%s", resp.StatusCode, page)
	}
	b.consentPage = string(page)
	decision := "approve"
	if b.deny {
		decision = "deny"
	}
	form := url.Values{"consent": {htmlUnescape(m[1])}, "access": {b.access}, "agent": {b.agent}, "password": {b.password}, "decision": {decision}}
	u, _ := url.Parse(authURL)
	req, _ := http.NewRequest(http.MethodPost, u.Scheme+"://"+u.Host+u.Path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin") // what a browser sends for its own form
	resp, err = noRedirect.Do(req)
	if err != nil {
		return nil, err
	}
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	b.lastPage, b.lastCode = string(page), resp.StatusCode
	if resp.StatusCode != http.StatusFound {
		return nil, &consentError{code: resp.StatusCode}
	}
	return url.Parse(resp.Header.Get("Location"))
}

type consentError struct{ code int }

func (e *consentError) Error() string { return "consent page returned " + http.StatusText(e.code) }

func htmlUnescape(s string) string {
	return strings.NewReplacer("&#43;", "+", "&#34;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'").Replace(s)
}

// fetcher adapts consentBot to the SDK's AuthorizationCodeFetcher.
func (b *consentBot) fetcher(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	loc, err := b.run(args.URL)
	if err != nil {
		return nil, err
	}
	q := loc.Query()
	if e := q.Get("error"); e != "" {
		return nil, &oauthRedirectError{e}
	}
	return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
}

type oauthRedirectError struct{ code string }

func (e *oauthRedirectError) Error() string { return "authorization failed: " + e.code }

// TestOAuthIntegration is the Phase 4 acceptance test for remote clients: an
// MCP client that only speaks OAuth discovers Kenfold's authorization server
// from the 401 challenge, identifies itself with a Client ID Metadata
// Document (or registers dynamically), gets the owner's approval on the
// consent page, and uses the memory with the access the owner chose. It
// TRUNCATES memory, api_key, and the oauth tables.
func TestOAuthIntegration(t *testing.T) {
	dbURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}

	// Kenfold must know its public URL before it starts, so reserve the port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	public := "http://" + ln.Addr().String()
	cfg, err := config.LoadFrom(func(k string) string {
		return map[string]string{"KENFOLD_DATABASE_URL": dbURL, "KENFOLD_PUBLIC_URL": public}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newRuntime(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := rt.pool.Exec(ctx, `TRUNCATE memory, api_key, oauth_client, oauth_owner CASCADE`); err != nil {
		t.Fatal(err)
	}
	const password = "correct horse battery staple"
	if err := rt.oauthDB.SetOwnerPassword(ctx, password); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	srv := httptest.NewUnstartedServer(httpHandler(cfg, rt, slog.New(slog.NewJSONHandler(&logs, nil))))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	// Clients' metadata documents, served over TLS as CIMD requires: a public
	// client, and one that authenticates with private_key_jwt like ChatGPT.
	var cimdURL, jwtClientURL string
	jwtKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cimd := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/client.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"client_id": cimdURL, "client_name": "Test Assistant", "redirect_uris": []string{"http://127.0.0.1:9/callback"},
				"grant_types": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_method": "none",
			})
		case "/oauth/jwt-client.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"client_id": jwtClientURL, "client_name": "JWT Assistant", "redirect_uris": []string{"https://assistant.example/oauth/cb"},
				"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
				"token_endpoint_auth_method": "private_key_jwt", "token_endpoint_auth_signing_alg": "RS256",
				"jwks_uri": "https://" + r.Host + "/oauth/jwks.json",
			})
		case "/oauth/jwks.json":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
				"n": b64url(jwtKey.N.Bytes()), "e": b64url(big.NewInt(int64(jwtKey.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cimd.Close()
	cimdURL = cimd.URL + "/oauth/client.json"
	jwtClientURL = cimd.URL + "/oauth/jwt-client.json"
	// Let the test fetcher reach the loopback TLS server (production refuses it).
	rt.oauth.SetFetcherForTest(cimd.Client().Transport)

	connect := func(t *testing.T, bot *consentBot, useCIMD bool) (*mcp.ClientSession, error) {
		t.Helper()
		hcfg := &auth.AuthorizationCodeHandlerConfig{
			RedirectURL:              "http://127.0.0.1:9/callback",
			AuthorizationCodeFetcher: bot.fetcher,
			RequestRefreshToken:      true,
			AcceptUnadvertisedIss:    true,
		}
		if useCIMD {
			hcfg.ClientIDMetadataDocumentConfig = &auth.ClientIDMetadataDocumentConfig{URL: cimdURL}
		} else {
			hcfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName: "DCR Client", RedirectURIs: []string{"http://127.0.0.1:9/callback"}, TokenEndpointAuthMethod: "none",
				GrantTypes: []string{"authorization_code", "refresh_token"},
			}}
		}
		h, err := auth.NewAuthorizationCodeHandler(hcfg)
		if err != nil {
			t.Fatal(err)
		}
		tr := &mcp.StreamableClientTransport{Endpoint: public + "/mcp", OAuthHandler: h, DisableStandaloneSSE: true, MaxRetries: -1}
		return mcp.NewClient(&mcp.Implementation{Name: "remote-test", Version: "1"}, nil).Connect(ctx, tr, nil)
	}

	t.Run("discovery", func(t *testing.T) {
		resp, err := http.Post(public+"/mcp", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+public+`/.well-known/oauth-protected-resource/mcp"`) {
			t.Fatalf("challenge = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
		var prm map[string]any
		getJSON(t, public+"/.well-known/oauth-protected-resource/mcp", &prm)
		if prm["resource"] != public+"/mcp" || prm["authorization_servers"].([]any)[0] != public {
			t.Errorf("PRM = %v", prm)
		}
		var asm map[string]any
		getJSON(t, public+"/.well-known/oauth-authorization-server", &asm)
		if asm["issuer"] != public || asm["client_id_metadata_document_supported"] != true || asm["registration_endpoint"] != public+"/oauth/register" ||
			asm["code_challenge_methods_supported"].([]any)[0] != "S256" || asm["authorization_response_iss_parameter_supported"] != true {
			t.Errorf("AS metadata = %v", asm)
		}
	})

	var writer *mcp.ClientSession
	var writerBot *consentBot
	t.Run("CIMD client with write access", func(t *testing.T) {
		bot := &consentBot{t: t, password: password, access: "write", agent: "chatgpt"}
		writerBot = bot
		cs, err := connect(t, bot, true)
		if err != nil {
			t.Fatalf("connect: %v (last page %d: %.300s)", err, bot.lastCode, bot.lastPage)
		}
		// The suggested agent name comes from the client's name; the owner changed it to chatgpt.
		// Read-only is preselected; write is offered because the client asked for it.
		for _, want := range []string{"Connect Test Assistant", cimdURL, "127.0.0.1:9", `value="test-assistant"`,
			`value="read" checked`, `id="rw" name="access" value="write">`} {
			if !strings.Contains(bot.consentPage, want) {
				t.Errorf("consent page lacks %q", want)
			}
		}
		writer = cs
		out := callTool[mcpserver.RememberOutput](t, cs, "remember", map[string]any{"content": "OAuth E2E: the staging cluster is in eu-west-1.", "project": "github.com/acme/infra"})
		if out.Status != "active" {
			t.Errorf("remember = %+v", out)
		}
		var agent string
		if err := rt.pool.QueryRow(ctx, `SELECT source_agent FROM memory WHERE id = $1`, out.ID).Scan(&agent); err != nil || agent != "chatgpt" {
			t.Errorf("source_agent = %q, %v (must come from the grant, not the client)", agent, err)
		}
	})

	t.Run("DCR client with read-only access", func(t *testing.T) {
		bot := &consentBot{t: t, password: password, access: "read", agent: "reader"}
		cs, err := connect(t, bot, false)
		if err != nil {
			t.Fatalf("connect: %v (last page %d: %.300s)", err, bot.lastCode, bot.lastPage)
		}
		defer cs.Close()
		got := callTool[mcpserver.RecallOutput](t, cs, "recall", map[string]any{"query": "staging cluster region", "project": "github.com/acme/infra"})
		if len(got.Memories) != 1 {
			t.Errorf("recall = %+v", got)
		}
		for tool, args := range map[string]map[string]any{
			"remember": {"content": "read-only clients cannot store this"},
			"forget":   {"id": "01a0e2f4-5446-7fe9-bb7a-89d49ad44bfa"},
			"handoff":  {"summary": "x"},
			"resume":   {},
		} {
			if msg := callToolErr(t, cs, tool, args); !strings.Contains(msg, "read-only") {
				t.Errorf("%s with a read-only grant: %q", tool, msg)
			}
		}
		// The REST API refuses writes too.
		var tok string
		if err := rt.pool.QueryRow(ctx, `SELECT count(*)::text FROM oauth_grant WHERE agent = 'reader' AND scopes = '{memory:read}'`).Scan(&tok); err != nil || tok != "1" {
			t.Errorf("read-only grant rows = %s, %v", tok, err)
		}
	})

	t.Run("wrong password, lockout, deny", func(t *testing.T) {
		bot := &consentBot{t: t, password: "wrong password!!", access: "write", agent: "evil"}
		for i := 0; i < 5; i++ {
			if _, err := connect(t, bot, true); err == nil {
				t.Fatal("connected with a wrong password")
			}
		}
		bot.password = password // right password, but locked out now
		if _, err := connect(t, bot, true); err == nil || bot.lastCode != http.StatusTooManyRequests {
			t.Errorf("after 5 failures: err %v, status %d", err, bot.lastCode)
		}
		if _, err := rt.pool.Exec(ctx, `UPDATE oauth_owner SET locked_until = NULL, failed_attempts = 0`); err != nil {
			t.Fatal(err)
		}
		deny := &consentBot{t: t, password: password, access: "write", agent: "x", deny: true}
		if _, err := connect(t, deny, true); err == nil || !strings.Contains(err.Error(), "access_denied") {
			t.Errorf("deny: %v", err)
		}
	})

	t.Run("token endpoint negatives", func(t *testing.T) {
		// A fresh code for the CIMD client, obtained by hand.
		verifier := strings.Repeat("v", 64)
		sum := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		authURL := public + "/oauth/authorize?" + url.Values{
			"response_type": {"code"}, "client_id": {cimdURL}, "redirect_uri": {"http://127.0.0.1:9/callback"},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"s1"}, "resource": {public + "/mcp"}, "scope": {"memory:write"},
		}.Encode()
		bot := &consentBot{t: t, password: password, access: "write", agent: "manual"}
		loc, err := bot.run(authURL)
		if err != nil {
			t.Fatal(err)
		}
		q := loc.Query()
		if q.Get("state") != "s1" || q.Get("iss") != public || q.Get("code") == "" {
			t.Fatalf("redirect = %v", loc)
		}
		code := q.Get("code")
		tokenReq := func(v url.Values) (int, map[string]any) {
			resp, err := http.PostForm(public+"/oauth/token", v)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var m map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&m)
			return resp.StatusCode, m
		}
		base := url.Values{"grant_type": {"authorization_code"}, "client_id": {cimdURL}, "redirect_uri": {"http://127.0.0.1:9/callback"}, "code": {code}}
		bad := func(k, v string) url.Values {
			c := url.Values{}
			for kk, vv := range base {
				c[kk] = vv
			}
			c.Set("code_verifier", verifier)
			c.Set(k, v)
			return c
		}
		if st, m := tokenReq(bad("code_verifier", strings.Repeat("w", 64))); st != 400 || m["error"] != "invalid_grant" {
			t.Errorf("wrong verifier: %d %v", st, m)
		}
		// A failed PKCE check still consumed the code: it cannot be retried (single use).
		st, m := tokenReq(bad("code_verifier", verifier))
		if st != 400 || m["error"] != "invalid_grant" {
			t.Errorf("code reuse after a failed attempt: %d %v", st, m)
		}

		// A clean run: exchange, then refresh rotation and reuse detection.
		loc, err = bot.run(authURL)
		if err != nil {
			t.Fatal(err)
		}
		base.Set("code", loc.Query().Get("code"))
		if st, m := tokenReq(bad("resource", "https://other.example/mcp")); st != 400 || m["error"] != "invalid_target" {
			t.Errorf("foreign resource: %d %v", st, m)
		}
		st, m = tokenReq(bad("resource", public+"/mcp"))
		if st != 200 || m["token_type"] != "Bearer" || m["scope"] != "memory:read memory:write" || !strings.HasPrefix(m["access_token"].(string), "kfa_") {
			t.Fatalf("exchange: %d %v", st, m)
		}
		access, refresh := m["access_token"].(string), m["refresh_token"].(string)
		st, m2 := tokenReq(url.Values{"grant_type": {"refresh_token"}, "client_id": {cimdURL}, "refresh_token": {refresh}})
		if st != 200 || m2["refresh_token"] == refresh {
			t.Fatalf("refresh: %d %v", st, m2)
		}
		if st, m := tokenReq(url.Values{"grant_type": {"refresh_token"}, "client_id": {"https://someone.else/c"}, "refresh_token": {m2["refresh_token"].(string)}}); st != 401 || m["error"] != "invalid_client" {
			t.Errorf("refresh by an unknown client: %d %v", st, m)
		}
		// Replaying the old refresh token revokes the grant: the new tokens die too.
		if st, m := tokenReq(url.Values{"grant_type": {"refresh_token"}, "client_id": {cimdURL}, "refresh_token": {refresh}}); st != 400 || !strings.Contains(m["error_description"].(string), "revoked") {
			t.Errorf("refresh reuse: %d %v", st, m)
		}
		for _, tok := range []string{access, m2["access_token"].(string)} {
			if code := bearerStatus(t, public+"/api/v1/refs?project=github.com/acme/infra", tok); code != http.StatusUnauthorized {
				t.Errorf("token after reuse detection: %d", code)
			}
		}
		if st, m := tokenReq(url.Values{"grant_type": {"password"}, "client_id": {cimdURL}}); st != 400 || m["error"] != "unsupported_grant_type" {
			t.Errorf("password grant: %d %v", st, m)
		}
		req, _ := http.NewRequest(http.MethodPost, public+"/oauth/token", strings.NewReader(base.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(cimdURL, "secret")
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("client_secret_basic accepted: %v %v", resp.StatusCode, err)
		}
	})

	t.Run("private_key_jwt client", func(t *testing.T) {
		verifier := strings.Repeat("j", 64)
		sum := sha256.Sum256([]byte(verifier))
		authURL := public + "/oauth/authorize?" + url.Values{
			"response_type": {"code"}, "client_id": {jwtClientURL}, "redirect_uri": {"https://assistant.example/oauth/cb"},
			"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "scope": {"memory:read"},
		}.Encode()
		bot := &consentBot{t: t, password: password, access: "read", agent: "jwt-assistant"}
		loc, err := bot.run(authURL)
		if err != nil {
			t.Fatalf("authorize: %v (%d: %.300s)", err, bot.lastCode, bot.lastPage)
		}
		code := loc.Query().Get("code")
		assertion := func(key *rsa.PrivateKey, aud string, exp time.Time, jti string) string {
			return signRS256(t, key, "k1", map[string]any{"iss": jwtClientURL, "sub": jwtClientURL, "aud": aud, "exp": exp.Unix(), "iat": time.Now().Unix(), "jti": jti})
		}
		post := func(path string, v url.Values) (int, map[string]any) {
			resp, err := http.PostForm(public+path, v)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var m map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&m)
			return resp.StatusCode, m
		}
		exchange := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://assistant.example/oauth/cb"}, "code_verifier": {verifier}}
		with := func(extra url.Values) url.Values {
			v := url.Values{}
			for k, vv := range exchange {
				v[k] = vv
			}
			for k, vv := range extra {
				v[k] = vv
			}
			return v
		}
		const jwtType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		soon := time.Now().Add(time.Minute)
		// Client authentication fails before the code is touched, so the code survives these.
		for name, extra := range map[string]url.Values{
			"client_id only": {"client_id": {jwtClientURL}},
			"wrong key":      {"client_assertion_type": {jwtType}, "client_assertion": {assertion(other, public, soon, "a1")}},
			"wrong audience": {"client_assertion_type": {jwtType}, "client_assertion": {assertion(jwtKey, "https://evil.example", soon, "a2")}},
			"expired":        {"client_assertion_type": {jwtType}, "client_assertion": {assertion(jwtKey, public, time.Now().Add(-5*time.Minute), "a3")}},
			"wrong type":     {"client_assertion_type": {"urn:x"}, "client_assertion": {assertion(jwtKey, public, soon, "a4")}},
			"mismatched id":  {"client_id": {cimdURL}, "client_assertion_type": {jwtType}, "client_assertion": {assertion(jwtKey, public, soon, "a5")}},
		} {
			if st, m := post("/oauth/token", with(extra)); st != http.StatusUnauthorized || m["error"] != "invalid_client" {
				t.Errorf("%s: %d %v", name, st, m)
			}
		}
		st, m := post("/oauth/token", with(url.Values{"client_assertion_type": {jwtType}, "client_assertion": {assertion(jwtKey, public+"/oauth/token", soon, "ok1")}}))
		if st != 200 || m["scope"] != "memory:read" {
			t.Fatalf("exchange with a valid assertion: %d %v", st, m)
		}
		refreshWith := func(a string) (int, map[string]any) {
			return post("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {m["refresh_token"].(string)}, "client_assertion_type": {jwtType}, "client_assertion": {a}})
		}
		replayed := assertion(jwtKey, public, soon, "ok1")
		if st, r := refreshWith(replayed); st != http.StatusUnauthorized || !strings.Contains(fmt.Sprint(r["error_description"]), "already used") {
			t.Errorf("replayed jti: %d %v", st, r)
		}
		st, r := refreshWith(assertion(jwtKey, public, soon, "ok2"))
		if st != 200 {
			t.Fatalf("refresh: %d %v", st, r)
		}
		access := r["access_token"].(string)
		if code := bearerStatus(t, public+"/api/v1/refs?project=github.com/acme/infra", access); code != http.StatusOK {
			t.Errorf("new access token: %d", code)
		}
		if st, _ := post("/oauth/revoke", url.Values{"token": {r["refresh_token"].(string)}, "client_assertion_type": {jwtType}, "client_assertion": {assertion(jwtKey, public+"/oauth/revoke", soon, "ok3")}}); st != 200 {
			t.Errorf("revoke: %d", st)
		}
		if code := bearerStatus(t, public+"/api/v1/refs?project=github.com/acme/infra", access); code != http.StatusUnauthorized {
			t.Errorf("access token after revoking the refresh token: %d", code)
		}
	})

	t.Run("authorize endpoint negatives", func(t *testing.T) {
		noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		get := func(q url.Values) (int, string) {
			resp, err := noRedirect.Get(public + "/oauth/authorize?" + q.Encode())
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode, resp.Header.Get("Location")
		}
		ok := url.Values{"response_type": {"code"}, "client_id": {cimdURL}, "redirect_uri": {"http://127.0.0.1:9/callback"},
			"code_challenge": {strings.Repeat("A", 43)}, "code_challenge_method": {"S256"}}
		with := func(k, v string) url.Values {
			c := url.Values{}
			for kk, vv := range ok {
				c[kk] = vv
			}
			if v == "" {
				c.Del(k)
			} else {
				c.Set(k, v)
			}
			return c
		}
		// Unverified redirect URIs are never redirected to.
		if st, loc := get(with("redirect_uri", "https://evil.example/cb")); st != 400 || loc != "" {
			t.Errorf("unregistered redirect: %d %q", st, loc)
		}
		// A client that asks for read only is not offered write access.
		resp, err := noRedirect.Get(public + "/oauth/authorize?" + with("scope", "memory:read").Encode())
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(page), `value="write"`) || !strings.Contains(string(page), "asked for read-only access") {
			t.Errorf("read-only request offers write:\n%.400s", page)
		}
		if st, loc := get(with("client_id", "https://127.0.0.1/client.json")); st != 400 || loc != "" {
			t.Errorf("CIMD on a private address: %d %q", st, loc)
		}
		// With a verified redirect URI, errors go back to the client.
		for name, q := range map[string]url.Values{
			"no PKCE":        with("code_challenge", ""),
			"plain PKCE":     with("code_challenge_method", "plain"),
			"implicit":       with("response_type", "token"),
			"foreign target": with("resource", "https://other.example/mcp"),
		} {
			st, loc := get(q)
			if st != http.StatusFound || !strings.HasPrefix(loc, "http://127.0.0.1:9/callback?") || !strings.Contains(loc, "error=") || !strings.Contains(loc, "iss=") {
				t.Errorf("%s: %d %q", name, st, loc)
			}
		}
		// A tampered consent form is refused.
		resp, err = http.PostForm(public+"/oauth/authorize", url.Values{"consent": {"forged.payload"}, "password": {password}, "decision": {"approve"}, "agent": {"x"}, "access": {"write"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("forged consent: %d", resp.StatusCode)
		}
		// A cross-site form post is refused before it reaches the handler.
		req, _ := http.NewRequest(http.MethodPost, public+"/oauth/authorize", strings.NewReader("consent=x"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("cross-site consent post: %d", resp.StatusCode)
		}
	})

	t.Run("revocation via the CLI ends the session", func(t *testing.T) {
		out, _, err := runCLI(t, map[string]string{"KENFOLD_DATABASE_URL": dbURL}, "oauth", "clients")
		if err != nil || !strings.Contains(out, "Test Assistant") || !strings.Contains(out, "chatgpt") || !strings.Contains(out, "read+write") || !strings.Contains(out, "reader") {
			t.Fatalf("oauth clients: %v\n%s", err, out)
		}
		var grant string
		if err := rt.pool.QueryRow(ctx, `SELECT id::text FROM oauth_grant WHERE agent = 'chatgpt'`).Scan(&grant); err != nil {
			t.Fatal(err)
		}
		if out, _, err := runCLI(t, map[string]string{"KENFOLD_DATABASE_URL": dbURL}, "oauth", "revoke", grant[:13]); err != nil || !strings.Contains(out, "Revoked grant") {
			t.Fatalf("oauth revoke: %v %s", err, out)
		}
		// The revoked token is refused; the client's refresh token is dead too, so
		// it must ask the owner again, who now denies it.
		writerBot.deny, writerBot.consentPage = true, ""
		if _, err := writer.CallTool(ctx, &mcp.CallToolParams{Name: "recall", Arguments: map[string]any{"query": "staging"}}); err == nil {
			t.Error("revoked client can still call tools")
		}
		if writerBot.consentPage == "" {
			t.Error("the revoked client was not sent back to the consent page")
		}
		var live int
		if err := rt.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_token t JOIN oauth_grant g ON g.id = t.grant_id WHERE g.id = $1 AND t.revoked_at IS NULL`, grant).Scan(&live); err != nil || live != 0 {
			t.Errorf("live tokens under the revoked grant = %d, %v", live, err)
		}
		writer.Close()
	})

	t.Run("security events are logged", func(t *testing.T) {
		for _, want := range []string{"oauth client registered", "oauth authorization approved", "oauth authorization denied",
			"oauth consent: wrong owner password", "oauth consent locked", "oauth refresh token reused"} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("server log lacks %q", want)
			}
		}
		if strings.Contains(logs.String(), password) {
			t.Error("the owner password appears in the log")
		}
	})

	t.Run("API keys still work next to OAuth", func(t *testing.T) {
		key, _, err := rt.keys.Create(ctx, "claude-code")
		if err != nil {
			t.Fatal(err)
		}
		cs, err := agentSession(ctx, public, "claude-code", key)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		out := callTool[mcpserver.RecallOutput](t, cs, "recall", map[string]any{"query": "staging cluster", "project": "github.com/acme/infra"})
		if len(out.Memories) != 1 || out.Memories[0].SourceAgent != "chatgpt" {
			t.Errorf("recall with an API key = %+v", out.Memories)
		}
	})
}

func getJSON(t *testing.T, u string, out any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func bearerStatus(t *testing.T, u, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// syncBuffer is a bytes.Buffer safe for concurrent writes (server goroutines).
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signRS256 returns a compact RS256 JWS, as a private_key_jwt client makes.
func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	input := b64url(h) + "." + b64url(p)
	d := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64url(sig)
}
