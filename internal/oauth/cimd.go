package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Client ID Metadata Documents (draft-ietf-oauth-client-id-metadata-document):
// a client identifies itself with an https URL; the authorization server
// fetches the JSON document at that URL to learn the client's name and
// redirect URIs. The URL is chosen by whoever starts an authorization, so the
// fetch is an SSRF vector and is locked down:
//
//   - https only, default port, no credentials, fragment, or dot segments;
//   - only public unicast addresses, checked at dial time (after DNS), so a
//     name that resolves to a private or loopback address is refused even
//     if it changes between checks;
//   - no redirects, 5 s timeout, 64 KiB body limit;
//   - the document's client_id must equal the URL.

const (
	cimdTimeout  = 5 * time.Second
	cimdMaxBytes = 64 << 10
	// cimdTTL is how long a fetched document is trusted before re-fetching.
	cimdTTL = 24 * time.Hour
)

// ClientMetadata is the subset of a client's metadata Kenfold uses.
type ClientMetadata struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
	// For private_key_jwt (RFC 7523) clients: where their public keys are,
	// and the algorithm they sign client assertions with.
	JWKSURI                     string          `json:"jwks_uri,omitempty"`
	JWKS                        json.RawMessage `json:"jwks,omitempty"`
	TokenEndpointAuthSigningAlg string          `json:"token_endpoint_auth_signing_alg,omitempty"`
	// TokenEndpointAuthMethods is a non-standard list some metadata documents
	// publish instead of token_endpoint_auth_method.
	TokenEndpointAuthMethods []string `json:"token_endpoint_auth_methods_supported,omitempty"`
}

// Client kinds and authentication methods.
const (
	kindCIMD          = "cimd"
	kindDCR           = "dcr"
	authNone          = "none"
	authPrivateKeyJWT = "private_key_jwt"
)

// authMethod is how the client authenticates at the token endpoint: the
// declared method, or, from a list of methods, private_key_jwt when the
// client publishes keys (it is the stronger one) and none otherwise.
func (m *ClientMetadata) authMethod() string {
	if m.TokenEndpointAuthMethod != "" {
		return m.TokenEndpointAuthMethod
	}
	hasKeys := m.JWKSURI != "" || len(m.JWKS) > 0
	switch {
	case slices.Contains(m.TokenEndpointAuthMethods, authPrivateKeyJWT) && hasKeys:
		return authPrivateKeyJWT
	case len(m.TokenEndpointAuthMethods) == 0 || slices.Contains(m.TokenEndpointAuthMethods, authNone):
		return authNone
	}
	return m.TokenEndpointAuthMethods[0]
}

// validate checks client metadata. kind is kindCIMD (a metadata document,
// which may use private_key_jwt) or kindDCR (dynamic registration: public
// clients only). Grant and response types beyond the authorization code flow
// are dropped rather than rejected: documents written for several
// authorization servers list extras (such as the JWT bearer grant), and the
// token endpoint refuses grants it does not implement anyway.
func (m *ClientMetadata) validate(kind string) error {
	if len(m.RedirectURIs) == 0 || len(m.RedirectURIs) > 20 {
		return errors.New("redirect_uris must list 1 to 20 URIs")
	}
	for _, r := range m.RedirectURIs {
		if err := validRedirectURI(r); err != nil {
			return err
		}
	}
	if len(m.GrantTypes) > 0 && !slices.Contains(m.GrantTypes, "authorization_code") {
		return errors.New("grant_types must include authorization_code")
	}
	m.GrantTypes = slices.DeleteFunc(m.GrantTypes, func(g string) bool { return g != "authorization_code" && g != "refresh_token" })
	if len(m.ResponseTypes) > 0 && !slices.Contains(m.ResponseTypes, "code") {
		return errors.New("response_types must include code")
	}
	m.ResponseTypes = slices.DeleteFunc(m.ResponseTypes, func(r string) bool { return r != "code" })

	switch kind {
	case kindDCR:
		if m.TokenEndpointAuthMethod != "" && m.TokenEndpointAuthMethod != authNone {
			return fmt.Errorf("unsupported token_endpoint_auth_method %q: dynamically registered clients are public (none)", m.TokenEndpointAuthMethod)
		}
		m.TokenEndpointAuthMethod, m.TokenEndpointAuthMethods = authNone, nil
		m.JWKSURI, m.JWKS, m.TokenEndpointAuthSigningAlg = "", nil, ""
	case kindCIMD:
		switch method := m.authMethod(); method {
		case authNone:
		case authPrivateKeyJWT:
			if m.JWKSURI == "" && len(m.JWKS) == 0 {
				return errors.New("private_key_jwt clients must publish jwks_uri or jwks")
			}
			if m.JWKSURI != "" {
				if u, err := url.Parse(m.JWKSURI); err != nil || u.Scheme != "https" || u.Host == "" {
					return errors.New("jwks_uri must be an https URL")
				}
			}
			if alg := m.TokenEndpointAuthSigningAlg; alg != "" && !slices.Contains(supportedSigningAlgs, alg) {
				return fmt.Errorf("unsupported token_endpoint_auth_signing_alg %q (Kenfold verifies %s)", alg, strings.Join(supportedSigningAlgs, ", "))
			}
		default:
			// There is no client secret to share: clients are public (PKCE
			// authenticates the code exchange) or prove their identity with
			// a key they publish.
			return fmt.Errorf("unsupported token_endpoint_auth_method %q (Kenfold supports none and private_key_jwt)", method)
		}
	default:
		return fmt.Errorf("unknown client kind %q", kind)
	}
	m.ClientName = strings.TrimSpace(m.ClientName)
	if len(m.ClientName) > 200 {
		m.ClientName = m.ClientName[:200]
	}
	return nil
}

// IsCIMDClientID reports whether id is (syntactically) a Client ID Metadata
// Document URL: https, with a path, as the draft requires.
func IsCIMDClientID(id string) bool {
	_, err := parseCIMDURL(id, false)
	return err == nil
}

func parseCIMDURL(id string, allowPrivate bool) (*url.URL, error) {
	return parseFetchURL(id, "client_id URL", allowPrivate)
}

// parseFetchURL checks a URL Kenfold will fetch on a client's behalf
// (metadata documents, key sets): https on the default port, with a path,
// without credentials, fragments, dot segments, or a non-public address.
func parseFetchURL(raw, what string, allowPrivate bool) (*url.URL, error) {
	if len(raw) > 2048 {
		return nil, fmt.Errorf("%s is too long", what)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Opaque != "" {
		return nil, fmt.Errorf("%s is not an https URL", what)
	}
	if u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("%s must not contain credentials or a fragment", what)
	}
	if u.Port() != "" && u.Port() != "443" && !allowPrivate {
		return nil, fmt.Errorf("%s must use the default https port", what)
	}
	if u.Path == "" || u.Path == "/" {
		return nil, fmt.Errorf("%s must have a path", what)
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return nil, fmt.Errorf("%s must not contain dot segments", what)
		}
	}
	if ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && !publicAddr(ip) && !allowPrivate {
		return nil, fmt.Errorf("%s points to a non-public address", what)
	}
	return u, nil
}

// publicAddr reports whether ip is a public unicast address.
func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// blockedPrefixes are special-purpose ranges not covered by the netip
// predicates (RFC 6890 and friends).
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
		"2001::/23", "2001:db8::/32", "fc00::/7",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// CIMDFetcher fetches Client ID Metadata Documents.
type CIMDFetcher struct {
	client       *http.Client
	allowPrivate bool // tests: loopback addresses and ports are allowed
}

// NewCIMDFetcher returns a fetcher with the protections described above.
// allowPrivate disables the address check (tests only).
func NewCIMDFetcher(allowPrivate bool) *CIMDFetcher {
	dialer := &net.Dialer{Timeout: cimdTimeout}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !publicAddr(ip) {
				return fmt.Errorf("refusing to connect to non-public address %s", host)
			}
			return nil
		}
	}
	tr := &http.Transport{
		Proxy:                 nil, // a proxy would bypass the address check
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   cimdTimeout,
		ResponseHeaderTimeout: cimdTimeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &CIMDFetcher{allowPrivate: allowPrivate, client: &http.Client{
		Transport: tr,
		Timeout:   cimdTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not followed for client metadata documents")
		},
	}}
}

// Fetch retrieves and validates the metadata document at clientID.
func (f *CIMDFetcher) Fetch(ctx context.Context, clientID string) (*ClientMetadata, error) {
	u, err := parseCIMDURL(clientID, f.allowPrivate)
	if err != nil {
		return nil, err
	}
	return f.fetch(ctx, u.String(), clientID)
}

// Accepts reports whether clientID is a metadata document URL this fetcher
// would fetch.
func (f *CIMDFetcher) Accepts(clientID string) bool {
	_, err := parseCIMDURL(clientID, f.allowPrivate)
	return err == nil
}

func (f *CIMDFetcher) fetch(ctx context.Context, fetchURL, clientID string) (*ClientMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch client metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch client metadata: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch client metadata: %w", err)
	}
	if len(raw) > cimdMaxBytes {
		return nil, errors.New("client metadata document is too large")
	}
	var m ClientMetadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("client metadata document is not valid JSON: %w", err)
	}
	if m.ClientID != clientID {
		return nil, errors.New("client metadata document's client_id does not match its URL")
	}
	if err := m.validate(kindCIMD); err != nil {
		return nil, err
	}
	return &m, nil
}

// fetchJWKS retrieves a client's JSON Web Key Set from jwks_uri, with the
// same protections as metadata documents.
func (f *CIMDFetcher) fetchJWKS(ctx context.Context, uri string) ([]byte, error) {
	u, err := parseFetchURL(uri, "jwks_uri", f.allowPrivate)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch jwks: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	if len(raw) > cimdMaxBytes {
		return nil, errors.New("jwks is too large")
	}
	return raw, nil
}
