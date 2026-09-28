package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// validRedirectURI reports whether uri may be registered as a redirect URI:
// an absolute https URL, or http on a loopback host (native apps), without
// a fragment or credentials (OAuth 2.1 section 2.3.1, RFC 8252 section 7.3).
// Custom schemes are not accepted: every client Kenfold targets (ChatGPT,
// claude.ai, MCP inspectors, CLIs) redirects to https or loopback.
func validRedirectURI(uri string) error {
	if uri == "" || len(uri) > 2000 {
		return errors.New("redirect_uri is empty or too long")
	}
	u, err := url.Parse(uri)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return fmt.Errorf("redirect_uri %q is not an absolute URL", uri)
	}
	if u.Fragment != "" || strings.Contains(uri, "#") {
		return fmt.Errorf("redirect_uri %q has a fragment", uri)
	}
	if u.User != nil {
		return fmt.Errorf("redirect_uri %q contains credentials", uri)
	}
	switch u.Scheme {
	case "https":
		if u.Hostname() == "" {
			return fmt.Errorf("redirect_uri %q has no host", uri)
		}
	case "http":
		if !loopbackHost(u.Hostname()) {
			return fmt.Errorf("redirect_uri %q: http is only allowed for loopback addresses", uri)
		}
	default:
		return fmt.Errorf("redirect_uri %q: scheme must be https (or http on a loopback address)", uri)
	}
	return nil
}

func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// redirectMatches reports whether requested equals a registered redirect URI.
// Comparison is exact (OAuth 2.1), except that the port of a loopback http
// URI may vary (RFC 8252 section 7.3: native apps bind an ephemeral port).
func redirectMatches(registered []string, requested string) bool {
	for _, r := range registered {
		if r == requested {
			return true
		}
	}
	req, err := url.Parse(requested)
	if err != nil || req.Scheme != "http" || !loopbackHost(req.Hostname()) {
		return false
	}
	for _, r := range registered {
		reg, err := url.Parse(r)
		if err != nil || reg.Scheme != "http" || !loopbackHost(reg.Hostname()) {
			continue
		}
		if reg.Hostname() == req.Hostname() && reg.Path == req.Path && reg.RawQuery == req.RawQuery {
			return true
		}
	}
	return false
}

// pkceChallengeRE matches an S256 code challenge (base64url of 32 bytes).
var pkceChallengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// verifierRE matches a code verifier (RFC 7636 section 4.1).
var verifierRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// pkceMatches reports whether verifier hashes to challenge (S256).
func pkceMatches(verifier, challenge string) bool {
	if !verifierRE.MatchString(verifier) || !pkceChallengeRE.MatchString(challenge) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// Token prefixes make leaked tokens recognizable (see internal/secrets).
const (
	prefixAccess  = "kfa_"
	prefixRefresh = "kfr_"
	prefixCode    = "kfc_"
	prefixClient  = "kfd_"
)

// newToken returns prefix + 32 random bytes (base64url) and its SHA-256.
func newToken(prefix string) (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	t := prefix + base64.RawURLEncoding.EncodeToString(b)
	return t, hashToken(t), nil
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

// randomString returns n random bytes, base64url-encoded (CSRF tokens).
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// canonicalResource normalizes a resource indicator for comparison: lowercase
// scheme and host, no trailing slash, no default port (RFC 8707, MCP
// "Canonical Server URI"). It returns "" for values that are not absolute
// http(s) URLs or that have a fragment.
func canonicalResource(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Fragment != "" || u.User != nil {
		return ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	u.RawQuery = ""
	return u.String()
}
