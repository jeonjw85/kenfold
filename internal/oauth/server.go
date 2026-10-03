// Package oauth is Kenfold's built-in OAuth 2.1 authorization server, for MCP
// clients that cannot be given an API key (ChatGPT, claude.ai, and other
// remote clients that follow the MCP authorization spec).
//
// Kenfold has one owner. A client identifies itself with a Client ID Metadata
// Document (preferred) or dynamic registration (optional, deprecated in
// MCP). The owner approves it on a consent page by entering the owner
// password, choosing read-only or read and write access, and naming the agent
// its writes are attributed to. The flow is authorization code with PKCE
// (S256 only), for public clients. Tokens are opaque, stored as hashes, and
// bound to Kenfold's MCP endpoint as audience. Refresh tokens rotate, and a
// reused one revokes the grant.
package oauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/kenfold/kenfold/internal/apikey"
	"github.com/kenfold/kenfold/internal/authz"
	"github.com/kenfold/kenfold/internal/memory"
)

// Paths, relative to the public base URL.
const (
	PathASMetadata  = "/.well-known/oauth-authorization-server"
	PathPRMetadata  = "/.well-known/oauth-protected-resource"
	PathAuthorize   = "/oauth/authorize"
	PathToken       = "/oauth/token"
	PathRegister    = "/oauth/register"
	PathRevoke      = "/oauth/revoke"
	maxFormBytes    = 64 << 10
	consentValidity = 10 * time.Minute
)

// Config configures the authorization server.
type Config struct {
	// PublicURL is the base URL clients reach Kenfold at, e.g.
	// https://kenfold.example.com. It is the issuer; the MCP endpoint at
	// PublicURL + MCPPath is the only resource tokens are issued for.
	PublicURL string
	MCPPath   string
	// DCR enables dynamic client registration (RFC 7591).
	DCR bool
	// Fetcher fetches Client ID Metadata Documents (default: NewCIMDFetcher(false)).
	Fetcher *CIMDFetcher
	Logger  *slog.Logger
}

// Server serves the OAuth endpoints.
type Server struct {
	cfg      Config
	store    *Store
	issuer   string
	resource string
	fetcher  *CIMDFetcher
	log      *slog.Logger
	csrfKey  []byte
	jwks     jwksCache   // private_key_jwt clients' key sets
	replay   replayCache // jti of verified client assertions
}

// New returns a Server. PublicURL must be an absolute http(s) URL.
func New(st *Store, cfg Config) (*Server, error) {
	issuer := canonicalResource(cfg.PublicURL)
	if issuer == "" {
		return nil, errors.New("oauth: KENFOLD_PUBLIC_URL must be an absolute http(s) URL")
	}
	if u, _ := url.Parse(issuer); u.Path != "" {
		return nil, errors.New("oauth: KENFOLD_PUBLIC_URL must not have a path (serve Kenfold at the root of its host)")
	}
	if cfg.MCPPath == "" {
		cfg.MCPPath = "/mcp"
	}
	if cfg.Fetcher == nil {
		cfg.Fetcher = NewCIMDFetcher(false)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Server{
		cfg: cfg, store: st, issuer: issuer, resource: issuer + cfg.MCPPath, fetcher: cfg.Fetcher, log: cfg.Logger,
		csrfKey: []byte(randomString(32)),
	}, nil
}

// SetLogger sets the logger for security events (registrations, approvals,
// denials, failed owner passwords, lockouts, token reuse). Call it before
// serving requests.
func (s *Server) SetLogger(l *slog.Logger) {
	if l != nil {
		s.log = l
	}
}

// SetFetcherForTest replaces the CIMD fetcher's transport and lifts its
// address checks, so tests can serve metadata documents from loopback TLS
// servers. Never call it outside tests.
func (s *Server) SetFetcherForTest(rt http.RoundTripper) {
	f := NewCIMDFetcher(true)
	f.client.Transport = rt
	s.fetcher = f
}

// Resource returns the canonical URI of the protected MCP endpoint.
func (s *Server) Resource() string { return s.resource }

// ResourceMetadataURL returns the Protected Resource Metadata URL advertised
// in WWW-Authenticate challenges.
func (s *Server) ResourceMetadataURL() string { return s.issuer + PathPRMetadata + s.cfg.MCPPath }

// Routes registers the OAuth endpoints on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	prm := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.resource,
		AuthorizationServers:   []string{s.issuer},
		ScopesSupported:        authz.Supported,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Kenfold",
	})
	// RFC 9728: /.well-known/oauth-protected-resource + the resource's path;
	// the bare path is served too for clients that probe it.
	mux.Handle(PathPRMetadata+s.cfg.MCPPath, prm)
	mux.Handle(PathPRMetadata, prm)
	mux.HandleFunc(PathASMetadata, s.asMetadata)
	mux.HandleFunc("/.well-known/openid-configuration", s.asMetadata) // some clients only probe OIDC discovery
	mux.HandleFunc(PathAuthorize, s.authorize)
	mux.HandleFunc(PathToken, s.token)
	mux.HandleFunc(PathRevoke, s.revoke)
	if s.cfg.DCR {
		mux.HandleFunc(PathRegister, s.register)
	}
}

func (s *Server) asMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m := map[string]any{
		"issuer":                                                s.issuer,
		"authorization_endpoint":                                s.issuer + PathAuthorize,
		"token_endpoint":                                        s.issuer + PathToken,
		"revocation_endpoint":                                   s.issuer + PathRevoke,
		"response_types_supported":                              []string{"code"},
		"response_modes_supported":                              []string{"query"},
		"grant_types_supported":                                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":                      []string{"S256"},
		"token_endpoint_auth_methods_supported":                 []string{authNone, authPrivateKeyJWT},
		"token_endpoint_auth_signing_alg_values_supported":      supportedSigningAlgs,
		"revocation_endpoint_auth_methods_supported":            []string{authNone, authPrivateKeyJWT},
		"revocation_endpoint_auth_signing_alg_values_supported": supportedSigningAlgs,
		"scopes_supported":                                      authz.Supported,
		"client_id_metadata_document_supported":                 true,
		"authorization_response_iss_parameter_supported":        true,
	}
	if s.cfg.DCR {
		m["registration_endpoint"] = s.issuer + PathRegister
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, m)
}

// ---- dynamic client registration (RFC 7591) ----

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var m ClientMetadata
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFormBytes)).Decode(&m); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be a JSON client metadata document")
		return
	}
	if err := m.validate(kindDCR); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	id, _, err := newToken(prefixClient)
	if err != nil {
		s.serverError(w, r, "register", err)
		return
	}
	m.ClientID = id
	if len(m.GrantTypes) == 0 {
		m.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	m.ResponseTypes = []string{"code"}
	m.TokenEndpointAuthMethod = "none"
	if _, err := s.store.putClient(r.Context(), "dcr", &m, false); err != nil {
		s.serverError(w, r, "register", err)
		return
	}
	s.log.InfoContext(r.Context(), "oauth client registered", "client_id", id, "client_name", m.ClientName)
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  m.ClientID,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                m.ClientName,
		"redirect_uris":              m.RedirectURIs,
		"grant_types":                m.GrantTypes,
		"response_types":             m.ResponseTypes,
		"token_endpoint_auth_method": "none",
	})
}

// client returns the client for id, fetching (or re-fetching) its Client ID
// Metadata Document when id is a URL.
func (s *Server) client(ctx context.Context, id string) (Client, error) {
	c, err := s.store.getClient(ctx, id)
	if err == nil && (c.Kind == "dcr" || (c.FetchedAt != nil && time.Since(*c.FetchedAt) < cimdTTL)) {
		return c, nil
	}
	if err != nil && !errors.Is(err, errNotFound) {
		return Client{}, err
	}
	if !s.fetcher.Accepts(id) {
		return Client{}, errNotFound
	}
	m, ferr := s.fetcher.Fetch(ctx, id)
	if ferr != nil {
		return Client{}, ferr
	}
	return s.store.putClient(ctx, "cimd", m, true)
}

// ---- authorization endpoint ----

// authRequest is a validated authorization request.
type authRequest struct {
	ClientID      string
	RedirectURI   string
	State         string
	CodeChallenge string
	Scopes        []string
	Resource      string
	client        Client
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer"><title>Kenfold: connect {{.ClientName}}</title>
<style>
body{font:16px/1.5 system-ui,-apple-system,sans-serif;max-width:34rem;margin:3rem auto;padding:0 1rem;color:#1b1b1b;background:#fff}
h1{font-size:1.4rem}.box{border:1px solid #ccc;border-radius:8px;padding:1rem 1.25rem;margin:1rem 0}
label{display:block;margin:.75rem 0 .25rem;font-weight:600}input[type=password],input[type=text]{width:100%;box-sizing:border-box;padding:.5rem;font-size:1rem;border:1px solid #767676;border-radius:4px}
fieldset{border:0;padding:0;margin:.75rem 0}legend{font-weight:600}.opt{display:flex;gap:.5rem;align-items:flex-start;margin:.35rem 0}
.err{color:#a00000;font-weight:600}.muted{color:#555;font-size:.9rem}code{background:#f2f2f2;padding:0 .25rem;border-radius:3px}
button{font-size:1rem;padding:.55rem 1.1rem;border-radius:4px;border:1px solid #0b57d0;background:#0b57d0;color:#fff;cursor:pointer;margin-right:.5rem}
button.secondary{background:#fff;color:#0b57d0}button:focus-visible,input:focus-visible{outline:3px solid #f5a623;outline-offset:2px}
@media (prefers-color-scheme:dark){body{background:#121212;color:#eee}.box{border-color:#444}.muted{color:#aaa}code{background:#2a2a2a}input[type=password],input[type=text]{background:#1e1e1e;color:#eee;border-color:#888}.err{color:#ff8a80}}
</style></head><body><main>
<h1>Connect {{.ClientName}} to your Kenfold memory?</h1>
<div class="box">
<p><strong>{{.ClientName}}</strong> is asking for access to the memory shared by your AI agents.</p>
<p class="muted">Client: <code>{{.ClientID}}</code><br>After approval you are sent back to: <code>{{.RedirectHost}}</code></p>
</div>
{{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
<form method="post" action="{{.Action}}">
<input type="hidden" name="consent" value="{{.Consent}}">
<fieldset><legend>Access</legend>
<div class="opt"><input type="radio" id="ro" name="access" value="read" checked><label for="ro" style="margin:0;font-weight:400">Read only: search and load memories</label></div>
{{if .WriteAllowed}}<div class="opt"><input type="radio" id="rw" name="access" value="write"><label for="rw" style="margin:0;font-weight:400">Read and write: also store, replace, and forget memories</label></div>{{else}}<p class="muted">This client asked for read-only access.</p>{{end}}
</fieldset>
<label for="agent">Agent name <span class="muted">(recorded on every memory it writes)</span></label>
<input type="text" id="agent" name="agent" value="{{.Agent}}" pattern="[a-z0-9][a-z0-9._\-]{0,63}" required autocomplete="off" spellcheck="false">
<label for="password">Kenfold owner password</label>
<input type="password" id="password" name="password" required autocomplete="current-password" autofocus>
<p><button type="submit" name="decision" value="approve">Allow</button><button type="submit" name="decision" value="deny" class="secondary" formnovalidate>Deny</button></p>
</form>
<p class="muted">Memory content is served to the client as data. Review what you allow: a client with write access can change what your other agents are told.</p>
</main></body></html>`))

type consentData struct {
	ClientName, ClientID, RedirectHost, Consent, Action, Agent, Error string
	WriteAllowed                                                      bool
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		req, redirectable, err := s.parseAuthRequest(r.Context(), r.URL.Query())
		if err != nil {
			s.authError(w, r, req, redirectable, err)
			return
		}
		s.showConsent(w, req, "", http.StatusOK)
	case http.MethodPost:
		s.decide(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// authFailure is an authorization error with an OAuth error code.
type authFailure struct{ code, desc string }

func (e *authFailure) Error() string { return e.code + ": " + e.desc }

// parseAuthRequest validates an authorization request. redirectable reports
// whether errors may be sent to the redirect URI (only once the client and
// redirect URI are verified; before that they are shown to the user).
func (s *Server) parseAuthRequest(ctx context.Context, q url.Values) (authRequest, bool, error) {
	req := authRequest{ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), State: q.Get("state")}
	if req.ClientID == "" {
		return req, false, &authFailure{"invalid_request", "client_id is required"}
	}
	c, err := s.client(ctx, req.ClientID)
	if errors.Is(err, errNotFound) {
		return req, false, &authFailure{"invalid_client", "unknown client; register it or use a client metadata document URL"}
	}
	if err != nil {
		return req, false, &authFailure{"invalid_client", "the client's metadata could not be loaded: " + err.Error()}
	}
	req.client = c
	if req.RedirectURI == "" && len(c.RedirectURIs) == 1 {
		req.RedirectURI = c.RedirectURIs[0]
	}
	if req.RedirectURI == "" || !redirectMatches(c.RedirectURIs, req.RedirectURI) {
		return req, false, &authFailure{"invalid_request", "redirect_uri is not registered for this client"}
	}
	// From here on, errors go back to the client.
	if q.Get("response_type") != "code" {
		return req, true, &authFailure{"unsupported_response_type", "only response_type=code is supported"}
	}
	if q.Get("code_challenge_method") != "S256" || !pkceChallengeRE.MatchString(q.Get("code_challenge")) {
		return req, true, &authFailure{"invalid_request", "PKCE with code_challenge_method=S256 is required"}
	}
	req.CodeChallenge = q.Get("code_challenge")
	if len(req.State) > 1024 {
		return req, true, &authFailure{"invalid_request", "state is too long"}
	}
	res := canonicalResource(q.Get("resource"))
	if q.Get("resource") != "" && res != s.resource {
		return req, true, &authFailure{"invalid_target", "tokens are only issued for " + s.resource}
	}
	req.Resource = s.resource
	req.Scopes = authz.Normalize(q.Get("scope"))
	if len(req.Scopes) == 0 {
		// No (known) scope requested: the owner chooses on the consent page.
		req.Scopes = []string{authz.ScopeRead, authz.ScopeWrite}
	}
	return req, true, nil
}

// authError reports an authorization error: to the client when redirectable,
// otherwise on an error page (never redirecting to an unverified URI).
func (s *Server) authError(w http.ResponseWriter, r *http.Request, req authRequest, redirectable bool, err error) {
	var f *authFailure
	if !errors.As(err, &f) {
		s.serverError(w, r, "authorize", err)
		return
	}
	if redirectable {
		q := url.Values{"error": {f.code}, "error_description": {f.desc}, "iss": {s.issuer}}
		if req.State != "" {
			q.Set("state", req.State)
		}
		redirect(w, req.RedirectURI, q)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte("Kenfold could not start the authorization: " + f.desc + "\n"))
}

func (s *Server) showConsent(w http.ResponseWriter, req authRequest, errMsg string, status int) {
	name := req.client.Name
	if name == "" {
		name = hostOf(req.ClientID)
	}
	agent := suggestAgent(name, req.ClientID)
	data := consentData{
		ClientName: name, ClientID: req.ClientID, RedirectHost: hostOf(req.RedirectURI),
		Consent: s.sealConsent(req), Action: PathAuthorize, Agent: agent, Error: errMsg,
		// Read-only is preselected (least privilege); write is one click away
		// when the client asked for it.
		WriteAllowed: !authz.ReadOnly(req.Scopes),
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// The page must not be framed (clickjacking) and runs no scripts.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = consentPage.Execute(w, data)
}

// sealConsent serializes the validated request with an expiry and an HMAC,
// so the consent form cannot be altered (CSRF, parameter tampering) and the
// POST does not trust anything the browser sends besides the owner's choices.
func (s *Server) sealConsent(req authRequest) string {
	payload, _ := json.Marshal(struct {
		C, R, S, P, Res string
		Sc              []string
		Exp             int64
	}{req.ClientID, req.RedirectURI, req.State, req.CodeChallenge, req.Resource, req.Scopes, time.Now().Add(consentValidity).Unix()})
	mac := hmac.New(sha256.New, s.csrfKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) openConsent(v string) (authRequest, bool) {
	p, sig, ok := strings.Cut(v, ".")
	if !ok {
		return authRequest{}, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	got, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return authRequest{}, false
	}
	mac := hmac.New(sha256.New, s.csrfKey)
	mac.Write(payload)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return authRequest{}, false
	}
	var c struct {
		C, R, S, P, Res string
		Sc              []string
		Exp             int64
	}
	if json.Unmarshal(payload, &c) != nil || time.Now().Unix() > c.Exp {
		return authRequest{}, false
	}
	return authRequest{ClientID: c.C, RedirectURI: c.R, State: c.S, CodeChallenge: c.P, Resource: c.Res, Scopes: c.Sc}, true
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	req, ok := s.openConsent(r.PostForm.Get("consent"))
	if !ok {
		http.Error(w, "This consent form has expired or was altered. Start the connection again from the client.", http.StatusBadRequest)
		return
	}
	c, err := s.store.getClient(r.Context(), req.ClientID)
	if err != nil || !redirectMatches(c.RedirectURIs, req.RedirectURI) {
		http.Error(w, "The client is no longer registered. Start the connection again from the client.", http.StatusBadRequest)
		return
	}
	req.client = c
	back := url.Values{"iss": {s.issuer}}
	if req.State != "" {
		back.Set("state", req.State)
	}
	if r.PostForm.Get("decision") != "approve" {
		back.Set("error", "access_denied")
		back.Set("error_description", "the owner denied access")
		s.log.InfoContext(r.Context(), "oauth authorization denied", "client_id", req.ClientID)
		redirect(w, req.RedirectURI, back)
		return
	}
	agent := strings.TrimSpace(r.PostForm.Get("agent"))
	if !memory.ValidAgent(agent) || reservedAgent(agent) {
		s.showConsent(w, req, "Choose an agent name: lowercase letters, digits, '.', '_', '-' (for example chatgpt).", http.StatusBadRequest)
		return
	}
	scopes := []string{authz.ScopeRead}
	if r.PostForm.Get("access") == "write" {
		if !slices.Contains(req.Scopes, authz.ScopeWrite) {
			s.showConsent(w, req, "This client asked for read-only access.", http.StatusBadRequest)
			return
		}
		scopes = []string{authz.ScopeRead, authz.ScopeWrite}
	}

	switch err := s.store.CheckOwner(r.Context(), r.PostForm.Get("password")); {
	case errors.Is(err, errNoPassword):
		s.showConsent(w, req, "No owner password is set. On the server, run: kenfold password", http.StatusForbidden)
		return
	case errors.Is(err, errLocked):
		s.log.WarnContext(r.Context(), "oauth consent locked after failed password attempts")
		s.showConsent(w, req, "Too many wrong passwords. Try again in 15 minutes.", http.StatusTooManyRequests)
		return
	case errors.Is(err, ErrWrongPassword):
		s.log.WarnContext(r.Context(), "oauth consent: wrong owner password", "client_id", req.ClientID)
		s.showConsent(w, req, "Wrong password.", http.StatusUnauthorized)
		return
	case err != nil:
		s.serverError(w, r, "authorize", err)
		return
	}

	g, err := s.store.createGrant(r.Context(), req.ClientID, agent, scopes, req.Resource)
	if err != nil {
		s.serverError(w, r, "authorize", err)
		return
	}
	code, err := s.store.createCode(r.Context(), g.ID, req.RedirectURI, req.CodeChallenge)
	if err != nil {
		s.serverError(w, r, "authorize", err)
		return
	}
	s.log.InfoContext(r.Context(), "oauth authorization approved", "client_id", req.ClientID, "agent", agent, "scopes", scopes, "grant", g.ID)
	back.Set("code", code)
	redirect(w, req.RedirectURI, back)
}

// ---- token endpoint ----

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "use POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "bad form body")
		return
	}
	f := r.PostForm
	clientID, ok := s.clientOf(w, r, s.issuer+PathToken)
	if !ok {
		return
	}
	if res := f.Get("resource"); res != "" && canonicalResource(res) != s.resource {
		oauthError(w, http.StatusBadRequest, "invalid_target", "tokens are only issued for "+s.resource)
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		rec, err := s.store.useCode(r.Context(), f.Get("code"), clientID, f.Get("redirect_uri"))
		switch {
		case errors.Is(err, errReuse):
			s.log.WarnContext(r.Context(), "oauth authorization code reused; grant revoked", "client_id", clientID)
			oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code already used; the grant was revoked")
			return
		case errors.Is(err, errInvalid):
			oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid, expired, or used authorization code")
			return
		case err != nil:
			s.serverError(w, r, "token", err)
			return
		}
		if !pkceMatches(f.Get("code_verifier"), rec.challenge) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		t, err := s.store.issue(r.Context(), s.store.pool, rec.grant, nil)
		if err != nil {
			s.serverError(w, r, "token", err)
			return
		}
		writeTokens(w, t)
	case "refresh_token":
		t, err := s.store.refresh(r.Context(), clientID, f.Get("refresh_token"), f.Get("scope"))
		switch {
		case errors.Is(err, errReuse):
			s.log.WarnContext(r.Context(), "oauth refresh token reused; grant revoked", "client_id", clientID)
			oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token already used; the grant was revoked, authorize again")
			return
		case errors.Is(err, errInvalid):
			oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
			return
		case errors.Is(err, errInvalidScope):
			oauthError(w, http.StatusBadRequest, "invalid_scope", "a refresh cannot exceed the token's granted scopes")
			return
		case err != nil:
			s.serverError(w, r, "token", err)
			return
		}
		writeTokens(w, t)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "use POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "bad form body")
		return
	}
	clientID, ok := s.clientOf(w, r, s.issuer+PathRevoke)
	if !ok {
		return
	}
	if err := s.store.revokeToken(r.Context(), clientID, r.PostForm.Get("token")); err != nil {
		s.serverError(w, r, "revoke", err)
		return
	}
	w.WriteHeader(http.StatusOK) // RFC 7009: also for unknown tokens
}

// clientOf authenticates the client of a token or revocation request and
// writes the error response when that fails.
func (s *Server) clientOf(w http.ResponseWriter, r *http.Request, endpoint string) (string, bool) {
	if _, _, hasBasic := r.BasicAuth(); hasBasic || r.PostForm.Get("client_secret") != "" {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "Kenfold does not use client secrets: send client_id (public clients) or a client_assertion (private_key_jwt)")
		return "", false
	}
	c, err := s.authenticateClient(r.Context(), r.PostForm, endpoint)
	var cae *clientAuthError
	switch {
	case errors.As(err, &cae):
		s.log.WarnContext(r.Context(), "oauth client authentication failed", "endpoint", endpoint, "reason", cae.desc)
		oauthError(w, http.StatusUnauthorized, "invalid_client", cae.desc)
		return "", false
	case err != nil:
		s.serverError(w, r, "client authentication", err)
		return "", false
	}
	return c.ID, true
}

// ---- resource server side ----

// Verify checks an access token for the MCP endpoint and returns the token
// info the MCP server and REST API use: the grant's agent, its scopes, and
// read-only status. Tokens are bound to s.Resource() at issuance.
func (s *Server) Verify(ctx context.Context, token string) (*auth.TokenInfo, error) {
	if !strings.HasPrefix(token, prefixAccess) {
		return nil, auth.ErrInvalidToken
	}
	a, err := s.store.verifyAccess(ctx, token)
	if errors.Is(err, errInvalid) {
		return nil, auth.ErrInvalidToken
	}
	if err != nil {
		s.log.ErrorContext(ctx, "oauth token verification failed", "err", err)
		return nil, errors.New("token verification is temporarily unavailable")
	}
	if a.Resource != s.resource {
		return nil, auth.ErrInvalidToken // issued for another resource (e.g. before KENFOLD_PUBLIC_URL changed)
	}
	return &auth.TokenInfo{
		Scopes:     a.Scopes,
		Expiration: a.Expires,
		UserID:     "oauth:" + a.GrantID,
		Extra: map[string]any{
			apikey.ExtraAgent:   a.Agent,
			authz.ExtraReadOnly: authz.ReadOnly(a.Scopes),
			"kenfold.client_id": a.ClientID,
		},
	}, nil
}

// IsAccessToken reports whether t looks like a Kenfold OAuth access token.
func IsAccessToken(t string) bool { return strings.HasPrefix(t, prefixAccess) }

// ---- helpers ----

func writeTokens(w http.ResponseWriter, t Tokens) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  t.Access,
		"token_type":    "Bearer",
		"expires_in":    t.ExpiresIn,
		"refresh_token": t.Refresh,
		"scope":         strings.Join(t.Scopes, " "),
	})
}

func oauthError(w http.ResponseWriter, code int, errCode, desc string) {
	writeJSON(w, code, map[string]string{"error": errCode, "error_description": desc})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.log.ErrorContext(r.Context(), "oauth request failed", "op", op, "err", err)
	oauthError(w, http.StatusInternalServerError, "server_error", "internal error; the Kenfold server log has details")
}

// redirect sends the user agent to uri with q added to its query.
func redirect(w http.ResponseWriter, uri string, q url.Values) {
	u, err := url.Parse(uri)
	if err != nil {
		http.Error(w, "bad redirect URI", http.StatusBadRequest)
		return
	}
	existing := u.Query()
	for k, vs := range q {
		existing[k] = vs
	}
	u.RawQuery = existing.Encode()
	w.Header().Set("Location", u.String())
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
}

func hostOf(s string) string {
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host
	}
	return s
}

// suggestAgent proposes an agent name from the client's name or host.
func suggestAgent(name, clientID string) string {
	for _, cand := range []string{name, hostOf(clientID)} {
		low := strings.ToLower(cand)
		switch {
		case strings.Contains(low, "chatgpt") || strings.Contains(low, "openai"):
			return "chatgpt"
		case strings.Contains(low, "claude"):
			return "claude-ai"
		}
	}
	a := memory.SanitizeAgent(name)
	a = strings.TrimRight(a[:min(len(a), 40)], "-._")
	if !memory.ValidAgent(a) || reservedAgent(a) {
		return "remote-client"
	}
	return a
}

// reservedAgent reports agent names a grant may not use: they would make its
// writes look like Kenfold's own.
func reservedAgent(a string) bool {
	return a == memory.UnknownAgent || a == "kenfold" || strings.HasPrefix(a, "kenfold-")
}

func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}
