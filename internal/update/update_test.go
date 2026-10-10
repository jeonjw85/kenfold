package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckReportsNewerRelease(t *testing.T) {
	var sawUA bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUA = r.Header.Get("User-Agent") != ""
		io.WriteString(w, `{"tag_name":"v0.1.1","html_url":"https://evil.example/phish","draft":false,"prerelease":false}`)
	}))
	defer srv.Close()

	got, err := Check(context.Background(), srv.Client(), srv.URL, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !sawUA {
		t.Error("request had no User-Agent")
	}
	if got.Version != "v0.1.1" || got.URL != "https://github.com/jeonjw85/kenfold/releases/tag/v0.1.1" {
		t.Fatalf("release = %+v", got)
	}
}

func TestCheckIgnoresCurrentOrOlder(t *testing.T) {
	for _, latest := range []string{"v0.1.0", "v0.0.9"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"tag_name":"`+latest+`"}`)
		}))
		got, err := Check(context.Background(), srv.Client(), srv.URL, "v0.1.0")
		srv.Close()
		if err != nil || got.Version != "" {
			t.Fatalf("latest %s: got %+v, %v", latest, got, err)
		}
	}
}

func TestCheckComparesNumbers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"tag_name":"v0.1.10"}`)
	}))
	defer srv.Close()
	got, err := Check(context.Background(), srv.Client(), srv.URL, "v0.1.9")
	if err != nil || got.Version != "v0.1.10" {
		t.Fatalf("v0.1.10 vs v0.1.9: %+v, %v", got, err)
	}
}

func TestCheckSkipsNonReleaseWithoutNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("non-release build contacted the network")
	}))
	defer srv.Close()
	for _, current := range []string{"dev", "v0.1.0-3-gabcdef", "v0.1.0-dirty", "0.1.0", "v0.1", ""} {
		got, err := Check(context.Background(), srv.Client(), srv.URL, current)
		if err != nil || got.Version != "" {
			t.Fatalf("current %q: %+v, %v", current, got, err)
		}
	}
}

func TestCheckRefusesRedirect(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("followed a redirect")
		io.WriteString(w, `{"tag_name":"v9.9.9"}`)
	}))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusFound)
	}))
	defer srv.Close()

	got, err := Check(context.Background(), srv.Client(), srv.URL, "v0.1.0")
	if err == nil || got.Version != "" {
		t.Fatalf("redirect followed or accepted: %+v, %v", got, err)
	}
}

func TestCheckIgnoresUnusableRelease(t *testing.T) {
	for _, body := range []string{
		`{"tag_name":"v0.2.0","draft":true}`,
		`{"tag_name":"v0.2.0","prerelease":true}`,
		`{"tag_name":"v0.2.0-rc.1"}`,
		`{"tag_name":"not-a-version"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		got, err := Check(context.Background(), srv.Client(), srv.URL, "v0.1.0")
		srv.Close()
		if err != nil || got.Version != "" {
			t.Fatalf("body %s: %+v, %v", body, got, err)
		}
	}
}

func TestCheckHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := Check(context.Background(), srv.Client(), srv.URL, "v0.1.0"); err == nil {
		t.Fatal("HTTP 500 accepted")
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{`)
	}))
	defer srv2.Close()
	if _, err := Check(context.Background(), srv2.Client(), srv2.URL, "v0.1.0"); err == nil {
		t.Fatal("truncated JSON accepted")
	}
}

func TestCheckLimitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"`))
		io.Copy(w, io.LimitReader(neverEnding('a'), 2<<20))
	}))
	defer srv.Close()
	if _, err := Check(context.Background(), srv.Client(), srv.URL, "v0.1.0"); err == nil {
		t.Fatal("oversized body accepted")
	}
}

func TestCacheAnnouncesOnce(t *testing.T) {
	var c Cache
	if c.Store(Release{}) {
		t.Fatal("empty release announced")
	}
	if !c.Store(Release{Version: "v0.1.1", URL: "https://github.com/jeonjw85/kenfold/releases/tag/v0.1.1"}) {
		t.Fatal("first newer release not announced")
	}
	if c.Store(Release{Version: "v0.1.1", URL: "https://github.com/jeonjw85/kenfold/releases/tag/v0.1.1"}) {
		t.Fatal("same release announced twice")
	}
	got := c.Get()
	if got.Version != "v0.1.1" {
		t.Fatalf("cached = %+v", got)
	}
	if !c.Store(Release{Version: "v0.2.0", URL: "https://github.com/jeonjw85/kenfold/releases/tag/v0.2.0"}) {
		t.Fatal("a different newer release was not announced")
	}
}

type neverEnding byte

func (b neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}
