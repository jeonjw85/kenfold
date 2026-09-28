package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

type browser struct {
	t      *testing.T
	base   string
	client *http.Client
	csrf   string
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, client: &http.Client{Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) do(method, path string, form url.Values, hdr map[string]string) (int, http.Header, string) {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, b.base+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for k, v := range hdr {
		if strings.HasPrefix(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if m := csrfRE.FindStringSubmatch(string(raw)); m != nil {
		b.csrf = m[1]
	}
	return resp.StatusCode, resp.Header, string(raw)
}

func (b *browser) get(path string) (int, string) {
	b.t.Helper()
	st, _, body := b.do(http.MethodGet, path, nil, nil)
	return st, body
}

// post submits a dashboard form with the page's CSRF token.
func (b *browser) post(path string, form url.Values) (int, http.Header) {
	b.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if _, ok := form["csrf"]; !ok {
		form.Set("csrf", b.csrf)
	}
	st, h, _ := b.do(http.MethodPost, path, form, nil)
	return st, h
}

// TestDashboardIntegration drives the dashboard like a browser. It TRUNCATES
// memory, api_key, and the oauth tables.
func TestDashboardIntegration(t *testing.T) {
	dbURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(func(k string) string { return map[string]string{"KENFOLD_DATABASE_URL": dbURL}[k] })
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newRuntime(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := rt.pool.Exec(ctx, `TRUNCATE memory, api_key, oauth_client, oauth_owner CASCADE`); err != nil {
		t.Fatal(err)
	}
	const password = "a dashboard owner password"
	if err := rt.oauthDB.SetOwnerPassword(ctx, password); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpHandler(cfg, rt, slog.New(slog.DiscardHandler)))
	defer srv.Close()

	const scope = "project:github.com/acme/dash"
	mk := func(p store.CreateParams) store.Memory {
		t.Helper()
		if p.SourceAgent == "" {
			p.SourceAgent = "codex"
		}
		if p.Trust == "" {
			p.Trust = memory.TrustAgent
		}
		if p.Status == "" {
			p.Status = memory.StatusActive
		}
		p.Confidence = 0.9
		m, err := rt.store.Create(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	oldRule := mk(store.CreateParams{Type: memory.TypeProject, Scope: scope, Content: "Use npm for installing packages."})
	extracted := mk(store.CreateParams{Type: memory.TypeProject, Scope: scope, Content: "Use pnpm, not npm, for installing packages.",
		SourceAgent: "kenfold-extractor", Status: memory.StatusProposed,
		Attrs: map[string]any{"evidence": "앞으로 npm 말고 pnpm만 써줘", "session_agent": "claude-code", "extractor_model": "kenfold-extract", "similar_to": []any{oldRule.ID}}})
	pref := mk(store.CreateParams{Type: memory.TypePreference, Scope: "user", Content: "Keep answers short.", Status: memory.StatusProposed})
	xss := mk(store.CreateParams{Type: memory.TypeSemantic, Scope: scope, Content: `Payload <script>alert("x")</script> & <img src=x onerror=alert(1)>`})
	stale := mk(store.CreateParams{Type: memory.TypeCodebase, Scope: scope, Content: "Tokens are checked by `CheckToken` in auth/token.go.",
		Refs: []store.RefTarget{{Path: "auth/token.go", Symbol: "CheckToken"}}})
	if _, err := rt.store.ApplyRefChecks(ctx, scope, strings.Repeat("a", 40), []store.RefCheck{{Path: "auth/token.go", Symbol: "CheckToken", Found: true, Hash: "sym:1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.store.ApplyRefChecks(ctx, scope, strings.Repeat("b", 40), []store.RefCheck{{Path: "auth/token.go", Symbol: "CheckToken", AnchorCommit: strings.Repeat("a", 40), Found: false}}); err != nil {
		t.Fatal(err)
	}
	_, key, err := rt.keys.Create(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	var grantID string
	if _, err := rt.pool.Exec(ctx, `INSERT INTO oauth_client (client_id, kind, client_name, redirect_uris) VALUES ('https://chatgpt.com/oauth/x/client.json', 'cimd', 'ChatGPT', '{https://chatgpt.com/cb}')`); err != nil {
		t.Fatal(err)
	}
	if err := rt.pool.QueryRow(ctx, `INSERT INTO oauth_grant (client_id, agent, scopes, resource) VALUES ('https://chatgpt.com/oauth/x/client.json', 'chatgpt', '{memory:read}', 'r') RETURNING id::text`).Scan(&grantID); err != nil {
		t.Fatal(err)
	}

	b := newBrowser(t, srv.URL)
	t.Run("login required, and only on loopback hosts", func(t *testing.T) {
		st, h, _ := b.do(http.MethodGet, "/dashboard/", nil, nil)
		if st != http.StatusSeeOther || h.Get("Location") != "/dashboard/login?next=%2Fdashboard%2F" {
			t.Errorf("anonymous: %d %q", st, h.Get("Location"))
		}
		if st, _, _ := b.do(http.MethodGet, "/dashboard/login", nil, map[string]string{"Host": "evil.example"}); st != http.StatusForbidden {
			t.Errorf("foreign host: %d", st)
		}
		if st, _, _ := b.do(http.MethodGet, "/dashboard", nil, nil); st != http.StatusMovedPermanently {
			t.Errorf("/dashboard without slash: %d", st)
		}
		st, h, body := b.do(http.MethodGet, "/dashboard/login", nil, nil)
		if st != 200 || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") || h.Get("X-Frame-Options") != "DENY" || strings.Contains(body, "<script") {
			t.Errorf("login page: %d, CSP %q", st, h.Get("Content-Security-Policy"))
		}
		if st, h, body := b.do(http.MethodPost, "/dashboard/login", url.Values{"password": {"wrong password!"}}, nil); st != http.StatusUnauthorized || h.Get("Set-Cookie") != "" || !strings.Contains(body, "Wrong password") {
			t.Errorf("wrong password: %d", st)
		}
		st, h, _ = b.do(http.MethodPost, "/dashboard/login", url.Values{"password": {password}, "next": {"/dashboard/review"}}, nil)
		cookie := h.Get("Set-Cookie")
		if st != http.StatusSeeOther || h.Get("Location") != "/dashboard/review" || !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Strict") || !strings.Contains(cookie, "Path=/dashboard/") {
			t.Fatalf("login: %d %q %q", st, h.Get("Location"), cookie)
		}
		// Open redirects are not possible through next.
		st, h, _ = newBrowser(t, srv.URL).do(http.MethodPost, "/dashboard/login", url.Values{"password": {password}, "next": {"//evil.example/x"}}, nil)
		if h.Get("Location") != "/dashboard/" {
			t.Errorf("next=//evil.example: %q", h.Get("Location"))
		}
	})

	t.Run("overview and review", func(t *testing.T) {
		st, body := b.get("/dashboard/")
		if st != 200 || !strings.Contains(body, `<span class="big">2</span> awaiting review`) || !strings.Contains(body, `<span class="big">1</span> may be outdated`) {
			t.Errorf("overview: %d\n%s", st, body)
		}
		st, body = b.get("/dashboard/review")
		for _, want := range []string{"Use pnpm, not npm, for installing packages.", "앞으로 npm 말고 pnpm만 써줘", "extracted by kenfold-extract from a claude-code session",
			"Use npm for installing packages.", "Approve, replacing this one", "Keep answers short."} {
			if !strings.Contains(body, want) {
				t.Errorf("review page lacks %q", want)
			}
		}
		if strings.Index(body, "Use pnpm") > strings.Index(body, "Keep answers short.") {
			t.Error("review is not oldest first")
		}
	})

	t.Run("actions need the CSRF token and a same-origin request", func(t *testing.T) {
		if st, _ := b.post("/dashboard/review/"+pref.ID+"/approve", url.Values{"csrf": {"forged"}}); st != http.StatusForbidden {
			t.Errorf("forged token: %d", st)
		}
		st, _, _ := b.do(http.MethodPost, "/dashboard/review/"+pref.ID+"/approve", url.Values{"csrf": {b.csrf}}, map[string]string{"Sec-Fetch-Site": "cross-site"})
		if st != http.StatusForbidden {
			t.Errorf("cross-site post: %d", st)
		}
		m, _ := rt.store.Get(ctx, pref.ID)
		if m.Status != memory.StatusProposed {
			t.Fatalf("a refused action changed the memory: %s", m.Status)
		}
	})

	t.Run("approve, replace, reject", func(t *testing.T) {
		st, h := b.post("/dashboard/review/"+extracted.ID+"/replace", url.Values{"target": {oldRule.ID}, "back": {"/dashboard/review"}})
		if st != http.StatusSeeOther || h.Get("Location") != "/dashboard/review" {
			t.Fatalf("replace: %d %q", st, h.Get("Location"))
		}
		if m, _ := rt.store.Get(ctx, extracted.ID); m.Status != memory.StatusActive || m.Trust != memory.TrustUser || m.Supersedes == nil || *m.Supersedes != oldRule.ID {
			t.Errorf("approved replacement = %+v", m)
		}
		if m, _ := rt.store.Get(ctx, oldRule.ID); m.Status != memory.StatusSuperseded {
			t.Errorf("replaced memory status = %s", m.Status)
		}
		if _, body := b.get("/dashboard/review"); !strings.Contains(body, "Approved; the memory it replaces is kept as history.") {
			t.Error("no confirmation after replace")
		}
		if st, _ := b.post("/dashboard/review/"+pref.ID+"/reject", url.Values{"reason": {"not my preference"}}); st != http.StatusSeeOther {
			t.Errorf("reject: %d", st)
		}
		if m, _ := rt.store.Get(ctx, pref.ID); m.Status != memory.StatusDeleted {
			t.Errorf("rejected status = %s", m.Status)
		}
		// Deciding twice is reported, not an error page.
		if st, _ := b.post("/dashboard/review/"+pref.ID+"/approve", nil); st != http.StatusSeeOther {
			t.Errorf("second decision: %d", st)
		}
		if _, body := b.get("/dashboard/review"); !strings.Contains(body, "already decided") || !strings.Contains(body, "Nothing to review.") {
			t.Error("second decision not reported, or queue not empty")
		}
	})

	t.Run("memories: escaping, search, filters, detail, forget", func(t *testing.T) {
		st, body := b.get("/dashboard/memories")
		if st != 200 || strings.Contains(body, `<script>alert("x")`) || strings.Contains(body, "<img src=x") || !strings.Contains(body, "&lt;script&gt;") {
			t.Errorf("memory content is not escaped: %d", st)
		}
		if _, body := b.get("/dashboard/memories?q=pnpm+packages"); !strings.Contains(body, "Use pnpm, not npm") || !strings.Contains(body, "Ranked as agents see it") {
			t.Error("search did not find the approved memory")
		}
		if _, body := b.get("/dashboard/memories?stale=1"); !strings.Contains(body, "CheckToken") || !strings.Contains(body, "code changed") || strings.Contains(body, "Keep answers") {
			t.Error("stale filter")
		}
		if _, body := b.get("/dashboard/memories?status=superseded"); !strings.Contains(body, "Use npm for installing packages.") {
			t.Error("status filter")
		}
		_, body = b.get("/dashboard/memories/" + extracted.ID)
		for _, want := range []string{"Versions", "Use npm for installing packages.", "this version", "앞으로 npm 말고 pnpm만 써줘"} {
			if !strings.Contains(body, want) {
				t.Errorf("detail lacks %q", want)
			}
		}
		if _, body := b.get("/dashboard/memories/" + stale.ID); !strings.Contains(body, "missing") || !strings.Contains(body, "auth/token.go") {
			t.Error("detail lacks code references")
		}
		if st, _ := b.get("/dashboard/memories/not-an-id"); st != http.StatusNotFound {
			t.Errorf("bad id: %d", st)
		}
		if st, _ := b.post("/dashboard/memories/"+xss.ID+"/forget", url.Values{"reason": {"test payload"}}); st != http.StatusSeeOther {
			t.Errorf("forget: %d", st)
		}
		if m, _ := rt.store.Get(ctx, xss.ID); m.Status != memory.StatusDeleted {
			t.Errorf("forgotten status = %s", m.Status)
		}
	})

	t.Run("clients: revoke a key and a grant", func(t *testing.T) {
		_, body := b.get("/dashboard/clients")
		if !strings.Contains(body, "ChatGPT") || !strings.Contains(body, key.Prefix) {
			t.Fatalf("clients page lacks the client or key")
		}
		if st, _ := b.post("/dashboard/clients/keys/"+key.ID+"/revoke", nil); st != http.StatusSeeOther {
			t.Errorf("revoke key: %d", st)
		}
		if st, _ := b.post("/dashboard/clients/grants/"+grantID+"/revoke", nil); st != http.StatusSeeOther {
			t.Errorf("revoke grant: %d", st)
		}
		keys, _ := rt.keys.List(ctx, false)
		grants, _ := rt.oauthDB.Grants(ctx, false)
		if len(keys) != 0 || len(grants) != 0 {
			t.Errorf("after revocation: %d keys, %d grants", len(keys), len(grants))
		}
	})

	t.Run("static assets", func(t *testing.T) {
		st, h, body := b.do(http.MethodGet, "/dashboard/static/app.css", nil, nil)
		if st != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/css") || !strings.Contains(body, ":focus-visible") {
			t.Errorf("css: %d %q", st, h.Get("Content-Type"))
		}
	})

	t.Run("a new owner password ends sessions; logout ends this one", func(t *testing.T) {
		other := newBrowser(t, srv.URL)
		other.do(http.MethodPost, "/dashboard/login", url.Values{"password": {password}}, nil)
		if st, _ := other.get("/dashboard/"); st != 200 {
			t.Fatalf("second session: %d", st)
		}
		if st, _ := other.post("/dashboard/logout", nil); st != http.StatusSeeOther {
			t.Errorf("logout: %d", st)
		}
		if st, _ := other.get("/dashboard/"); st != http.StatusSeeOther {
			t.Errorf("after logout: %d", st)
		}
		if err := rt.oauthDB.SetOwnerPassword(ctx, password+" v2"); err != nil {
			t.Fatal(err)
		}
		if st, _ := b.get("/dashboard/"); st != http.StatusSeeOther {
			t.Errorf("after a password change: %d", st)
		}
	})
}
