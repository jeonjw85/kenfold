// Package secrets detects credentials in text before it is stored as memory.
//
// Memory is shared with every agent and injected into prompts, so a leaked
// credential would spread to all of them. Detection favors precision: rules
// match well-known token formats, key material, credentials in URLs, and
// assignments to secret-named variables whose values look random. Plain prose
// that merely mentions "password" or "token" is not flagged.
package secrets

import (
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Finding is one detected secret. It never carries the secret itself, so it is
// safe to log and to return to agents.
type Finding struct {
	Rule  string `json:"rule"`  // stable rule id, e.g. "github-token"
	Label string `json:"label"` // human-readable name, e.g. "GitHub token"
	Start int    `json:"start"` // byte offset of the secret value
	End   int    `json:"end"`
}

type rule struct {
	id, label string
	re        *regexp.Regexp
	group     int                     // capture group holding the secret value (0 = whole match)
	check     func(value string) bool // optional extra validation of the value
	// checkMatch, if set, validates using all capture groups (index 0 = whole match).
	checkMatch func(groups []string) bool
}

// Rules are ordered from most to least specific; overlapping matches keep the
// earliest rule.
var rules = []rule{
	{id: "private-key", label: "private key",
		re: regexp.MustCompile(`-----BEGIN[A-Z0-9 ]{0,40} PRIVATE KEY(?: BLOCK)?-----[\s\S]*?(?:-----END[A-Z0-9 ]{0,40} PRIVATE KEY(?: BLOCK)?-----|\z)`)},
	{id: "aws-access-key", label: "AWS access key ID",
		re: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`)},
	{id: "github-token", label: "GitHub token",
		re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{50,255})\b`)},
	{id: "gitlab-token", label: "GitLab token",
		re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{id: "anthropic-key", label: "Anthropic API key",
		re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{id: "openai-key", label: "OpenAI API key",
		re:    regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`),
		check: func(v string) bool { return !strings.HasPrefix(v, "sk-ant-") && hasDigit(v) }},
	{id: "slack-token", label: "Slack token",
		re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`)},
	{id: "slack-webhook", label: "Slack webhook URL",
		re: regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9_/]{20,}`)},
	{id: "stripe-key", label: "Stripe secret key",
		re: regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{20,}\b`)},
	{id: "google-api-key", label: "Google API key",
		re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{id: "npm-token", label: "npm token",
		re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{id: "huggingface-token", label: "Hugging Face token",
		re: regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`)},
	{id: "kenfold-key", label: "Kenfold API key",
		re: regexp.MustCompile(`\bkf_[A-Za-z0-9_-]{43}`)},
	{id: "kenfold-oauth-token", label: "Kenfold OAuth token",
		re: regexp.MustCompile(`\bkf[arc]_[A-Za-z0-9_-]{43}`)},
	{id: "jwt", label: "JSON Web Token",
		re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{id: "url-credentials", label: "password in URL",
		// scheme://user:password@host — user is group 1, the password group 2.
		re:    regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]{1,20}://([^\s:/@]{1,64}):([^\s@/]{3,128})@[^\s/]`),
		group: 2,
		checkMatch: func(m []string) bool {
			user, pass := m[1], m[2]
			// Local development defaults like postgres://app:app@localhost are
			// documentation, not leaks; real passwords are neither the user
			// name nor a short dictionary-style word.
			if isPlaceholder(pass) || strings.EqualFold(pass, user) {
				return false
			}
			return len(pass) >= 8 || hasDigit(pass) || strings.ContainsAny(pass, "!#$%&*+=?^~-_")
		}},
	{id: "authorization-header", label: "credential in Authorization header",
		re:    regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*["']?(?:bearer|basic|token)\s+([A-Za-z0-9._~+/=-]{16,})`),
		group: 1,
		check: func(v string) bool { return !isPlaceholder(v) && looksRandom(v) }},
	{id: "secret-assignment", label: "secret assigned to a variable",
		// NAME = "value" / NAME: value / --name value, where NAME ends with a
		// secret-like word. The value must look random, so prose and
		// placeholders like "changeme" or "<your-token>" pass.
		re:    regexp.MustCompile(`(?i)(?:^|[\s"'{(,;])(?:export\s+|--)?[a-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|client[_-]?secret|auth[_-]?key|credentials?)["']?\s*(?:[:=]|\s)\s*["']?([^\s"'<>{}()\[\],;]{8,200})`),
		group: 1,
		check: func(v string) bool { return !isPlaceholder(v) && looksRandom(v) }},
}

// Scan returns the secrets found in text, ordered by position, without
// overlaps.
func Scan(text string) []Finding {
	var out []Finding
	for _, r := range rules {
		for _, m := range r.re.FindAllStringSubmatchIndex(text, -1) {
			start, end := m[2*r.group], m[2*r.group+1]
			if start < 0 {
				continue
			}
			if r.check != nil && !r.check(text[start:end]) {
				continue
			}
			if r.checkMatch != nil && !r.checkMatch(groups(text, m)) {
				continue
			}
			if overlaps(out, start, end) {
				continue
			}
			out = append(out, Finding{Rule: r.id, Label: r.label, Start: start, End: end})
		}
	}
	slices.SortFunc(out, func(a, b Finding) int { return a.Start - b.Start })
	return out
}

// Contains reports whether text contains a secret.
func Contains(text string) bool { return len(Scan(text)) > 0 }

// Redact replaces every detected secret with "[REDACTED:<rule>]" and returns
// the result with the findings (offsets refer to the original text).
func Redact(text string) (string, []Finding) {
	fs := Scan(text)
	if len(fs) == 0 {
		return text, nil
	}
	var b strings.Builder
	last := 0
	for _, f := range fs {
		b.WriteString(text[last:f.Start])
		b.WriteString("[REDACTED:" + f.Rule + "]")
		last = f.End
	}
	b.WriteString(text[last:])
	return b.String(), fs
}

// Labels returns the distinct labels of fs, for messages like
// "content contains a GitHub token".
func Labels(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		if !slices.Contains(out, f.Label) {
			out = append(out, f.Label)
		}
	}
	return out
}

func overlaps(fs []Finding, start, end int) bool {
	for _, f := range fs {
		if start < f.End && f.Start < end {
			return true
		}
	}
	return false
}

// groups extracts submatch strings from an index slice; unmatched groups are "".
func groups(text string, m []int) []string {
	out := make([]string, len(m)/2)
	for i := range out {
		if m[2*i] >= 0 {
			out[i] = text[m[2*i]:m[2*i+1]]
		}
	}
	return out
}

// placeholderWords are values people write in docs and examples instead of a
// real secret.
var placeholderWords = []string{
	"changeme", "change-me", "change_me", "example", "placeholder", "redacted",
	"your", "xxxx", "****", "dummy", "sample", "secret", "password", "token",
	"fake", "test", "none", "null", "undefined", "todo", "insert", "replace",
	"process.env", "os.getenv",
}

func isPlaceholder(v string) bool {
	l := strings.ToLower(strings.Trim(v, `"'`))
	if strings.HasPrefix(l, "$") || strings.HasPrefix(l, "%") || strings.HasPrefix(l, "{{") || strings.HasPrefix(l, "<") {
		return true // $VAR, %VAR%, {{ template }}, <your-key>
	}
	for _, w := range placeholderWords {
		if strings.Contains(l, w) {
			return true
		}
	}
	// A run of one repeated character ("aaaaaaaa", "********").
	if r := []rune(l); len(r) > 0 && strings.Count(l, string(r[0])) == len(r) {
		return true
	}
	return false
}

// looksRandom reports whether v looks like generated key material rather than
// a word, identifier, path, host:port, or hex digest: long enough, containing
// letters plus digits or symbols, not just hex, and with high per-character
// entropy.
func looksRandom(v string) bool {
	if len(v) < 12 || strings.ContainsAny(v, " /\\:") {
		return false
	}
	if strings.Trim(strings.ToLower(v), "0123456789abcdef") == "" {
		return false // hex: commit SHAs, digests, ids
	}
	var upper, lower, digit, symbol bool
	for _, r := range v {
		switch {
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsLower(r):
			lower = true
		case unicode.IsDigit(r):
			digit = true
		case r != '.' && r != '_' && r != '-':
			symbol = true
		}
	}
	if !upper && !lower {
		return false
	}
	// Needs at least three character classes (e.g. upper+lower+digit), or
	// letters with a digit and a symbol.
	classes := 0
	for _, b := range []bool{upper, lower, digit, symbol} {
		if b {
			classes++
		}
	}
	if classes < 3 && !(digit && symbol) {
		return false
	}
	return entropy(v) >= 3.5
}

// entropy is the Shannon entropy of s in bits per character.
func entropy(s string) float64 {
	counts := map[rune]int{}
	n := 0
	for _, r := range s {
		counts[r]++
		n++
	}
	var h float64
	for _, c := range counts {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

func hasDigit(s string) bool { return strings.IndexFunc(s, unicode.IsDigit) >= 0 }
