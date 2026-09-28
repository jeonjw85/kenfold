package coderef

import (
	"reflect"
	"testing"

	"github.com/kenfold/kenfold/internal/memory"
)

func TestExtract(t *testing.T) {
	cases := []struct {
		text string
		want []Ref
	}{
		{"API authentication is handled by `RequireAPIKey` in internal/auth/middleware.go; it hashes the bearer token.",
			[]Ref{{Path: "internal/auth/middleware.go"}, {Path: "internal/auth/middleware.go", Symbol: "RequireAPIKey"}}},
		// Unmarked code-like identifiers count when one file is mentioned; Korean particles end the path.
		{"만료된 토큰은 internal/auth/jwt.go에서 ErrTokenExpired로 401을 반환합니다.",
			[]Ref{{Path: "internal/auth/jwt.go"}, {Path: "internal/auth/jwt.go", Symbol: "ErrTokenExpired"}}},
		// Two files: backticked symbols are looked up in the repository, unmarked ones are ignored.
		{"`dedupePayload` moved from internal/webhook/handler.go to internal/webhook/dedupe.go; RefundJob is unchanged.",
			[]Ref{{Path: "internal/webhook/handler.go"}, {Path: "internal/webhook/dedupe.go"}, {Symbol: "dedupePayload"}}},
		{"Call utils.seed_everything(42) at the start of every training script.", nil},
		{"Run `seed_everything()` first; see scripts/train.py:12 and src/app.tsx#L10-L20.",
			[]Ref{{Path: "scripts/train.py"}, {Path: "src/app.tsx"}, {Symbol: "seed_everything"}}},
		{"The Button is styled in src/components/Button.module.css (see .golangci.yml, README.md, Makefile).",
			[]Ref{{Path: "src/components/Button.module.css"}, {Path: ".golangci.yml"}, {Path: "README.md"}, {Path: "Makefile"}}},
		{"Server.Dedupe in store.go is called by Handler.ServeHTTP.",
			[]Ref{{Path: "store.go"}, {Path: "store.go", Symbol: "Server.Dedupe"}, {Path: "store.go", Symbol: "Handler.ServeHTTP"}}},
		// Not files: URLs, import paths, absolute paths, product names, versions, env files, domains, directories.
		{"Upgraded to Next.js 16 and Node.js 22; see https://github.com/acme/api/blob/main/x.go, github.com/acme/api/y.go, " +
			"/etc/app.conf, ~/notes.md, pgx v5.7, .env, api.acme.dev, internal/ratelimit, log/slog, Intl.NumberFormat.", nil},
		// ALL_CAPS and proper nouns are not symbols; GitHub is a stopword.
		{"The limit is RATE_LIMIT_RPM in internal/ratelimit/limit.go, deployed with GitHub Actions.",
			[]Ref{{Path: "internal/ratelimit/limit.go"}}},
		{"Use pnpm, not npm or yarn, in this repository.", nil},
	}
	for _, c := range cases {
		if got := Extract(c.text); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Extract(%q)\n got %v\nwant %v", c.text, got, c.want)
		}
	}
}

func TestExtractLimits(t *testing.T) {
	text := ""
	for _, p := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go"} {
		text += p + " "
	}
	for _, s := range []string{"`one1`", "`two2`", "`three`", "`four4`", "`five5`", "`six66`", "`seven`"} {
		text += s + " "
	}
	got := Extract(text)
	paths, symbols := 0, 0
	for _, r := range got {
		if r.Symbol == "" {
			paths++
		} else {
			symbols++
		}
	}
	if paths != maxPaths || symbols != maxSymbols {
		t.Errorf("paths %d symbols %d: %v", paths, symbols, got)
	}
}

func TestFor(t *testing.T) {
	const text = "Tokens are checked in internal/auth/jwt.go."
	if got := For(memory.TypeCodebase, "project:github.com/o/r", text); len(got) != 1 {
		t.Errorf("codebase in project = %v", got)
	}
	for _, c := range []struct {
		typ   memory.Type
		scope string
	}{{memory.TypeCodebase, "user"}, {memory.TypeEpisodic, "project:x/y"}, {memory.TypePreference, "project:x/y"}, {memory.TypeTemporary, "project:x/y"}} {
		if got := For(c.typ, c.scope, text); got != nil {
			t.Errorf("For(%s, %s) = %v", c.typ, c.scope, got)
		}
	}
}

func TestCodeFile(t *testing.T) {
	for p, want := range map[string]bool{"a/b.go": true, "x.PY": true, "k8s/deploy.yaml": false, "README.md": false, "Makefile": false, "notes": false, "db/q.sql": true} {
		if got := codeFile(p); got != want {
			t.Errorf("codeFile(%q) = %v", p, got)
		}
	}
}
