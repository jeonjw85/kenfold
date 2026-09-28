package dashboard

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/apikey"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/oauth"
	"github.com/kenfold/kenfold/internal/store"
)

// Every page renders with representative data (no database needed), and
// memory content is escaped.
func TestPagesRender(t *testing.T) {
	pages, err := parsePages()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sess := "sess-1"
	m := store.Memory{ID: "01a0e2f4-5446-7fe9-bb7a-89d49ad44bfa", Type: memory.TypeProject, Scope: "project:github.com/o/r",
		Content: `<script>alert(1)</script> use pnpm`, SourceAgent: "kenfold-extractor", Status: memory.StatusProposed, Trust: memory.TrustAgent,
		Confidence: 0.9, CreatedAt: now, UpdatedAt: now, SourceSession: &sess,
		Attrs: map[string]any{"evidence": "npm 말고 pnpm", "session_agent": "codex", "extractor_model": "m", "similar_to": []any{"x"}}}
	other := m
	other.ID, other.Status, other.SourceAgent = "01a0e2f4-5446-7fe9-bb7a-89d49ad44bfb", memory.StatusActive, "claude-code"
	path := "a.go"
	commit := strings.Repeat("c", 40)
	data := map[string]any{
		"login.html":    map[string]any{"Next": Prefix, "NoPassword": true},
		"overview.html": overviewData{Proposed: 2, Stale: 1, ByType: []typeCount{{memory.TypeProject, 3}}, Scopes: []store.ScopeCount{{Scope: "user", Active: 1}}, HasConsol: true},
		"review.html":   reviewData{Items: []reviewItem{{Memory: m, Similar: []store.Memory{other}}}, Page: 2, More: true, Scopes: []store.ScopeCount{{Scope: m.Scope, Proposed: 1}}},
		"memories.html": memoriesData{Rows: []memoryRow{{Memory: other, Score: 0.5, Refs: []store.CodeRef{{Path: path, Symbol: "F", State: store.RefMissing}}}}, Searched: true, Types: memory.Types, Statuses: []memory.Status{memory.StatusActive}, Status: "active"},
		"memory.html": detailData{Memory: m, Refs: []store.CodeRef{{Path: path, State: store.RefCurrent, CheckedCommit: &commit}}, History: []store.Memory{other, m},
			Links: []store.Link{{Relation: "derived_from", Outgoing: true, Memory: other}}, Similar: []store.Memory{other}, Attrs: map[string]string{"evidence": "e"}},
		"clients.html": clientsData{Keys: []apikey.Key{{ID: "k", Agent: "codex", Prefix: "kf_abc", CreatedAt: now}},
			Grants: []oauth.Grant{{ID: "g", ClientID: "https://chatgpt.com/c", ClientName: "ChatGPT", Agent: "chatgpt", Scopes: []string{"memory:read"}, CreatedAt: now}}},
		"consolidation.html": []Proposal{{ID: "p1", Kind: "conflict", Scope: m.Scope, Keep: other.ID, Reason: "newer decision", Model: "m", Members: []store.Memory{m, other}, CreatedAt: now},
			{ID: "p2", Kind: "duplicate", Scope: m.Scope, Keep: m.ID, Members: []store.Memory{m, other}, CreatedAt: now},
			{ID: "p3", Kind: "digest", Scope: m.Scope, Content: "Digest of 2 sessions", Members: []store.Memory{m, other}, CreatedAt: now}},
		"message.html": nil,
	}
	for name, t2 := range pages {
		d, ok := data[name]
		if !ok {
			t.Errorf("no test data for %s", name)
			continue
		}
		var b bytes.Buffer
		if err := t2.ExecuteTemplate(&b, "layout", view{Title: "T", Data: d, LoggedIn: true, CSRF: "tok", Path: "/dashboard/x", Pending: 3, Consol: true}); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		out := b.String()
		if strings.Contains(out, "<script>alert") {
			t.Errorf("%s: content not escaped", name)
		}
		if strings.Contains(out, "<form method=\"post\"") && !strings.Contains(out, `name="csrf" value="tok"`) {
			t.Errorf("%s: a form lacks the CSRF token", name)
		}
	}
	// Consolidation enabled without proposals, and not enabled (no nav link).
	var b bytes.Buffer
	if err := pages["consolidation.html"].ExecuteTemplate(&b, "layout", view{Data: []Proposal(nil), LoggedIn: true, Consol: true}); err != nil ||
		!strings.Contains(b.String(), "No proposals") || !strings.Contains(b.String(), `href="/dashboard/consolidation"`) {
		t.Errorf("empty consolidation page: %v", err)
	}
	b.Reset()
	if err := pages["consolidation.html"].ExecuteTemplate(&b, "layout", view{LoggedIn: true}); err != nil ||
		!strings.Contains(b.String(), "not enabled") || strings.Contains(b.String(), `href="/dashboard/consolidation"`) {
		t.Errorf("consolidation page when not enabled: %v", err)
	}
}

func TestSafeBack(t *testing.T) {
	for in, want := range map[string]string{
		"/dashboard/review?page=2":        "/dashboard/review?page=2",
		"/dashboard/":                     "/dashboard/",
		"":                                Prefix,
		"https://evil.example/":           Prefix,
		"//evil.example/":                 Prefix,
		"/dashboard//evil":                Prefix,
		"/dashboard/../mcp":               Prefix,
		"/other":                          Prefix,
		"/dashboard/x\r\nSet-Cookie: a=b": Prefix,
		"/dashboard/\\evil":               Prefix,
	} {
		if got := safeBack(in); got != want {
			t.Errorf("safeBack(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllowedHost(t *testing.T) {
	local := &Server{}
	remote := &Server{d: Deps{Remote: true, PublicHost: "kenfold.example.com"}}
	for host, want := range map[string][2]bool{
		"localhost:7077":          {true, true},
		"127.0.0.1:7077":          {true, true},
		"[::1]:7077":              {true, true},
		"kenfold.example.com":     {false, true},
		"KENFOLD.example.com:443": {false, true},
		"evil.example":            {false, false},
		"localhost.evil.example":  {false, false},
		"127.0.0.1.nip.io":        {false, false},
	} {
		if got := local.allowedHost(host); got != want[0] {
			t.Errorf("local allowedHost(%q) = %v", host, got)
		}
		if got := remote.allowedHost(host); got != want[1] {
			t.Errorf("remote allowedHost(%q) = %v", host, got)
		}
	}
}

func TestSessions(t *testing.T) {
	var ss sessions
	ss.m = map[string]*session{}
	now := time.Now()
	s := ss.create("stamp", now)
	if got := ss.get(s.id, now.Add(time.Hour)); got == nil || got.csrf == "" || got.csrf == got.id {
		t.Fatalf("live session = %+v", got)
	}
	if ss.get(s.id, now.Add(time.Hour+sessionIdle+time.Minute)) != nil {
		t.Error("idle session survived")
	}
	s = ss.create("stamp", now)
	for i := 1; i <= 11; i++ { // keep it active past the absolute limit
		if ss.get(s.id, now.Add(time.Duration(i)*time.Hour)) == nil {
			t.Fatalf("session ended early at %dh", i)
		}
	}
	if ss.get(s.id, now.Add(sessionMax+time.Minute)) != nil {
		t.Error("session outlived the absolute limit")
	}
	for i := 0; i < maxSessions+5; i++ {
		ss.create("stamp", now)
	}
	if len(ss.m) > maxSessions {
		t.Errorf("%d sessions kept", len(ss.m))
	}
	ss.setFlash(s, "hi")
	if ss.takeFlash(s) != "hi" || ss.takeFlash(s) != "" {
		t.Error("flash is not one-shot")
	}
}

func TestAllowedRequest(t *testing.T) {
	local := &Server{}
	remote := &Server{d: Deps{Remote: true, PublicHost: "kenfold.example.com"}}
	req := func(host string, hdr ...string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "http://"+host+"/dashboard/", nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return r
	}
	if !local.allowed(req("127.0.0.1:7077")) {
		t.Error("local browser refused")
	}
	// A tunnel that rewrites Host to localhost still adds forwarding headers.
	for _, h := range forwardingHeaders {
		if local.allowed(req("localhost:7077", h, "203.0.113.9")) {
			t.Errorf("local mode accepted a request with %s", h)
		}
	}
	if !remote.allowed(req("kenfold.example.com", "X-Forwarded-For", "203.0.113.9")) {
		t.Error("remote mode refused the public host through its proxy")
	}
	if remote.allowed(req("other.example.com", "X-Forwarded-For", "203.0.113.9")) {
		t.Error("remote mode accepted another host")
	}
}
