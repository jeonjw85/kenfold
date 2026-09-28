package coderef

import (
	"errors"
	"strings"
	"testing"
)

func TestDefinitions(t *testing.T) {
	goSrc := `package auth

// RequireAPIKey checks the bearer token.
func RequireAPIKey(next http.Handler) http.Handler {
	return next
}

type Server struct{ n int }

func (s *Server) Dedupe(id string) bool { return RequireAPIKey(nil) != nil }

type Other struct{}

func (o *Other) Dedupe(id string) bool { return false }

var ErrTokenExpired = errors.New("expired")

const Limit = 100
`
	tsSrc := `export function useFlag(name: string): boolean {
  return cache.get(name)
}
export const keys = { flags: ['flags'] }
export const RevenueChart = () => <Chart useFlag={useFlag} />
export class Api {
  fetchAll() { return useFlag('x') }
}
type Variant = 'primary' | 'ghost'
`
	pySrc := `import os

LIMIT = 3

def seed_everything(n):
    return n

class Model:
    def fit(self, x):
        return seed_everything(n=1)
`
	rsSrc := "pub fn parse_token(s: &str) -> bool { true }\nstruct Config { a: u8 }\nimpl Config { fn load() -> Self { todo!() } }\n"
	javaSrc := "class Billing {\n  int charge(int cents) { return helper.charge(cents); }\n}\n"
	cSrc := "static int parse_header(const char *s) {\n  return 0;\n}\nint main(void) { return parse_header(\"x\"); }\n"

	cases := []struct {
		path, src, symbol string
		want              []string // prefixes of the definitions, in order
	}{
		{"auth.go", goSrc, "RequireAPIKey", []string{"func RequireAPIKey("}},
		{"auth.go", goSrc, "Dedupe", []string{"func (s *Server) Dedupe", "func (o *Other) Dedupe"}},
		{"auth.go", goSrc, "Server.Dedupe", []string{"func (s *Server) Dedupe"}},
		{"auth.go", goSrc, "Server", []string{"Server struct"}},
		{"auth.go", goSrc, "ErrTokenExpired", []string{"ErrTokenExpired = errors.New"}},
		{"auth.go", goSrc, "Limit", []string{"Limit = 100"}},
		{"auth.go", goSrc, "Missing", nil},
		{"flags.tsx", tsSrc, "useFlag", []string{"function useFlag("}}, // not the JSX attribute or calls
		{"flags.tsx", tsSrc, "RevenueChart", []string{"RevenueChart = () =>"}},
		{"flags.tsx", tsSrc, "keys", []string{"keys = {"}},
		{"flags.tsx", tsSrc, "Api.fetchAll", []string{"fetchAll()"}},
		{"flags.tsx", tsSrc, "Variant", []string{"type Variant"}},
		{"train.py", pySrc, "seed_everything", []string{"def seed_everything"}}, // not the keyword argument n=
		{"train.py", pySrc, "Model.fit", []string{"def fit"}},
		{"train.py", pySrc, "LIMIT", []string{"LIMIT = 3"}},
		{"lib.rs", rsSrc, "parse_token", []string{"pub fn parse_token"}},
		{"lib.rs", rsSrc, "Config::load", []string{"fn load()"}},
		{"Billing.java", javaSrc, "charge", []string{"int charge("}}, // not helper.charge(...)
		{"parse.c", cSrc, "parse_header", []string{"static int parse_header"}},
	}
	for _, c := range cases {
		got, err := definitions(c.path, []byte(c.src), c.symbol)
		if err != nil {
			t.Errorf("%s %s: %v", c.path, c.symbol, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%s %s: got %d definitions %q, want %d", c.path, c.symbol, len(got), got, len(c.want))
			continue
		}
		for i := range got {
			if !strings.Contains(got[i], c.want[i]) {
				t.Errorf("%s %s [%d] = %q, want it to contain %q", c.path, c.symbol, i, got[i], c.want[i])
			}
		}
	}
	if _, err := definitions("notes.unknownext", []byte("x"), "x"); !errors.Is(err, errUnsupported) {
		t.Errorf("unknown language: %v", err)
	}
	if _, err := definitions("big.go", make([]byte, maxParseBytes+1), "x"); !errors.Is(err, errUnsupported) {
		t.Errorf("oversized file: %v", err)
	}
}

func TestSplitSymbol(t *testing.T) {
	for in, want := range map[string][2]string{
		"Dedupe": {"", "Dedupe"}, "Server.Dedupe": {"Server", "Dedupe"}, "pkg.Server.Dedupe": {"Server", "Dedupe"},
		"Config::load": {"Config", "load"}, "Api#fetch": {"Api", "fetch"}, "seed()": {"", "seed"},
	} {
		if q, n := splitSymbol(in); q != want[0] || n != want[1] {
			t.Errorf("splitSymbol(%q) = %q, %q", in, q, n)
		}
	}
}
