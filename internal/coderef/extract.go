// Package coderef finds the code a memory talks about and checks whether that
// code has changed since.
//
// The server side (Extract) reads memory text and lists the files and symbols
// it mentions. The client side (Repo.Check) runs where the repository is (the
// session hook, `kenfold refs sync`): it hashes each file or symbol definition
// at HEAD so the server can tell whether the code a memory describes still
// exists and is unchanged. Symbol definitions are located with tree-sitter.
package coderef

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kenfold/kenfold/internal/memory"
)

// Ref is a file and/or symbol mentioned by a memory. Path is repository
// relative ("" when a symbol is mentioned without a file); Symbol is an
// identifier, optionally qualified ("Server.Dedupe"), or "" for a whole file.
type Ref struct {
	Path   string `json:"path"`
	Symbol string `json:"symbol"`
}

const (
	maxPaths      = 6
	maxSymbols    = 6
	maxPathRunes  = 300
	maxSymbolLen  = 100
	minSymbolLen  = 3
	maxQualifiers = 2
)

// codeExt lists extensions of files worth anchoring.
var codeExt = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`go py pyi ts tsx mts cts js jsx mjs cjs rs java kt kts swift rb php cs fs c h cc cpp cxx hpp hh m mm
		scala sql sh bash zsh fish ps1 yaml yml toml json jsonc proto graphql gql css scss sass less html vue svelte astro md mdx
		tf hcl ex exs erl dart lua zig nix ml mli clj cljs r jl pl pm gradle xml ini cfg conf lock`) {
		codeExt[e] = true
	}
}

// dataExt are extensions in codeExt whose files define no symbols.
var dataExt = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`yaml yml toml json jsonc md mdx lock ini cfg conf xml html css scss sass less`) {
		dataExt[e] = true
	}
}

// codeFile reports whether path is source code that may define symbols.
func codeFile(path string) bool {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return false
	}
	ext := strings.ToLower(path[i+1:])
	return codeExt[ext] && !dataExt[ext]
}

// exactFiles are well-known files without an extension.
var exactFiles = map[string]bool{"Makefile": true, "Dockerfile": true, "Justfile": true, "Gemfile": true, "Rakefile": true, "Procfile": true}

// notFiles are "Name.js" product names that look like files.
var notFiles = map[string]bool{
	"next.js": true, "node.js": true, "vue.js": true, "nuxt.js": true, "express.js": true, "three.js": true, "chart.js": true,
	"d3.js": true, "moment.js": true, "ember.js": true, "backbone.js": true, "react.js": true, "socket.io": true, "alpine.js": true,
	"solid.js": true, "p5.js": true, "anime.js": true, "day.js": true, "pixi.js": true, "babylon.js": true,
}

var (
	lineSuffixRE = regexp.MustCompile(`(:\d+(:\d+)?|#L\d+(-L?\d+)?)$`)
	domainRE     = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	segmentRE    = regexp.MustCompile(`^[A-Za-z0-9_.@+\-\[\]]+$`)
	backtickRE   = regexp.MustCompile("`([^`\n]{1,120})`")
	// identifier, optionally qualified with . :: or #, optionally followed by ().
	symbolRE = regexp.MustCompile(`^[A-Za-z_$][\w$]*((\.|::|#)[A-Za-z_$][\w$]*){0,2}(\(\))?$`)
	callRE   = regexp.MustCompile(`(?:^|[^\w.$])([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*){0,2})\(\)`)
)

// Extract returns the files and symbols text mentions, in order of first
// mention. Symbols in backticks or written as calls ("Name()") always count;
// other identifiers that look like code (CamelCase with an inner capital,
// snake_case) only count when the text mentions exactly one file, and are then
// looked up in that file. When a symbol can be tied to a single mentioned
// file it is returned with that path; otherwise the client searches the
// repository for its definition. Every mentioned file is also returned on its
// own, so a memory whose symbols cannot be found still tracks its file.
func Extract(text string) []Ref {
	paths := findPaths(text)
	strong := strongSymbols(text, paths)
	var weak []string
	if len(paths) == 1 {
		weak = weakSymbols(text, paths)
	}

	var out []Ref
	seen := map[Ref]bool{}
	add := func(r Ref) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, p := range paths {
		add(Ref{Path: p})
	}
	symbols := 0
	for _, s := range append(strong, weak...) {
		if symbols == maxSymbols {
			break
		}
		r := Ref{Symbol: s}
		if len(paths) == 1 {
			r.Path = paths[0]
		}
		if !seen[r] {
			symbols++
		}
		add(r)
	}
	return out
}

// findPaths returns repository-relative file paths mentioned in text.
func findPaths(text string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(text, notPathRune) {
		p, ok := cleanPath(tok)
		if ok && !slices.Contains(out, p) {
			out = append(out, p)
			if len(out) == maxPaths {
				break
			}
		}
	}
	return out
}

func notPathRune(r rune) bool {
	if r < utf8.RuneSelf {
		return !(r == '/' || r == '.' || r == '_' || r == '-' || r == '@' || r == '+' || r == ':' || r == '#' ||
			r == '[' || r == ']' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z'))
	}
	return true // non-ASCII (e.g. a Korean particle right after a path) ends the token
}

// cleanPath normalizes a token to a repository-relative path, or reports false.
func cleanPath(tok string) (string, bool) {
	if strings.Contains(tok, "://") {
		return "", false
	}
	tok = strings.TrimRight(tok, ".,:;!?")
	tok = lineSuffixRE.ReplaceAllString(tok, "")
	tok = strings.TrimRight(tok, ".,:;!?")
	tok = strings.TrimPrefix(tok, "./")
	if tok == "" || strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~") || strings.HasPrefix(tok, "#") ||
		strings.ContainsAny(tok, ":#") || utf8.RuneCountInString(tok) > maxPathRunes {
		return "", false
	}
	segs := strings.Split(tok, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || !segmentRE.MatchString(s) {
			return "", false
		}
	}
	// "github.com/org/repo/..." is a URL or import path, not a repository path.
	if len(segs) > 1 && domainRE.MatchString(segs[0]) {
		return "", false
	}
	base := segs[len(segs)-1]
	if exactFiles[base] {
		return tok, true
	}
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 && !(dot == 0 && strings.Count(base, ".") > 1) {
		return "", false // no extension (or only a leading dot, e.g. ".env")
	}
	if !codeExt[base[dot+1:]] {
		return "", false
	}
	if len(segs) == 1 && notFiles[strings.ToLower(base)] {
		return "", false
	}
	return tok, true
}

// strongSymbols returns identifiers in backticks and "name()" calls.
func strongSymbols(text string, paths []string) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSuffix(strings.TrimSpace(s), "()")
		if len(s) < minSymbolLen || len(s) > maxSymbolLen || !symbolRE.MatchString(s) || slices.Contains(out, s) || isPathLike(s, paths) || stopword(s) {
			return
		}
		out = append(out, s)
	}
	for _, m := range backtickRE.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range callRE.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	return out
}

// weakSymbols returns unmarked identifiers that look like code.
func weakSymbols(text string, paths []string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r == '_' || r == '.' || r == '$' || unicode.IsDigit(r) || (r < utf8.RuneSelf && unicode.IsLetter(r)))
	}) {
		tok = strings.Trim(tok, ".")
		if len(tok) < minSymbolLen || len(tok) > maxSymbolLen || !symbolRE.MatchString(tok) || isPathLike(tok, paths) || slices.Contains(out, tok) || stopword(tok) {
			continue
		}
		if looksLikeCode(tok) {
			out = append(out, tok)
		}
	}
	return out
}

// looksLikeCode reports whether an unmarked identifier is probably code:
// CamelCase with a capital after a lowercase letter (RequireAPIKey, useFlag),
// snake_case (seed_everything), or qualified by a type (Server.Dedupe).
// ALL_CAPS names (usually environment variables) and plain words are not.
func looksLikeCode(s string) bool {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '#' })
	if len(parts) > 1 {
		if first := []rune(parts[0])[0]; unicode.IsUpper(first) && len(parts) == 2 {
			return true
		}
	}
	return slices.ContainsFunc(parts, func(p string) bool { return camel(p) || snake(p) })
}

// camel reports a capital letter following a lowercase one (useFlag, RequireAPIKey).
func camel(s string) bool {
	lower := false
	for _, r := range s {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r) && lower:
			return true
		}
	}
	return false
}

// snake reports an inner underscore in a name with lowercase letters (seed_everything).
func snake(s string) bool {
	t := strings.Trim(s, "_")
	return strings.Contains(t, "_") && strings.IndexFunc(t, unicode.IsLower) >= 0
}

// isPathLike reports whether s is (part of) a mentioned path or a file name.
func isPathLike(s string, paths []string) bool {
	for _, p := range paths {
		if strings.Contains(p, s) {
			return true
		}
	}
	if i := strings.LastIndexByte(s, '.'); i > 0 && codeExt[s[i+1:]] {
		return true
	}
	return false
}

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`true false null nil none None True False undefined this self super main init new
		string int bool float byte error any void async await return import export default const var let func function def
		class struct interface type enum yield ok err ctx req res GitHub GitLab JavaScript TypeScript PostgreSQL MySQL
		MongoDB GraphQL OpenAI OAuth iPhone iPad macOS iOS YouTube LinkedIn PayPal WordPress DevOps NoSQL McDonald`) {
		stopwords[w] = true
	}
}

func stopword(s string) bool { return stopwords[s] }

// For returns the references to record for a memory: those Extract finds,
// for project, codebase, and semantic memories in a project scope. Other
// memories (session summaries, preferences, notes, user-wide facts) are not
// tied to a repository's code.
func For(typ memory.Type, scope, content string) []Ref {
	if !strings.HasPrefix(scope, "project:") && !strings.HasPrefix(scope, "repo:") {
		return nil
	}
	switch typ {
	case memory.TypeProject, memory.TypeCodebase, memory.TypeSemantic:
		return Extract(content)
	}
	return nil
}
