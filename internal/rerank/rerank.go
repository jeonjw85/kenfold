// Package rerank is a client for cross-encoder rerank APIs: POST {base}/rerank
// with a query and documents, returning a relevance score per document. The
// request shape is shared by llama.cpp's llama-server (--reranking), Jina,
// Cohere (v2), and Voyage; Kenfold's default is a local llama-server with
// bge-reranker-v2-m3.
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultTimeout bounds each request.
	DefaultTimeout = 10 * time.Second
	// MaxDocRunes truncates each document. Cost grows with length, and the
	// part of a memory that matters (a statement, or a summary's requests)
	// comes first.
	MaxDocRunes      = 600
	maxQueryRunes    = 500
	maxResponseBytes = 8 << 20
	maxErrorBody     = 512
)

// Config configures a Client.
type Config struct {
	// BaseURL of the API, e.g. http://127.0.0.1:8080/v1 for llama-server,
	// https://api.jina.ai/v1, https://api.cohere.com/v2.
	BaseURL string
	// Model name. llama-server serves one model and accepts any name.
	Model string
	// APIKey is sent as a bearer token when set.
	APIKey string
	// Timeout per request; defaults to DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the HTTP client (tests).
	HTTPClient *http.Client
}

// Client calls a rerank endpoint.
type Client struct {
	endpoint string
	cfg      Config
	http     *http.Client
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("rerank base URL %q: want http(s)://host[:port]/path", cfg.BaseURL)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("rerank model name is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/rerank"
	return &Client{endpoint: u.String(), cfg: cfg, http: hc}, nil
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.cfg.Model }

// Endpoint returns the full rerank URL (for logs).
func (c *Client) Endpoint() string { return c.endpoint }

type request struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

type result struct {
	Index          int      `json:"index"`
	RelevanceScore *float64 `json:"relevance_score"`
}

type response struct {
	Results []result `json:"results"` // llama-server, Jina, Cohere
	Data    []result `json:"data"`    // Voyage
}

// Rerank scores each document's relevance to query and returns the scores in
// document order, in [0, 1] (higher is more relevant). APIs that return raw
// logits (llama-server) are mapped through a sigmoid; that keeps the order
// and makes scores comparable across calls to the same model.
func (c *Client) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	in := make([]string, len(docs))
	for i, d := range docs {
		in[i] = truncateRunes(strings.TrimSpace(d), MaxDocRunes)
		if in[i] == "" {
			in[i] = " "
		}
	}
	payload, err := json.Marshal(request{Model: c.cfg.Model, Query: truncateRunes(query, maxQueryRunes), Documents: in, TopN: len(in)})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("rerank response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank: %s: %s", resp.Status, snippet(raw))
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("rerank: decode response: %w", err)
	}
	results := r.Results
	if len(results) == 0 {
		results = r.Data
	}
	if len(results) != len(docs) {
		return nil, fmt.Errorf("rerank: got %d scores for %d documents", len(results), len(docs))
	}
	scores := make([]float64, len(docs))
	seen := make([]bool, len(docs))
	logits := false
	for _, res := range results {
		if res.Index < 0 || res.Index >= len(docs) || seen[res.Index] {
			return nil, fmt.Errorf("rerank: invalid or duplicate index %d", res.Index)
		}
		if res.RelevanceScore == nil {
			return nil, fmt.Errorf("rerank: no relevance_score for document %d", res.Index)
		}
		s := *res.RelevanceScore
		if math.IsNaN(s) || math.IsInf(s, 0) {
			return nil, fmt.Errorf("rerank: invalid score for document %d", res.Index)
		}
		if s < 0 || s > 1 {
			logits = true
		}
		seen[res.Index] = true
		scores[res.Index] = s
	}
	if logits {
		for i, s := range scores {
			scores[i] = 1 / (1 + math.Exp(-s))
		}
	}
	return scores, nil
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "…"
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	return s
}
