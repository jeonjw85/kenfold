package oauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Client authentication at the token and revocation endpoints.
//
// Public clients (token_endpoint_auth_method none) send client_id only: PKCE
// binds the code to whoever started the flow, and refresh tokens rotate.
// Clients whose metadata document declares private_key_jwt (ChatGPT does)
// prove their identity with a JWT signed by a key they publish (RFC 7523
// section 2.2): Kenfold fetches the key set, verifies the signature (RS256,
// PS256, ES256), and checks iss = sub = client_id, aud, exp, and jti reuse.

const (
	assertionTypeJWT  = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	maxAssertionBytes = 8 << 10
	maxAssertionLife  = time.Hour
	assertionSkew     = time.Minute
	jwksTTL           = time.Hour
	jwksRefetchAfter  = time.Minute // on an unknown key id, re-fetch at most this often
	maxJWKSKeys       = 20
	maxReplayEntries  = 100_000
)

// supportedSigningAlgs are the client assertion algorithms Kenfold verifies.
var supportedSigningAlgs = []string{"RS256", "PS256", "ES256"}

// clientAuthError is a failed client authentication (invalid_client).
type clientAuthError struct{ desc string }

func (e *clientAuthError) Error() string { return e.desc }

func authFail(format string, a ...any) error { return &clientAuthError{fmt.Sprintf(format, a...)} }

// ---- key sets ----

type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type verifierKey struct {
	kid string
	alg string // optional restriction from the JWK
	rsa *rsa.PublicKey
	ec  *ecdsa.PublicKey
}

// parseJWKS returns the usable signing keys of a JSON Web Key Set: RSA keys
// of 2048 to 8192 bits and EC P-256 keys. Other keys are skipped.
func parseJWKS(raw []byte) ([]verifierKey, error) {
	var set struct {
		Keys []jsonWebKey `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("the key set is not valid JSON: %w", err)
	}
	var out []verifierKey
	for _, k := range set.Keys {
		if len(out) == maxJWKSKeys {
			break
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		vk := verifierKey{kid: k.Kid, alg: k.Alg}
		switch k.Kty {
		case "RSA":
			n, err1 := b64(k.N)
			e, err2 := b64(k.E)
			if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
				continue
			}
			pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
			if bits := pub.N.BitLen(); bits < 2048 || bits > 8192 || pub.E < 3 || pub.E%2 == 0 {
				continue
			}
			vk.rsa = pub
		case "EC":
			if k.Crv != "P-256" {
				continue
			}
			x, err1 := b64(k.X)
			y, err2 := b64(k.Y)
			if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
				continue
			}
			pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
			if err != nil {
				continue
			}
			vk.ec = pub
		default:
			continue
		}
		out = append(out, vk)
	}
	if len(out) == 0 {
		return nil, errors.New("the key set has no usable signing key (RSA 2048+ or EC P-256)")
	}
	return out, nil
}

func b64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// jwksCache caches clients' key sets by URL.
type jwksCache struct {
	mu      sync.Mutex
	entries map[string]jwksEntry
}

type jwksEntry struct {
	keys    []verifierKey
	fetched time.Time
}

// clientKeys returns the client's signing keys: inline jwks, or its jwks_uri
// (cached for jwksTTL; refresh forces a re-fetch, at most once per
// jwksRefetchAfter, for key rotation).
func (s *Server) clientKeys(ctx context.Context, c Client, refresh bool) ([]verifierKey, error) {
	if len(c.Metadata.JWKS) > 0 {
		return parseJWKS(c.Metadata.JWKS)
	}
	uri := c.Metadata.JWKSURI
	now := s.store.now()
	s.jwks.mu.Lock()
	e, ok := s.jwks.entries[uri]
	s.jwks.mu.Unlock()
	if ok && ((!refresh && now.Sub(e.fetched) < jwksTTL) || (refresh && now.Sub(e.fetched) < jwksRefetchAfter)) {
		return e.keys, nil
	}
	raw, err := s.fetcher.fetchJWKS(ctx, uri)
	if err != nil {
		return nil, err
	}
	keys, err := parseJWKS(raw)
	if err != nil {
		return nil, err
	}
	s.jwks.mu.Lock()
	if s.jwks.entries == nil {
		s.jwks.entries = map[string]jwksEntry{}
	}
	s.jwks.entries[uri] = jwksEntry{keys: keys, fetched: now}
	s.jwks.mu.Unlock()
	return keys, nil
}

// ---- assertions ----

type assertion struct {
	alg, kid     string
	signingInput []byte
	sig          []byte
	claims       assertionClaims
}

type assertionClaims struct {
	Iss string   `json:"iss"`
	Sub string   `json:"sub"`
	Aud audience `json:"aud"`
	Exp *float64 `json:"exp"`
	Nbf *float64 `json:"nbf"`
	Iat *float64 `json:"iat"`
	Jti string   `json:"jti"`
}

// audience is a JWT aud claim: a string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud must be a string or an array of strings")
	}
	*a = many
	return nil
}

// parseAssertion decodes a compact JWS without verifying it.
func parseAssertion(raw string) (*assertion, error) {
	if len(raw) > maxAssertionBytes {
		return nil, authFail("client_assertion is too large")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, authFail("client_assertion is not a signed JWT")
	}
	hb, err := b64(parts[0])
	if err != nil {
		return nil, authFail("client_assertion header is not base64url")
	}
	var h struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, authFail("client_assertion header is not JSON")
	}
	if len(h.Crit) > 0 {
		return nil, authFail("client_assertion uses unsupported critical header parameters")
	}
	if !slices.Contains(supportedSigningAlgs, h.Alg) {
		return nil, authFail("client_assertion alg %q is not supported (use %s)", h.Alg, strings.Join(supportedSigningAlgs, ", "))
	}
	pb, err := b64(parts[1])
	if err != nil {
		return nil, authFail("client_assertion payload is not base64url")
	}
	var c assertionClaims
	if err := json.Unmarshal(pb, &c); err != nil {
		return nil, authFail("client_assertion claims are not valid: %v", err)
	}
	sig, err := b64(parts[2])
	if err != nil || len(sig) == 0 {
		return nil, authFail("client_assertion signature is not base64url")
	}
	return &assertion{alg: h.Alg, kid: h.Kid, signingInput: []byte(parts[0] + "." + parts[1]), sig: sig, claims: c}, nil
}

// verify checks the signature against keys. kidKnown reports whether a key
// with the assertion's key id was among them (for rotation handling).
func (a *assertion) verify(keys []verifierKey) (ok, kidKnown bool) {
	digest := sha256.Sum256(a.signingInput)
	for _, k := range keys {
		if a.kid != "" && k.kid != "" && k.kid != a.kid {
			continue
		}
		if a.kid != "" && k.kid == a.kid {
			kidKnown = true
		}
		if k.alg != "" && k.alg != a.alg {
			continue
		}
		switch a.alg {
		case "RS256":
			if k.rsa != nil && rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, digest[:], a.sig) == nil {
				return true, true
			}
		case "PS256":
			if k.rsa != nil && rsa.VerifyPSS(k.rsa, crypto.SHA256, digest[:], a.sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil {
				return true, true
			}
		case "ES256":
			if k.ec != nil && len(a.sig) == 64 {
				r, sv := new(big.Int).SetBytes(a.sig[:32]), new(big.Int).SetBytes(a.sig[32:])
				if ecdsa.Verify(k.ec, digest[:], r, sv) {
					return true, true
				}
			}
		}
	}
	return false, kidKnown
}

// checkClaims validates an assertion's time and audience claims.
func (s *Server) checkClaims(c assertionClaims, endpoint string) error {
	now := s.store.now()
	if c.Exp == nil {
		return authFail("client_assertion has no exp")
	}
	exp := time.Unix(int64(*c.Exp), 0)
	if now.After(exp.Add(assertionSkew)) {
		return authFail("client_assertion has expired")
	}
	if exp.After(now.Add(maxAssertionLife + assertionSkew)) {
		return authFail("client_assertion is valid for too long (at most an hour)")
	}
	if c.Nbf != nil && time.Unix(int64(*c.Nbf), 0).After(now.Add(assertionSkew)) {
		return authFail("client_assertion is not valid yet")
	}
	if c.Iat != nil && time.Unix(int64(*c.Iat), 0).After(now.Add(assertionSkew)) {
		return authFail("client_assertion was issued in the future")
	}
	// RFC 7523 names the token endpoint; the OAuth 2.1 draft recommends the
	// issuer. Both are accepted, as is the endpoint being called.
	accepted := []string{s.issuer, s.issuer + "/", s.issuer + PathToken, endpoint}
	if !slices.ContainsFunc(c.Aud, func(a string) bool { return slices.Contains(accepted, a) }) {
		return authFail("client_assertion aud must be %s or %s", s.issuer, s.issuer+PathToken)
	}
	return nil
}

// replayCache remembers the jti of verified assertions until they expire.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// use records key until expiry and reports false if it was already recorded.
func (c *replayCache) use(key string, until, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	if len(c.seen) >= maxReplayEntries {
		for k, t := range c.seen {
			if now.After(t) {
				delete(c.seen, k)
			}
		}
	}
	if t, ok := c.seen[key]; ok && now.Before(t) {
		return false
	}
	if len(c.seen) >= maxReplayEntries {
		return false // fail closed instead of letting verified clients grow it without bound
	}
	c.seen[key] = until
	return true
}

// authenticateClient authenticates the client of a token or revocation
// request (endpoint is the URL being called). Failures are *clientAuthError.
func (s *Server) authenticateClient(ctx context.Context, f url.Values, endpoint string) (Client, error) {
	raw, typ := f.Get("client_assertion"), f.Get("client_assertion_type")
	if raw == "" && typ == "" {
		id := f.Get("client_id")
		if id == "" {
			return Client{}, authFail("client_id is required")
		}
		c, err := s.store.getClient(ctx, id)
		if errors.Is(err, errNotFound) {
			return Client{}, authFail("unknown client")
		}
		if err != nil {
			return Client{}, err
		}
		// A metadata document can switch from public to private_key_jwt.
		// Do not trust an old public-client record indefinitely at the token
		// endpoint while authorization and JWT requests refresh its metadata.
		if c.Kind == kindCIMD {
			c, err = s.client(ctx, id)
			if err != nil {
				return Client{}, authFail("the client's metadata could not be loaded: %v", err)
			}
		}
		if c.Metadata.authMethod() == authPrivateKeyJWT {
			return Client{}, authFail("this client must authenticate with a client_assertion (private_key_jwt)")
		}
		return c, nil
	}
	if typ != assertionTypeJWT || raw == "" {
		return Client{}, authFail("client_assertion_type must be %s", assertionTypeJWT)
	}
	a, err := parseAssertion(raw)
	if err != nil {
		return Client{}, err
	}
	id := a.claims.Sub
	if id == "" || a.claims.Iss != id {
		return Client{}, authFail("client_assertion iss and sub must both be the client_id")
	}
	if cid := f.Get("client_id"); cid != "" && cid != id {
		return Client{}, authFail("client_id does not match the client_assertion")
	}
	if err := s.checkClaims(a.claims, endpoint); err != nil {
		return Client{}, err
	}
	c, err := s.client(ctx, id)
	if errors.Is(err, errNotFound) {
		return Client{}, authFail("unknown client")
	}
	if err != nil {
		return Client{}, authFail("the client's metadata could not be loaded: %v", err)
	}
	if c.Metadata.authMethod() != authPrivateKeyJWT {
		return Client{}, authFail("this client is not registered for private_key_jwt")
	}
	if want := c.Metadata.TokenEndpointAuthSigningAlg; want != "" && a.alg != want {
		return Client{}, authFail("client_assertion must be signed with %s", want)
	}
	keys, err := s.clientKeys(ctx, c, false)
	if err != nil {
		return Client{}, authFail("the client's keys could not be loaded: %v", err)
	}
	ok, kidKnown := a.verify(keys)
	if !ok && !kidKnown && len(c.Metadata.JWKS) == 0 {
		// An unknown key id may mean the client rotated its keys.
		if keys, err := s.clientKeys(ctx, c, true); err == nil {
			ok, _ = a.verify(keys)
		}
	}
	if !ok {
		return Client{}, authFail("client_assertion signature is not valid")
	}
	if a.claims.Jti != "" {
		until := time.Unix(int64(*a.claims.Exp), 0).Add(assertionSkew)
		if !s.replay.use(id+"\x00"+a.claims.Jti, until, s.store.now()) {
			return Client{}, authFail("client_assertion was already used")
		}
	}
	return c, nil
}
