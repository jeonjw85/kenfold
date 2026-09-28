package rerank

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func server(t *testing.T, status int, body string, check func(r *http.Request, in request)) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in request
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		if check != nil {
			check(r, in)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	c, err := New(Config{BaseURL: ts.URL + "/v1/", Model: "bge-reranker-v2-m3", APIKey: "k" + "1"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNew(t *testing.T) {
	c, err := New(Config{BaseURL: "http://reranker:8080/v1", Model: "m"})
	if err != nil || c.Endpoint() != "http://reranker:8080/v1/rerank" || c.Model() != "m" {
		t.Fatalf("New = %v, %v", c, err)
	}
	for _, cfg := range []Config{{BaseURL: "reranker:8080", Model: "m"}, {BaseURL: "ftp://x", Model: "m"}, {BaseURL: "http://x/v1", Model: " "}} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) accepted", cfg)
		}
	}
}

func TestRerankProbabilities(t *testing.T) {
	long := strings.Repeat("가", MaxDocRunes+50)
	c := server(t, 200, `{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1},{"index":2,"relevance_score":0.5}]}`,
		func(r *http.Request, in request) {
			if r.URL.Path != "/v1/rerank" || r.Header.Get("Authorization") != "Bearer k1" {
				t.Errorf("path %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
			}
			if in.Model != "bge-reranker-v2-m3" || in.Query != "q" || len(in.Documents) != 3 || in.TopN != 3 {
				t.Errorf("request = %+v", in)
			}
			if utf8.RuneCountInString(in.Documents[2]) != MaxDocRunes || in.Documents[1] != " " {
				t.Errorf("documents not truncated/filled: %d runes, %q", utf8.RuneCountInString(in.Documents[2]), in.Documents[1])
			}
		})
	got, err := c.Rerank(context.Background(), "q", []string{"a", "  ", long})
	if err != nil {
		t.Fatal(err)
	}
	if want := []float64{0.1, 0.9, 0.5}; !equal(got, want) {
		t.Errorf("scores = %v, want %v", got, want)
	}
}

func TestRerankLogitsAndVoyage(t *testing.T) {
	c := server(t, 200, `{"results":[{"index":0,"relevance_score":-2.0},{"index":1,"relevance_score":3.5}]}`, nil)
	got, err := c.Rerank(context.Background(), "q", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !(got[1] > got[0]) || got[0] <= 0 || got[1] >= 1 || math.Abs(got[0]-1/(1+math.Exp(2))) > 1e-9 {
		t.Errorf("sigmoid scores = %v", got)
	}
	v := server(t, 200, `{"data":[{"index":1,"relevance_score":0.2},{"index":0,"relevance_score":0.7}]}`, nil)
	if got, err := v.Rerank(context.Background(), "q", []string{"a", "b"}); err != nil || !equal(got, []float64{0.7, 0.2}) {
		t.Errorf("voyage = %v, %v", got, err)
	}
}

func TestRerankErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"status":        {500, `{"error":"boom"}`},
		"not json":      {200, `nope`},
		"count":         {200, `{"results":[{"index":0,"relevance_score":0.1}]}`},
		"duplicate":     {200, `{"results":[{"index":0,"relevance_score":0.1},{"index":0,"relevance_score":0.2}]}`},
		"out of range":  {200, `{"results":[{"index":0,"relevance_score":0.1},{"index":5,"relevance_score":0.2}]}`},
		"missing score": {200, `{"results":[{"index":0,"relevance_score":0.1},{"index":1}]}`},
	} {
		c := server(t, tc.status, tc.body, nil)
		if _, err := c.Rerank(context.Background(), "q", []string{"a", "b"}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	c := server(t, 200, `{}`, func(*http.Request, request) { t.Error("empty input must not call the API") })
	if got, err := c.Rerank(context.Background(), "q", nil); err != nil || got != nil {
		t.Errorf("empty = %v, %v", got, err)
	}
}

func equal(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}
