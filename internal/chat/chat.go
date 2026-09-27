// Package chat is a client for OpenAI-compatible chat completion APIs
// (POST {base}/chat/completions) with structured JSON output. It covers local
// Ollama (Kenfold's default for memory extraction) as well as OpenAI, vLLM,
// LM Studio, and other compatible servers.
package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds one completion. Small local models on CPU can take
	// tens of seconds for a long session summary.
	DefaultTimeout   = 3 * time.Minute
	maxResponseBytes = 8 << 20
	maxErrorBody     = 512
)

// ReasoningOmit leaves reasoning_effort out of the request, for servers that
// reject the parameter.
const ReasoningOmit = "omit"

// Config configures a Client.
type Config struct {
	// BaseURL of the API, e.g. http://127.0.0.1:11434/v1.
	BaseURL string
	// Model name, e.g. qwen3.5:4b.
	Model string
	// APIKey is sent as a bearer token when set.
	APIKey string
	// Reasoning is sent as reasoning_effort. "" defaults to "none": extraction
	// is a short structured task, and small thinking models otherwise spend
	// minutes reasoning on CPU. Use ReasoningOmit to leave the field out.
	Reasoning string
	// Timeout per request; defaults to DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the HTTP client (tests).
	HTTPClient *http.Client
}

// Client calls a chat completions endpoint.
type Client struct {
	endpoint string
	cfg      Config
	http     *http.Client
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("chat base URL %q: want http(s)://host[:port]/path", cfg.BaseURL)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("chat model name is required")
	}
	if cfg.Reasoning == "" {
		cfg.Reasoning = "none"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/chat/completions"
	return &Client{endpoint: u.String(), cfg: cfg, http: hc}, nil
}

// Model returns the configured model name; it is recorded on extracted memories.
func (c *Client) Model() string { return c.cfg.Model }

// Endpoint returns the full chat completions URL.
func (c *Client) Endpoint() string { return c.endpoint }

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is one structured completion.
type Request struct {
	System string
	User   string
	// SchemaName and Schema describe the required JSON output. The schema is
	// enforced by the server where supported (response_format json_schema) and
	// the result is always validated again by the caller.
	SchemaName string
	Schema     map[string]any
	MaxTokens  int
}

// Usage reports token counts, when the server returns them.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// ErrTruncated means the model stopped at MaxTokens, so its JSON is likely incomplete.
var ErrTruncated = errors.New("model output was truncated at the token limit")

type wireRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Temperature     float64   `json:"temperature"`
	Seed            int       `json:"seed"`
	MaxTokens       int       `json:"max_tokens,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	ResponseFormat  any       `json:"response_format,omitempty"`
}

type wireResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

// JSON runs a completion and decodes the model's JSON answer into out. It
// returns the raw content (for diagnostics) and token usage.
func (c *Client) JSON(ctx context.Context, r Request, out any) (string, Usage, error) {
	body := wireRequest{
		Model:       c.cfg.Model,
		Messages:    []Message{{Role: "system", Content: r.System}, {Role: "user", Content: r.User}},
		Temperature: 0,
		Seed:        7,
		MaxTokens:   r.MaxTokens,
	}
	if c.cfg.Reasoning != ReasoningOmit {
		body.ReasoningEffort = c.cfg.Reasoning
	}
	if r.Schema != nil {
		name := r.SchemaName
		if name == "" {
			name = "result"
		}
		body.ResponseFormat = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": name, "strict": true, "schema": r.Schema},
		}
	} else {
		body.ResponseFormat = map[string]any{"type": "json_object"}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", Usage{}, fmt.Errorf("chat response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, &StatusError{Code: resp.StatusCode, Status: resp.Status, Body: snippet(raw)}
	}
	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return "", Usage{}, fmt.Errorf("chat: decode response: %w", err)
	}
	if len(wr.Choices) == 0 {
		return "", wr.Usage, errors.New("chat: response has no choices")
	}
	content := wr.Choices[0].Message.Content
	if wr.Choices[0].FinishReason == "length" {
		return content, wr.Usage, ErrTruncated
	}
	if err := decodeJSON(content, out); err != nil {
		return content, wr.Usage, err
	}
	return content, wr.Usage, nil
}

// StatusError is a non-200 response.
type StatusError struct {
	Code   int
	Status string
	Body   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("chat: %s: %s", e.Status, e.Body) }

// Retryable reports whether err is worth retrying later: transport errors,
// timeouts, rate limits, and server errors, but not bad requests or bad output.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code == http.StatusRequestTimeout || se.Code >= 500
	}
	var oe *OutputError
	if errors.As(err, &oe) || errors.Is(err, ErrTruncated) {
		return false
	}
	return true
}

// OutputError means the model answered but not with valid JSON.
type OutputError struct{ Err error }

func (e *OutputError) Error() string { return "chat: model output is not valid JSON: " + e.Err.Error() }
func (e *OutputError) Unwrap() error { return e.Err }

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

// decodeJSON parses the model's answer, tolerating what models add around JSON
// when the server does not enforce the format: reasoning blocks and Markdown
// code fences.
func decodeJSON(content string, out any) error {
	s := strings.TrimSpace(thinkBlock.ReplaceAllString(content, ""))
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	if i := strings.IndexByte(s, '{'); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndexByte(s, '}'); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}
	if s == "" {
		return &OutputError{Err: errors.New("empty answer")}
	}
	if err := json.Unmarshal([]byte(s), out); err != nil {
		return &OutputError{Err: err}
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "…"
	}
	return strings.ToValidUTF8(s, "?")
}
