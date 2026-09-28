package oauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

// testSigner signs client assertions like a private_key_jwt client.
type testSigner struct {
	kid string
	alg string
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
}

func newRSASigner(t *testing.T, kid, alg string) *testSigner {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &testSigner{kid: kid, alg: alg, rsa: k}
}

func newECSigner(t *testing.T, kid string) *testSigner {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testSigner{kid: kid, alg: "ES256", ec: k}
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwk returns the signer's public key as a JWK.
func (s *testSigner) jwk() map[string]any {
	if s.rsa != nil {
		return map[string]any{"kty": "RSA", "kid": s.kid, "use": "sig", "alg": s.alg,
			"n": enc(s.rsa.N.Bytes()), "e": enc(big.NewInt(int64(s.rsa.E)).Bytes())}
	}
	pub, _ := s.ec.PublicKey.Bytes() // uncompressed: 0x04 || X || Y
	return map[string]any{"kty": "EC", "kid": s.kid, "crv": "P-256", "x": enc(pub[1:33]), "y": enc(pub[33:65])}
}

func jwksOf(signers ...*testSigner) []byte {
	var keys []map[string]any
	for _, s := range signers {
		keys = append(keys, s.jwk())
	}
	b, _ := json.Marshal(map[string]any{"keys": keys})
	return b
}

// sign returns a compact JWS of claims.
func (s *testSigner) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]any{"alg": s.alg, "kid": s.kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	input := enc(h) + "." + enc(p)
	d := sha256.Sum256([]byte(input))
	var sig []byte
	var err error
	switch s.alg {
	case "RS256":
		sig, err = rsa.SignPKCS1v15(rand.Reader, s.rsa, crypto.SHA256, d[:])
	case "PS256":
		sig, err = rsa.SignPSS(rand.Reader, s.rsa, crypto.SHA256, d[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case "ES256":
		var r, sv *big.Int
		r, sv, err = ecdsa.Sign(rand.Reader, s.ec, d[:])
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		sv.FillBytes(sig[32:])
	}
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + enc(sig)
}

func claims(clientID, aud string, exp time.Time) map[string]any {
	return map[string]any{"iss": clientID, "sub": clientID, "aud": aud, "exp": exp.Unix(), "iat": time.Now().Unix(), "jti": randomString(12)}
}

func TestAssertionSignatures(t *testing.T) {
	rs := newRSASigner(t, "r1", "RS256")
	ps := newRSASigner(t, "p1", "PS256")
	es := newECSigner(t, "e1")
	other := newRSASigner(t, "r1", "RS256") // same kid, different key
	keys, err := parseJWKS(jwksOf(rs, ps, es))
	if err != nil || len(keys) != 3 {
		t.Fatalf("parseJWKS = %d keys, %v", len(keys), err)
	}
	c := claims("https://client.example/c.json", "https://k.example", time.Now().Add(time.Minute))
	for _, s := range []*testSigner{rs, ps, es} {
		a, err := parseAssertion(s.sign(t, c))
		if err != nil {
			t.Fatalf("%s: %v", s.alg, err)
		}
		if ok, _ := a.verify(keys); !ok {
			t.Errorf("%s: valid signature rejected", s.alg)
		}
	}
	a, _ := parseAssertion(other.sign(t, c))
	if ok, known := a.verify(keys); ok || !known {
		t.Errorf("signature by an unknown key: ok %v, kid known %v", ok, known)
	}
	// A tampered payload fails.
	good := rs.sign(t, c)
	parts := strings.Split(good, ".")
	c2 := claims("https://attacker.example/c.json", "https://k.example", time.Now().Add(time.Minute))
	p, _ := json.Marshal(c2)
	if a, err := parseAssertion(parts[0] + "." + enc(p) + "." + parts[2]); err != nil {
		t.Fatal(err)
	} else if ok, _ := a.verify(keys); ok {
		t.Error("tampered payload accepted")
	}
	// Algorithm confusion: the key's declared alg is enforced.
	rsAsPS := &testSigner{kid: "r1", alg: "PS256", rsa: rs.rsa}
	if a, _ := parseAssertion(rsAsPS.sign(t, c)); a != nil {
		if ok, _ := a.verify(keys); ok {
			t.Error("a key published for RS256 verified a PS256 signature")
		}
	}
	for name, raw := range map[string]string{
		"none alg":  enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{}`)) + ".",
		"HS256":     enc([]byte(`{"alg":"HS256"}`)) + "." + enc([]byte(`{}`)) + "." + enc([]byte("x")),
		"crit":      enc([]byte(`{"alg":"RS256","crit":["x"]}`)) + "." + enc([]byte(`{}`)) + "." + enc([]byte("x")),
		"two parts": "a.b",
		"huge":      strings.Repeat("a", maxAssertionBytes+1),
	} {
		if _, err := parseAssertion(raw); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestParseJWKS(t *testing.T) {
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	weak := &testSigner{kid: "weak", alg: "RS256", rsa: small}
	good := newRSASigner(t, "good", "RS256")
	encKey := good.jwk()
	encKey["use"] = "enc"
	encKey["kid"] = "enc"
	b, _ := json.Marshal(map[string]any{"keys": []any{weak.jwk(), encKey, good.jwk(), map[string]any{"kty": "oct", "k": "c2VjcmV0"}}})
	keys, err := parseJWKS(b)
	if err != nil || len(keys) != 1 || keys[0].kid != "good" {
		t.Errorf("keys = %+v, %v (1024-bit, encryption, and symmetric keys must be skipped)", keys, err)
	}
	if _, err := parseJWKS([]byte(`{"keys":[]}`)); err == nil {
		t.Error("empty key set accepted")
	}
	if _, err := parseJWKS([]byte(`nope`)); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestCheckClaims(t *testing.T) {
	now := time.Now()
	s := &Server{issuer: "https://k.example", store: &Store{now: func() time.Time { return now }}}
	ep := "https://k.example/oauth/token"
	f := func(v float64) *float64 { return &v }
	u := func(t time.Time) *float64 { return f(float64(t.Unix())) }
	ok := assertionClaims{Aud: audience{"https://k.example"}, Exp: u(now.Add(time.Minute))}
	if err := s.checkClaims(ok, ep); err != nil {
		t.Errorf("valid claims: %v", err)
	}
	for _, aud := range []string{"https://k.example/", "https://k.example/oauth/token"} {
		c := ok
		c.Aud = audience{"https://other.example", aud}
		if err := s.checkClaims(c, ep); err != nil {
			t.Errorf("aud %q: %v", aud, err)
		}
	}
	for name, c := range map[string]assertionClaims{
		"no exp":      {Aud: ok.Aud},
		"expired":     {Aud: ok.Aud, Exp: u(now.Add(-2 * time.Minute))},
		"too long":    {Aud: ok.Aud, Exp: u(now.Add(3 * time.Hour))},
		"not yet":     {Aud: ok.Aud, Exp: ok.Exp, Nbf: u(now.Add(5 * time.Minute))},
		"future iat":  {Aud: ok.Aud, Exp: ok.Exp, Iat: u(now.Add(5 * time.Minute))},
		"wrong aud":   {Aud: audience{"https://evil.example"}, Exp: ok.Exp},
		"missing aud": {Exp: ok.Exp},
	} {
		if err := s.checkClaims(c, ep); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var a audience
	if json.Unmarshal([]byte(`"x"`), &a) != nil || len(a) != 1 || json.Unmarshal([]byte(`["x","y"]`), &a) != nil || len(a) != 2 || json.Unmarshal([]byte(`3`), &a) == nil {
		t.Error("audience decoding")
	}
}

func TestReplayCache(t *testing.T) {
	var c replayCache
	now := time.Now()
	if !c.use("a", now.Add(time.Minute), now) {
		t.Fatal("first use rejected")
	}
	if c.use("a", now.Add(time.Minute), now) {
		t.Error("replay accepted")
	}
	if !c.use("a", now.Add(3*time.Minute), now.Add(2*time.Minute)) {
		t.Error("reuse after expiry rejected")
	}
}

func TestClientMetadataNegotiation(t *testing.T) {
	// ChatGPT's metadata document (shape as published at chatgpt.com/oauth/<id>/client.json).
	chatgpt := ClientMetadata{ClientID: "https://chatgpt.com/oauth/x/client.json", ClientName: "ChatGPT",
		RedirectURIs: []string{"https://chatgpt.com/connector/oauth/x"}, GrantTypes: []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "private_key_jwt", TokenEndpointAuthSigningAlg: "RS256",
		JWKSURI: "https://chatgpt.com/oauth/jwks.json"}
	if err := chatgpt.validate(kindCIMD); err != nil || chatgpt.authMethod() != authPrivateKeyJWT {
		t.Errorf("ChatGPT document: %v, %s", err, chatgpt.authMethod())
	}
	// Claude's document lists an extra grant Kenfold does not implement; it is dropped.
	claude := ClientMetadata{ClientID: "https://claude.ai/oauth/mcp-oauth-client-metadata", ClientName: "Claude",
		RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback"}, TokenEndpointAuthMethod: "none",
		GrantTypes: []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:jwt-bearer"}}
	if err := claude.validate(kindCIMD); err != nil || claude.authMethod() != authNone || len(claude.GrantTypes) != 2 {
		t.Errorf("Claude document: %v, %s, %v", err, claude.authMethod(), claude.GrantTypes)
	}
	// A list of methods picks private_key_jwt only when keys are published.
	list := ClientMetadata{RedirectURIs: []string{"https://a.example/cb"}, TokenEndpointAuthMethods: []string{"none", "private_key_jwt"}, JWKSURI: "https://a.example/jwks"}
	if list.authMethod() != authPrivateKeyJWT {
		t.Error("list with keys should use private_key_jwt")
	}
	list.JWKSURI = ""
	if list.authMethod() != authNone {
		t.Error("list without keys should fall back to none")
	}
	for name, m := range map[string]ClientMetadata{
		"jwt without keys": {RedirectURIs: []string{"https://a.example/cb"}, TokenEndpointAuthMethod: "private_key_jwt"},
		"http jwks":        {RedirectURIs: []string{"https://a.example/cb"}, TokenEndpointAuthMethod: "private_key_jwt", JWKSURI: "http://a.example/jwks"},
		"unsupported alg":  {RedirectURIs: []string{"https://a.example/cb"}, TokenEndpointAuthMethod: "private_key_jwt", JWKSURI: "https://a.example/j", TokenEndpointAuthSigningAlg: "HS256"},
		"client secret":    {RedirectURIs: []string{"https://a.example/cb"}, TokenEndpointAuthMethod: "client_secret_basic"},
		"no code grant":    {RedirectURIs: []string{"https://a.example/cb"}, GrantTypes: []string{"client_credentials"}},
		"no code response": {RedirectURIs: []string{"https://a.example/cb"}, ResponseTypes: []string{"token"}},
	} {
		if err := m.validate(kindCIMD); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Dynamic registration is for public clients only.
	dcr := ClientMetadata{RedirectURIs: []string{"https://a.example/cb"}, TokenEndpointAuthMethod: "private_key_jwt", JWKSURI: "https://a.example/j"}
	if err := dcr.validate(kindDCR); err == nil {
		t.Error("DCR with private_key_jwt accepted")
	}
	dcr = ClientMetadata{RedirectURIs: []string{"https://a.example/cb"}, JWKSURI: "https://a.example/j"}
	if err := dcr.validate(kindDCR); err != nil || dcr.JWKSURI != "" || dcr.TokenEndpointAuthMethod != authNone {
		t.Errorf("DCR normalization: %+v, %v", dcr, err)
	}
}
