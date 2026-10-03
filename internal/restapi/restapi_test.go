package restapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kenfold/kenfold/internal/coderef"
)

// These requests are rejected before the store is touched, so a nil store is fine.
func TestRejectsBadInput(t *testing.T) {
	h := New(nil, nil)
	commit := strings.Repeat("a", 40)
	report := func(r coderef.CheckReport) string {
		b, _ := json.Marshal(r)
		return string(b)
	}
	ok := coderef.Result{Path: "a.go", Found: true, Hash: "blob:x"}
	for name, c := range map[string]struct {
		method, path, body string
		want               int
	}{
		"no project":                    {"GET", "/api/v1/refs", "", 400},
		"user scope":                    {"GET", "/api/v1/refs?project=+", "", 400},
		"local path":                    {"GET", "/api/v1/refs?project=/Users/me/repo", "", 400},
		"unknown path":                  {"GET", "/api/v1/other", "", 404},
		"wrong method":                  {"PUT", "/api/v1/refs", "", 405},
		"wrong method (check)":          {"GET", "/api/v1/refs/check", "", 405},
		"not json":                      {"POST", "/api/v1/refs/check", "nope", 400},
		"unknown field":                 {"POST", "/api/v1/refs/check", `{"project":"github.com/o/r","commit":"` + commit + `","extra":1}`, 400},
		"multiple reports":              {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Project: "github.com/o/r", Commit: commit}) + `{}`, 400},
		"trailing garbage":              {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Project: "github.com/o/r", Commit: commit}) + `broken`, 400},
		"oversized trailing whitespace": {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Project: "github.com/o/r", Commit: commit}) + strings.Repeat(" ", maxBody), 400},
		"bad commit":                    {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Project: "github.com/o/r", Commit: "HEAD", Results: []coderef.Result{ok}}), 400},
		"no project in report":          {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Commit: commit, Results: []coderef.Result{ok}}), 400},
		"escaping path":                 {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Project: "github.com/o/r", Commit: commit, Results: []coderef.Result{{Path: "../x.go", Found: true, Hash: "h"}}}), 400},
		"found without hash":            {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Project: "github.com/o/r", Commit: commit, Results: []coderef.Result{{Path: "a.go", Found: true}}}), 400},
		"missing with hash":             {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Project: "github.com/o/r", Commit: commit, Results: []coderef.Result{{Path: "a.go", Hash: "h"}}}), 400},
		"bad anchor":                    {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Project: "github.com/o/r", Commit: commit, Results: []coderef.Result{{Path: "a.go", AnchorCommit: "--all", Found: true, Hash: "h"}}}), 400},
		"bad resolved path":             {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Project: "github.com/o/r", Commit: commit, Results: []coderef.Result{{Symbol: "Run", Found: true, Hash: "h", ResolvedPath: "/etc/x.go"}}}), 400},
		"too many results":              {"POST", "/api/v1/refs/check", report(coderef.CheckReport{Project: "github.com/o/r", Commit: commit, Results: make([]coderef.Result, maxResults+1)}), 400},
		"oversized body":                {"POST", "/api/v1/refs/check", `{"project":"` + strings.Repeat("x", maxBody) + `"}`, 400},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, c.want, rec.Body.String())
		}
		var e struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error == "" {
			t.Errorf("%s: body %q is not a JSON error", name, rec.Body.String())
		}
		if c.want == http.StatusMethodNotAllowed && rec.Header().Get("Allow") == "" {
			t.Errorf("%s: no Allow header", name)
		}
	}
}
