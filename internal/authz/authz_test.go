package authz

import (
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string][]string{
		"":                                 nil,
		"memory:read":                      {ScopeRead},
		"memory:write":                     {ScopeRead, ScopeWrite},
		"memory:write memory:read":         {ScopeRead, ScopeWrite},
		"offline_access memory:read admin": {ScopeRead},
		"  memory:read   memory:read ":     {ScopeRead},
	} {
		if got := Normalize(in); !slices.Equal(got, want) {
			t.Errorf("Normalize(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCanWrite(t *testing.T) {
	if !CanWrite(nil) {
		t.Error("unauthenticated requests have full access")
	}
	if !CanWrite(&auth.TokenInfo{}) {
		t.Error("API keys have full access")
	}
	if CanWrite(&auth.TokenInfo{Extra: map[string]any{ExtraReadOnly: true}}) {
		t.Error("read-only token can write")
	}
	if !ReadOnly([]string{ScopeRead}) || ReadOnly([]string{ScopeRead, ScopeWrite}) {
		t.Error("ReadOnly")
	}
}
