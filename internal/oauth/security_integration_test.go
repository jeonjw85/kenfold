package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/authz"
	"github.com/kenfold/kenfold/migrations"
)

func securityStore(t *testing.T) *Store {
	t.Helper()
	dbURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool)
}

func securityClient(t *testing.T, st *Store, kind string, m ClientMetadata) Client {
	t.Helper()
	if m.ClientID == "" {
		m.ClientID = prefixClient + randomString(32)
	}
	c, err := st.putClient(context.Background(), kind, &m, kind == kindCIMD)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := st.pool.Exec(context.Background(), `DELETE FROM oauth_client WHERE client_id = $1`, c.ID); err != nil {
			t.Error(err)
		}
	})
	return c
}

func securityGrant(t *testing.T, st *Store, c Client, scopes []string) Grant {
	t.Helper()
	g, err := st.createGrant(context.Background(), c.ID, "security-test", scopes, "https://k.example/mcp")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func securityServer(t *testing.T, st *Store) *Server {
	t.Helper()
	s, err := New(st, Config{PublicURL: "https://k.example"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func securityTokenRequest(t *testing.T, s *Server, values url.Values) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, PathToken, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.token(w, r)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode token response: %v, body=%s", err, w.Body.String())
	}
	return w.Code, body
}

func TestAuthorizationCodeClientBindingIntegration(t *testing.T) {
	st := securityStore(t)
	ctx := context.Background()
	const redirectURI = "https://client.example/callback"
	c := securityClient(t, st, kindDCR, ClientMetadata{RedirectURIs: []string{redirectURI}})
	other := securityClient(t, st, kindDCR, ClientMetadata{RedirectURIs: []string{redirectURI}})
	g := securityGrant(t, st, c, []string{authz.ScopeRead})
	code, err := st.createCode(ctx, g.ID, redirectURI, codeChallenge)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range [][2]string{{other.ID, redirectURI}, {c.ID, redirectURI + "/other"}} {
		if _, err := st.exchangeCode(ctx, code, binding[0], binding[1], codeVerifier); !errors.Is(err, errInvalid) {
			t.Fatalf("wrong code binding: %v", err)
		}
	}
	tokens, err := st.exchangeCode(ctx, code, c.ID, redirectURI, codeVerifier)
	if err != nil {
		t.Fatalf("wrong binding consumed the authorization code: %v", err)
	}
	for _, binding := range [][2]string{{other.ID, redirectURI}, {c.ID, redirectURI + "/other"}} {
		if _, err := st.exchangeCode(ctx, code, binding[0], binding[1], codeVerifier); !errors.Is(err, errInvalid) {
			t.Fatalf("wrong binding of used code: %v", err)
		}
		if _, err := st.verifyAccess(ctx, tokens.Access); err != nil {
			t.Fatalf("wrong binding revoked the existing grant: %v", err)
		}
	}
	if _, err := st.exchangeCode(ctx, code, c.ID, redirectURI, codeVerifier); !errors.Is(err, errReuse) {
		t.Fatalf("matching client code reuse: %v", err)
	}
	if _, err := st.verifyAccess(ctx, tokens.Access); !errors.Is(err, errInvalid) {
		t.Fatalf("code reuse failed to revoke tokens: %v", err)
	}
}

const (
	codeVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	codeChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

func TestAuthorizationCodeInvalidPKCELeavesCodeUsableIntegration(t *testing.T) {
	st := securityStore(t)
	s := securityServer(t, st)
	const redirect = "https://client.example/callback"
	c := securityClient(t, st, kindDCR, ClientMetadata{RedirectURIs: []string{redirect}})
	g := securityGrant(t, st, c, []string{authz.ScopeRead})
	code, err := st.createCode(context.Background(), g.ID, redirect, codeChallenge)
	if err != nil {
		t.Fatal(err)
	}
	v := url.Values{"grant_type": {"authorization_code"}, "client_id": {c.ID}, "redirect_uri": {redirect}, "code": {code}}
	for _, verifier := range []string{"", "short", strings.Repeat("b", 43)} {
		v.Set("code_verifier", verifier)
		if status, body := securityTokenRequest(t, s, v); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
			t.Fatalf("invalid PKCE: status=%d error=%v", status, body["error"])
		}
		var unused bool
		if err := st.pool.QueryRow(context.Background(), `SELECT used_at IS NULL FROM oauth_code WHERE code_hash = $1`, hashToken(code)).Scan(&unused); err != nil || !unused {
			t.Errorf("invalid PKCE consumed the code: unused=%v err=%v", unused, err)
		}
	}
	v.Set("code_verifier", codeVerifier)
	if status, body := securityTokenRequest(t, s, v); status != http.StatusOK {
		t.Errorf("valid retry after invalid PKCE rejected: status=%d error=%v", status, body["error"])
	}
}

func TestAuthorizationCodeReplayRequiresPKCEIntegration(t *testing.T) {
	st := securityStore(t)
	s := securityServer(t, st)
	const redirect = "https://client.example/callback"
	c := securityClient(t, st, kindDCR, ClientMetadata{RedirectURIs: []string{redirect}})
	g := securityGrant(t, st, c, []string{authz.ScopeRead})
	code, err := st.createCode(context.Background(), g.ID, redirect, codeChallenge)
	if err != nil {
		t.Fatal(err)
	}
	v := url.Values{"grant_type": {"authorization_code"}, "client_id": {c.ID}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {codeVerifier}}
	status, body := securityTokenRequest(t, s, v)
	if status != http.StatusOK {
		t.Fatalf("initial exchange: status=%d error=%v", status, body["error"])
	}
	access := body["access_token"].(string)
	v.Set("code_verifier", strings.Repeat("b", 43))
	if status, body := securityTokenRequest(t, s, v); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("unauthenticated replay: status=%d error=%v", status, body["error"])
	}
	if _, err := st.verifyAccess(context.Background(), access); err != nil {
		t.Errorf("replay without correct PKCE revoked the grant: %v", err)
	}
	v.Set("code_verifier", codeVerifier)
	if status, body := securityTokenRequest(t, s, v); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("authenticated replay: status=%d error=%v", status, body["error"])
	}
	if _, err := st.verifyAccess(context.Background(), access); !errors.Is(err, errInvalid) {
		t.Errorf("authenticated replay failed to revoke the grant: %v", err)
	}
}

func TestAuthorizationCodeIssuanceFailureRollsBackIntegration(t *testing.T) {
	for _, kind := range []string{"access", "refresh"} {
		t.Run(kind, func(t *testing.T) {
			st := securityStore(t)
			s := securityServer(t, st)
			ctx := context.Background()
			const redirect = "https://client.example/callback"
			c := securityClient(t, st, kindDCR, ClientMetadata{RedirectURIs: []string{redirect}})
			g := securityGrant(t, st, c, []string{authz.ScopeRead})
			code, err := st.createCode(ctx, g.ID, redirect, codeChallenge)
			if err != nil {
				t.Fatal(err)
			}
			// Fail one real INSERT while leaving all other grants unaffected.
			if _, err := st.pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE oauth_token ADD CONSTRAINT test_code_issue_failure CHECK (grant_id <> '%s' OR kind <> '%s')`, g.ID, kind)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := st.pool.Exec(ctx, `ALTER TABLE oauth_token DROP CONSTRAINT IF EXISTS test_code_issue_failure`); err != nil {
					t.Error(err)
				}
			})
			v := url.Values{"grant_type": {"authorization_code"}, "client_id": {c.ID}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {codeVerifier}}
			if status, body := securityTokenRequest(t, s, v); status != http.StatusInternalServerError {
				t.Fatalf("issuance failure: status=%d error=%v", status, body["error"])
			}
			var unused bool
			if err := st.pool.QueryRow(ctx, `SELECT used_at IS NULL FROM oauth_code WHERE code_hash = $1`, hashToken(code)).Scan(&unused); err != nil || !unused {
				t.Errorf("issuance failure consumed code: unused=%v err=%v", unused, err)
			}
			var count int
			if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_token WHERE grant_id = $1`, g.ID).Scan(&count); err != nil || count != 0 {
				t.Errorf("issuance failure left partial tokens: count=%d err=%v", count, err)
			}
			if _, err := st.pool.Exec(ctx, `ALTER TABLE oauth_token DROP CONSTRAINT test_code_issue_failure`); err != nil {
				t.Fatal(err)
			}
			if status, body := securityTokenRequest(t, s, v); status != http.StatusOK {
				t.Errorf("retry after issuance failure rejected: status=%d error=%v", status, body["error"])
			}
		})
	}
}

func TestRefreshScopeValidationIntegration(t *testing.T) {
	st := securityStore(t)
	s := securityServer(t, st)
	c := securityClient(t, st, kindDCR, ClientMetadata{RedirectURIs: []string{"https://client.example/callback"}})
	g := securityGrant(t, st, c, []string{authz.ScopeRead})
	tokens, err := st.issue(context.Background(), st.pool, g, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ID}, "refresh_token": {tokens.Refresh}}
	for _, scope := range []string{authz.ScopeWrite, "unknown", authz.ScopeRead + " unknown", " "} {
		v.Set("scope", scope)
		if status, body := securityTokenRequest(t, s, v); status != http.StatusBadRequest || body["error"] != "invalid_scope" {
			t.Fatalf("invalid scope %q: status %d, body %v", scope, status, body)
		}
		var live bool
		if err := st.pool.QueryRow(context.Background(), `SELECT revoked_at IS NULL FROM oauth_token WHERE token_hash = $1`, hashToken(tokens.Refresh)).Scan(&live); err != nil || !live {
			t.Fatalf("invalid scope %q consumed refresh token: live=%v err=%v", scope, live, err)
		}
	}
	v.Del("scope")
	if status, body := securityTokenRequest(t, s, v); status != http.StatusOK || body["scope"] != authz.ScopeRead {
		t.Fatalf("refresh after invalid requests: status %d, body %v", status, body)
	}
}

func TestRefreshScopeNarrowingIntegration(t *testing.T) {
	st := securityStore(t)
	s := securityServer(t, st)
	c := securityClient(t, st, kindDCR, ClientMetadata{RedirectURIs: []string{"https://client.example/callback"}})
	g := securityGrant(t, st, c, []string{authz.ScopeRead, authz.ScopeWrite})
	tokens, err := st.issue(context.Background(), st.pool, g, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ID}, "refresh_token": {tokens.Refresh}, "scope": {authz.ScopeRead}}
	status, body := securityTokenRequest(t, s, v)
	if status != http.StatusOK || body["scope"] != authz.ScopeRead {
		t.Fatalf("narrow refresh: status %d, body %v", status, body)
	}
	info, err := s.Verify(context.Background(), body["access_token"].(string))
	if err != nil || authz.CanWrite(info) || !slices.Equal(info.Scopes, []string{authz.ScopeRead}) {
		t.Fatalf("narrowed token still allows writes: info=%+v err=%v", info, err)
	}
	v.Set("refresh_token", body["refresh_token"].(string))
	v.Set("scope", authz.ScopeWrite)
	if status, body := securityTokenRequest(t, s, v); status != http.StatusBadRequest || body["error"] != "invalid_scope" {
		t.Fatalf("scope restoration: status %d, body %v", status, body)
	}
	v.Del("scope")
	status, body = securityTokenRequest(t, s, v)
	if status != http.StatusOK || body["scope"] != authz.ScopeRead {
		t.Fatalf("refresh lost narrowed scopes: status %d, body %v", status, body)
	}
}

func TestClientAuthenticationMetadataRefreshIntegration(t *testing.T) {
	st := securityStore(t)
	s := securityServer(t, st)
	signer := newECSigner(t, "rotated")
	var metadata ClientMetadata
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(metadata)
	}))
	defer srv.Close()
	s.SetFetcherForTest(srv.Client().Transport)
	metadata = ClientMetadata{ClientID: srv.URL + "/client.json", RedirectURIs: []string{"https://client.example/callback"}, TokenEndpointAuthMethod: authNone}
	c := securityClient(t, st, kindCIMD, metadata)
	metadata.TokenEndpointAuthMethod = authPrivateKeyJWT
	metadata.JWKS = jwksOf(signer)
	if _, err := st.pool.Exec(context.Background(), `UPDATE oauth_client SET fetched_at = $2 WHERE client_id = $1`, c.ID, time.Now().Add(-2*cimdTTL)); err != nil {
		t.Fatal(err)
	}
	endpoint := s.issuer + PathToken
	if _, err := s.authenticateClient(context.Background(), url.Values{"client_id": {c.ID}}, endpoint); err == nil || !strings.Contains(err.Error(), "must authenticate") {
		t.Fatalf("stale public-client metadata bypassed JWT authentication: %v", err)
	}
	v := url.Values{"client_assertion_type": {assertionTypeJWT}, "client_assertion": {signer.sign(t, claims(c.ID, endpoint, time.Now().Add(time.Minute)))}}
	if _, err := s.authenticateClient(context.Background(), v, endpoint); err != nil {
		t.Fatalf("new JWT authentication failed: %v", err)
	}
}

func TestRefreshRevocationRaceIntegration(t *testing.T) {
	st := securityStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := securityClient(t, st, kindDCR, ClientMetadata{RedirectURIs: []string{"https://client.example/callback"}})
	g := securityGrant(t, st, c, []string{authz.ScopeRead})
	tokens, err := st.issue(ctx, st.pool, g, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Pause a rotation after locking its refresh token and grant. Revocation
	// must wait for the rotation before deciding which tokens to invalidate.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var grantID string
	if err := tx.QueryRow(ctx, `SELECT g.id FROM oauth_token t JOIN oauth_grant g ON g.id = t.grant_id WHERE t.token_hash = $1 FOR UPDATE OF t, g`, hashToken(tokens.Refresh)).Scan(&grantID); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(os.Getenv("KENFOLD_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	appName := "oauth-revoke-test-" + randomString(8)
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	done := make(chan error, 1)
	go func() { done <- NewStore(pool).revokeToken(ctx, c.ID, tokens.Refresh) }()
	for {
		var waiting bool
		if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name = $1 AND wait_event_type = 'Lock')`, appName).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("revocation did not wait for the rotation: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE oauth_token SET revoked_at = now() WHERE token_hash = $1`, hashToken(tokens.Refresh)); err != nil {
		t.Fatal(err)
	}
	rotated, err := st.issue(ctx, tx, g, hashToken(tokens.Refresh))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, access := range []string{tokens.Access, rotated.Access} {
		if _, err := st.verifyAccess(ctx, access); !errors.Is(err, errInvalid) {
			t.Fatalf("an access token survived concurrent refresh revocation: %v", err)
		}
	}
	if _, err := st.refresh(ctx, c.ID, rotated.Refresh, ""); !errors.Is(err, errInvalid) {
		t.Fatalf("new refresh token survived revocation: %v", err)
	}
}
