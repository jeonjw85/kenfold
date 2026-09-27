package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

const dim = 4

// fakeAPI serves /v1/embeddings. It returns data in reverse order to check that
// the client honors "index", and records what it received.
type fakeAPI struct {
	calls    atomic.Int32
	lastAuth atomic.Value
	lastReq  atomic.Value
	dims     int
	status   int
	body     string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.lastAuth.Store(r.Header.Get("Authorization"))
	if r.URL.Path != "/v1/embeddings" || r.Method != http.MethodPost {
		http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
		return
	}
	if f.status != 0 {
		http.Error(w, f.body, f.status)
		return
	}
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.lastReq.Store(req)
	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	var data []item
	for i := len(req.Input) - 1; i >= 0; i-- {
		v := make([]float32, f.dims)
		v[0] = float32(len(req.Input[i]))
		data = append(data, item{Index: i, Embedding: v})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": req.Model})
}

func newClient(t *testing.T, api *fakeAPI, mod func(*Config)) *Client {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cfg := Config{BaseURL: srv.URL + "/v1/", Model: "bge-m3", Dim: dim, BatchSize: 2}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no url":     {Model: "m", Dim: 4},
		"bad scheme": {BaseURL: "ftp://x", Model: "m", Dim: 4},
		"no host":    {BaseURL: "http://", Model: "m", Dim: 4},
		"no model":   {BaseURL: "http://x/v1", Dim: 4},
		"no dim":     {BaseURL: "http://x/v1", Model: "m"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	c, err := New(Config{BaseURL: "http://ollama:11434/v1/", Model: "bge-m3", Dim: 1024})
	if err != nil || c.Endpoint() != "http://ollama:11434/v1/embeddings" || c.Model() != "bge-m3" {
		t.Errorf("New = %v, %v", c, err)
	}
	if c, _ := New(Config{BaseURL: "http://x/v1", Model: "kenfold-embed", Name: "bge-m3", Dim: 4}); c.Model() != "bge-m3" {
		t.Errorf("Name override: Model() = %q", c.Model())
	}
}

func TestEmbedBatchingAndOrder(t *testing.T) {
	api := &fakeAPI{dims: dim}
	c := newClient(t, api, nil)
	texts := []string{"a", "bb", "ccc", "dddd", "eeeee"}
	vecs, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("got %d vectors", len(vecs))
	}
	for i, v := range vecs {
		if int(v[0]) != len(texts[i]) {
			t.Errorf("vector %d belongs to input of length %v, want %d (order not preserved)", i, v[0], len(texts[i]))
		}
	}
	if n := api.calls.Load(); n != 3 {
		t.Errorf("requests = %d, want 3 batches of <= 2", n)
	}
	req := api.lastReq.Load().(request)
	if req.Model != "bge-m3" || req.EncodingFormat != "float" || req.Dimensions != 0 {
		t.Errorf("request = %+v", req)
	}
	if auth := api.lastAuth.Load().(string); auth != "" {
		t.Errorf("Authorization sent without API key: %q", auth)
	}
	if vecs, err := c.Embed(context.Background(), nil); err != nil || len(vecs) != 0 || api.calls.Load() != 3 {
		t.Errorf("empty input made a request or failed: %v", err)
	}
}

func TestEmbedAPIKeyAndDimensions(t *testing.T) {
	api := &fakeAPI{dims: dim}
	c := newClient(t, api, func(c *Config) { c.APIKey = "sk-test"; c.SendDimensions = true })
	if err := c.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth := api.lastAuth.Load().(string); auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", auth)
	}
	if req := api.lastReq.Load().(request); req.Dimensions != dim {
		t.Errorf("dimensions = %d, want %d", req.Dimensions, dim)
	}
}

func TestEmbedErrors(t *testing.T) {
	ctx := context.Background()

	wrongDim := newClient(t, &fakeAPI{dims: dim + 1}, nil)
	if _, err := wrongDim.Embed(ctx, []string{"x"}); err == nil || !strings.Contains(err.Error(), "requires 4") {
		t.Errorf("wrong dimension err = %v", err)
	}

	notFound := newClient(t, &fakeAPI{dims: dim, status: http.StatusNotFound, body: `{"error":"model \"bge-m3\" not found, try pulling it first"}`}, nil)
	_, err := notFound.Embed(ctx, []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "try pulling it first") {
		t.Errorf("404 err = %v", err)
	}

	huge := newClient(t, &fakeAPI{dims: dim, status: http.StatusInternalServerError, body: strings.Repeat("x", 5000)}, nil)
	if _, err := huge.Embed(ctx, []string{"x"}); err == nil || len(err.Error()) > 700 {
		t.Errorf("error body not truncated: %d chars", len(err.Error()))
	}

	down, _ := New(Config{BaseURL: "http://127.0.0.1:1/v1", Model: "m", Dim: dim})
	if _, err := down.Embed(ctx, []string{"x"}); err == nil {
		t.Error("unreachable server: no error")
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := newClient(t, &fakeAPI{dims: dim}, nil).Embed(cancelled, []string{"x"}); err == nil {
		t.Error("cancelled context: no error")
	}

	// Malformed response shapes.
	for name, payload := range map[string]string{
		"not json":        `nope`,
		"too few":         `{"data":[]}`,
		"duplicate index": `{"data":[{"index":0,"embedding":[1,2,3,4]},{"index":0,"embedding":[1,2,3,4]}]}`,
		"index range":     `{"data":[{"index":5,"embedding":[1,2,3,4]},{"index":0,"embedding":[1,2,3,4]}]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(payload)) }))
		c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dim: dim})
		if _, err := c.Embed(ctx, []string{"a", "b"}); err == nil {
			t.Errorf("%s: no error", name)
		}
		srv.Close()
	}
}

func TestInputSanitizing(t *testing.T) {
	api := &fakeAPI{dims: dim}
	c := newClient(t, api, nil)
	long := strings.Repeat("가", MaxInputRunes+100)
	if _, err := c.Embed(context.Background(), []string{"", long}); err != nil {
		t.Fatal(err)
	}
	req := api.lastReq.Load().(request)
	if req.Input[0] != " " {
		t.Errorf("empty input sent as %q", req.Input[0])
	}
	if n := utf8.RuneCountInString(req.Input[1]); n != MaxInputRunes || !utf8.ValidString(req.Input[1]) {
		t.Errorf("long input: %d runes, valid=%v", n, utf8.ValidString(req.Input[1]))
	}
}
