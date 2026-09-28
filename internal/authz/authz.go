// Package authz defines Kenfold's access levels. API keys have full access;
// OAuth grants have the scopes the owner approved on the consent page.
package authz

import (
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Scopes. memory:write implies memory:read.
const (
	ScopeRead  = "memory:read"
	ScopeWrite = "memory:write"
)

// Supported lists the scopes in order of increasing access.
var Supported = []string{ScopeRead, ScopeWrite}

// ExtraReadOnly marks a TokenInfo whose holder may only read.
const ExtraReadOnly = "kenfold.read_only"

// CanWrite reports whether ti allows writes. Unauthenticated requests (a
// server running with KENFOLD_AUTH=none, or stdio) have full access.
func CanWrite(ti *auth.TokenInfo) bool {
	if ti == nil {
		return true
	}
	ro, _ := ti.Extra[ExtraReadOnly].(bool)
	return !ro
}

// Normalize parses a space-separated scope parameter into known scopes,
// adding memory:read when memory:write is present. Unknown scopes are
// dropped (RFC 6749 lets the server grant fewer scopes than requested).
func Normalize(scope string) []string {
	var out []string
	for _, s := range strings.Fields(scope) {
		if slices.Contains(Supported, s) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if slices.Contains(out, ScopeWrite) && !slices.Contains(out, ScopeRead) {
		out = append(out, ScopeRead)
	}
	slices.SortFunc(out, func(a, b string) int { return slices.Index(Supported, a) - slices.Index(Supported, b) })
	return out
}

// ReadOnly reports whether scopes grant no write access.
func ReadOnly(scopes []string) bool { return !slices.Contains(scopes, ScopeWrite) }
