package secrets

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// Fixtures are assembled at runtime from a seeded generator, so the repository
// contains no string that looks like a real credential (and push-protection
// scanners have nothing to flag).
var rng = rand.New(rand.NewPCG(1, 2))

func randFrom(alphabet string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b)
}

const (
	alnum   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	upperNm = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b64url  = alnum + "-_"
)

// mixed returns a random value guaranteed to contain upper, lower and digits.
func mixed(n int) string { return "Aa9" + randFrom(alnum, n-3) }

func join(parts ...string) string { return strings.Join(parts, "") }

func TestDetectsSecrets(t *testing.T) {
	cases := map[string]struct{ text, rule, secret string }{}
	add := func(name, rule, prefix, secret, suffix string) {
		cases[name] = struct{ text, rule, secret string }{prefix + secret + suffix, rule, secret}
	}

	pem := join("-----BEGIN ", "RSA PRIVATE KEY-----\n", randFrom(alnum, 64), "\n", randFrom(alnum, 64), "\n-----END ", "RSA PRIVATE KEY-----")
	add("pem key", "private-key", "Key:\n", pem, "\nthanks")
	openssh := join("-----BEGIN ", "OPENSSH PRIVATE KEY-----\n", randFrom(alnum, 70))
	add("truncated openssh key", "private-key", "", openssh, "")
	add("aws key id", "aws-access-key", "aws key ", join("AKIA", randFrom("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 16)), " in prod")
	add("github classic", "github-token", "use ", join("gh", "p_", randFrom(alnum, 36)), " for CI")
	add("github fine-grained", "github-token", "", join("github", "_pat_", randFrom(alnum+"_", 82)), "")
	add("gitlab", "gitlab-token", "token=", join("glp", "at-", randFrom(b64url, 19), "-"), " ok")
	add("anthropic", "anthropic-key", "ANTHROPIC ", join("sk-", "ant-api03-", randFrom(b64url, 90)), "")
	add("openai project", "openai-key", "", join("sk-", "proj-", mixed(48)), "")
	add("openai legacy", "openai-key", "key ", join("sk-", mixed(48)), ".")
	add("slack bot", "slack-token", "", join("xo", "xb-", randFrom("0123456789", 12), "-", randFrom(alnum, 24)), "")
	add("slack webhook", "slack-webhook", "post to ", join("https://hooks.", "slack.com/services/T", randFrom(upperNm, 8), "/B", randFrom(upperNm, 8), "/", randFrom(alnum, 24)), "")
	add("stripe", "stripe-key", "", join("sk", "_live_", randFrom(alnum, 24)), "")
	add("google", "google-api-key", "maps key ", join("AI", "za", randFrom(b64url, 34), "-"), " is set")
	add("npm", "npm-token", "", join("np", "m_", randFrom(alnum, 36)), "")
	add("huggingface", "huggingface-token", "", join("h", "f_", randFrom(alnum, 34)), "")
	add("kenfold", "kenfold-key", "Bearer? no: ", join("kf", "_", randFrom(b64url, 42), "-"), " end")
	add("kenfold oauth access", "kenfold-oauth-token", "token ", join("kf", "a_", randFrom(b64url, 43)), "")
	add("kenfold oauth refresh", "kenfold-oauth-token", "refresh=", join("kf", "r_", randFrom(b64url, 43)), " ")
	jwt := join("ey", "J", randFrom(b64url, 20), ".ey", "J", randFrom(b64url, 30), ".", randFrom(b64url, 43))
	add("jwt", "jwt", "session ", jwt, "")
	add("postgres url", "url-credentials", "DATABASE_URL=postgres://app:", "Xk29fLq0Zr", "@db.internal:5432/app")
	add("redis url", "url-credentials", "redis://default:", "p4Ss-W0rd!x", "@cache:6379")
	add("bearer header", "authorization-header", `curl -H "Authorization: Bearer `, mixed(40), `" https://api`)
	add("env assignment", "secret-assignment", "export DB_PASSWORD=", mixed(20), "")
	add("json field", "secret-assignment", `{"client_secret": "`, mixed(32), `"}`)
	add("yaml field", "secret-assignment", "api_key: ", mixed(28), "\n")
	add("cli flag", "secret-assignment", "run --token ", mixed(30), " --verbose")
	add("korean prose", "secret-assignment", "비밀번호는 password: ", mixed(16), " 입니다")

	for name, c := range cases {
		fs := Scan(c.text)
		if len(fs) == 0 {
			t.Errorf("%s: not detected in %q", name, c.text)
			continue
		}
		f := fs[0]
		if f.Rule != c.rule {
			t.Errorf("%s: rule = %s, want %s", name, f.Rule, c.rule)
		}
		if got := c.text[f.Start:f.End]; got != c.secret {
			t.Errorf("%s: span = %q, want %q", name, got, c.secret)
		}
		red, _ := Redact(c.text)
		if strings.Contains(red, c.secret) || !strings.Contains(red, "[REDACTED:"+f.Rule+"]") {
			t.Errorf("%s: redacted = %q", name, red)
		}
	}
}

func TestIgnoresNonSecrets(t *testing.T) {
	for _, text := range []string{
		"We use pgx v5 as the PostgreSQL driver; do not add an ORM.",
		"Never store secrets or passwords in memory.",
		"The API key is stored in the KENFOLD_API_KEY environment variable.",
		"Set token = <your-token> before running.",
		"password: changeme",
		"DB_PASSWORD=${DB_PASSWORD}",
		"api_key: $OPENAI_API_KEY",
		"export GITHUB_TOKEN=$(gh auth token)",
		"client_secret: '{{ vault.client_secret }}'",
		"Use postgres://kenfold:kenfold@127.0.0.1:54329/kenfold for local dev.", // low-entropy local default
		"postgres://user:<password>@host/db",
		"password_reset_token_ttl = 3600",
		"The token_budget is 2000 tokens.",
		"Rotate the secret-manager credentials monthly.",
		"sk-learn is a Python library",
		"The function getSecretToken returns a string.",
		"access_key_id is read from ~/.aws/credentials",
		"Authorization: Bearer $TOKEN",
		"토큰은 절대 커밋하지 않는다.",
		"commit 1f3a9c2 fixed the tokenizer",
		"uuid 0199a0e1-fae4-703c-9169-61ede9d496fd",
		"sha256 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"password: aaaaaaaaaaaaaaaa",
	} {
		if fs := Scan(text); len(fs) != 0 {
			t.Errorf("false positive %s in %q (span %q)", fs[0].Rule, text, text[fs[0].Start:fs[0].End])
		}
	}
}

func TestRedactMultipleAndOrder(t *testing.T) {
	gh := join("gh", "p_", randFrom(alnum, 36))
	aws := join("AK", "IA", randFrom("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 16))
	text := "first " + aws + " then " + gh + " done"
	red, fs := Redact(text)
	if len(fs) != 2 || fs[0].Rule != "aws-access-key" || fs[1].Rule != "github-token" {
		t.Fatalf("findings = %+v", fs)
	}
	if red != "first [REDACTED:aws-access-key] then [REDACTED:github-token] done" {
		t.Errorf("redacted = %q", red)
	}
	if got := Labels(append(fs, fs[0])); len(got) != 2 {
		t.Errorf("labels = %v", got)
	}
	if same, fs := Redact("nothing here"); same != "nothing here" || fs != nil {
		t.Errorf("clean text changed: %q %v", same, fs)
	}
}

func TestNoOverlapsAndStableUnderRedaction(t *testing.T) {
	// A key inside a secret assignment is reported once, by the specific rule.
	gh := join("gh", "p_", randFrom(alnum, 36))
	fs := Scan("GITHUB_TOKEN=" + gh)
	if len(fs) != 1 || fs[0].Rule != "github-token" {
		t.Errorf("findings = %+v; want a single github-token", fs)
	}
	// Redacted output contains no further secrets.
	red, _ := Redact("export API_KEY=" + mixed(32) + " and " + gh)
	if fs := Scan(red); len(fs) != 0 {
		t.Errorf("redacted text still has %+v: %q", fs, red)
	}
}

func TestLooksRandom(t *testing.T) {
	for _, v := range []string{mixed(16), mixed(40), "p4Ss-W0rd!x9Q", "Xk29fLq0Zr7m"} {
		if !looksRandom(v) {
			t.Errorf("looksRandom(%q) = false", v)
		}
	}
	for _, v := range []string{"production", "localhost:5432", "0123456789ab", "/etc/secret/key", "hello world 123", "Aaaaaaaaaaaaaaa1"} {
		if looksRandom(v) {
			t.Errorf("looksRandom(%q) = true", v)
		}
	}
}

func FuzzScan(f *testing.F) {
	f.Add("export DB_PASSWORD=" + mixed(20))
	f.Add("-----BEGIN PRIVATE KEY-----")
	f.Add("https://u:p@h")
	f.Fuzz(func(t *testing.T, s string) {
		fs := Scan(s)
		prev := 0
		for _, x := range fs {
			if x.Start < prev || x.End <= x.Start || x.End > len(s) {
				t.Fatalf("bad finding %+v for len %d", x, len(s))
			}
			prev = x.End
		}
		red, _ := Redact(s)
		if len(fs) == 0 && red != s {
			t.Fatal("redaction changed clean text")
		}
	})
}
