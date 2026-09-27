// Package embed is a client for OpenAI-compatible embedding APIs
// (POST {base}/embeddings). That covers local Ollama (http://host:11434/v1,
// Kenfold's default: bge-m3) as well as OpenAI, vLLM, LM Studio, and others.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultBatchSize is the number of texts sent per request.
	DefaultBatchSize = 32
	// DefaultTimeout bounds each HTTP request.
	DefaultTimeout = 30 * time.Second
	// MaxInputRunes truncates each input so it stays within typical model
	// context windows (bge-m3: 8192 tokens). Memories are short in practice.
	MaxInputRunes = 6000
	// maxResponseBytes caps how much of a response body is read.
	maxResponseBytes = 64 << 20
	// maxErrorBody caps how much of an error body is quoted in errors.
	maxErrorBody = 512
)

// Config configures a Client.
type Config struct {
	// BaseURL of the OpenAI-compatible API, e.g. http://127.0.0.1:11434/v1.
	BaseURL string
	// Model name, e.g. bge-m3 or text-embedding-3-large.
	Model string
	// APIKey is sent as a bearer token when set. Ollama ignores it.
	APIKey string
	// Dim is the required embedding dimension; responses of any other size are rejected.
	Dim int
	// SendDimensions asks the provider to return Dim dimensions (the OpenAI
	// "dimensions" parameter). Needed for models such as text-embedding-3-large
	// whose native size differs from Dim; leave off for models that are natively Dim.
	SendDimensions bool
	// BatchSize defaults to DefaultBatchSize.
	BatchSize int
	// Timeout per request; defaults to DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the HTTP client (tests).
	HTTPClient *http.Client
}

// Client calls an embeddings endpoint.
type Client struct {
	endpoint string
	cfg      Config
	http     *http.Client
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("embedding base URL %q: want http(s)://host[:port]/path", cfg.BaseURL)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("embedding model name is required")
	}
	if cfg.Dim <= 0 {
		return nil, errors.New("embedding dimension must be positive")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/embeddings"
	return &Client{endpoint: u.String(), cfg: cfg, http: hc}, nil
}

// Model returns the configured model name; it is recorded with each vector.
func (c *Client) Model() string { return c.cfg.Model }

// Endpoint returns the full embeddings URL (for logs; contains no secrets
// unless the base URL itself embeds credentials).
func (c *Client) Endpoint() string { return c.endpoint }

type request struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	Dimensions     int      `json:"dimensions,omitempty"`
}

type response struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one vector per input text, in input order.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.cfg.BatchSize {
		end := min(start+c.cfg.BatchSize, len(texts))
		vecs, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// Probe embeds a short text to check connectivity, model availability, and
// dimension. It is used at startup to report misconfiguration early.
func (c *Client) Probe(ctx context.Context) error {
	_, err := c.Embed(ctx, []string{"kenfold"})
	return err
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	in := make([]string, len(texts))
	for i, t := range texts {
		in[i] = truncateRunes(t, MaxInputRunes)
		if strings.TrimSpace(in[i]) == "" {
			in[i] = " " // some providers reject empty strings
		}
	}
	body := request{Model: c.cfg.Model, Input: in, EncodingFormat: "float"}
	if c.cfg.SendDimensions {
		body.Dimensions = c.cfg.Dim
	}
	payload, err := json.Marshal(body)
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
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("embeddings response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings: %s: %s", resp.Status, snippet(raw))
	}

	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("embeddings: decode response: %w", err)
	}
	if len(r.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: got %d vectors for %d inputs", len(r.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= len(texts) || out[d.Index] != nil {
			return nil, fmt.Errorf("embeddings: invalid or duplicate index %d", d.Index)
		}
		if len(d.Embedding) != c.cfg.Dim {
			return nil, fmt.Errorf("embeddings: model %q returned %d dimensions; Kenfold requires %d (set a %d-dimensional model, or enable the dimensions parameter if the model supports it)",
				c.cfg.Model, len(d.Embedding), c.cfg.Dim, c.cfg.Dim)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
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
