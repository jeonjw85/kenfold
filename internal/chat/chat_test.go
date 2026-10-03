package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type fakeAPI struct {
	status  int
	content string
	finish  string
	last    atomic.Value // map[string]any
	auth    atomic.Value
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.auth.Store(r.Header.Get("Authorization"))
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.last.Store(body)
	if f.status != 0 {
		http.Error(w, `{"error":"model not found"}`, f.status)
		return
	}
	finish := f.finish
	if finish == "" {
		finish = "stop"
	}
	json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"finish_reason": finish, "message": map[string]any{"role": "assistant", "content": f.content}}},
		"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
	})
}

func client(t *testing.T, f *fakeAPI, mod func(*Config)) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := Config{BaseURL: srv.URL + "/v1/", Model: "qwen3.5:4b"}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type answer struct {
	Items []string `json:"items"`
}

func TestNewValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no url":   {Model: "m"},
		"scheme":   {BaseURL: "ftp://x", Model: "m"},
		"no model": {BaseURL: "http://x/v1"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c, err := New(Config{BaseURL: "http://ollama:11434/v1", Model: "m"})
	if err != nil || c.Endpoint() != "http://ollama:11434/v1/chat/completions" || c.Model() != "m" {
		t.Errorf("New = %v, %v", c, err)
	}
}

func TestJSONRequestShape(t *testing.T) {
	f := &fakeAPI{content: `{"items":["a","b"]}`}
	c := client(t, f, func(c *Config) { c.APIKey = "sk-test" })
	schema := map[string]any{"type": "object"}
	var out answer
	raw, usage, err := c.JSON(context.Background(), Request{System: "sys", User: "usr", SchemaName: "x", Schema: schema, MaxTokens: 99}, &out)
	if err != nil || len(out.Items) != 2 || raw == "" || usage.PromptTokens != 11 {
		t.Fatalf("JSON = %v, %q, %+v, %v", out, raw, usage, err)
	}
	body := f.last.Load().(map[string]any)
	if body["model"] != "qwen3.5:4b" || body["temperature"] != 0.0 || body["max_tokens"] != 99.0 || body["reasoning_effort"] != "none" {
		t.Errorf("body = %v", body)
	}
	rf := body["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["name"] != "x" || js["strict"] != true {
		t.Errorf("response_format = %v", rf)
	}
	msgs := body["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != "usr" {
		t.Errorf("messages = %v", msgs)
	}
	if f.auth.Load().(string) != "Bearer sk-test" {
		t.Errorf("auth = %v", f.auth.Load())
	}
}

func TestTextOmitsFormat(t *testing.T) {
	f := &fakeAPI{content: "yes"}
	c := client(t, f, func(c *Config) { c.Reasoning = ReasoningOmit })
	got, usage, err := c.Text(context.Background(), "", "judge this", 10)
	if err != nil || got != "yes" || usage.CompletionTokens != 7 {
		t.Fatalf("Text = %q, %+v, %v", got, usage, err)
	}
	body := f.last.Load().(map[string]any)
	if _, ok := body["response_format"]; ok {
		t.Errorf("response_format = %v", body["response_format"])
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("messages = %v", msgs)
	}
}

func TestReasoningOmit(t *testing.T) {
	f := &fakeAPI{content: `{"items":[]}`}
	c := client(t, f, func(c *Config) { c.Reasoning = ReasoningOmit })
	var out answer
	if _, _, err := c.JSON(context.Background(), Request{User: "u"}, &out); err != nil {
		t.Fatal(err)
	}
	body := f.last.Load().(map[string]any)
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("reasoning_effort sent despite omit")
	}
	if rf := body["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("no schema should request json_object: %v", rf)
	}
	if f.auth.Load().(string) != "" {
		t.Error("Authorization sent without API key")
	}
}

func TestDecodeTolerance(t *testing.T) {
	for name, content := range map[string]string{
		"plain":      `{"items":["x"]}`,
		"think":      "<think>let me see\n{not json}</think>\n{\"items\":[\"x\"]}",
		"fence":      "```json\n{\"items\":[\"x\"]}\n```",
		"prose":      "Here you go: {\"items\":[\"x\"]} Hope it helps.",
		"whitespace": "\n\n  {\"items\":[\"x\"]}  \n",
	} {
		var out answer
		if err := decodeJSON(content, &out); err != nil || len(out.Items) != 1 || out.Items[0] != "x" {
			t.Errorf("%s: %v, %v", name, out, err)
		}
	}
	for _, bad := range []string{"", "no json here", "<think>only thinking</think>", `{"items": [`} {
		var out answer
		var oe *OutputError
		if err := decodeJSON(bad, &out); !errors.As(err, &oe) {
			t.Errorf("decode(%q) err = %v, want OutputError", bad, err)
		}
	}
}

func TestErrorsAndRetryable(t *testing.T) {
	ctx := context.Background()
	var out answer

	_, _, err := client(t, &fakeAPI{status: 404}, nil).JSON(ctx, Request{User: "u"}, &out)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 || !strings.Contains(err.Error(), "model not found") || Retryable(err) {
		t.Errorf("404: %v retryable=%v", err, Retryable(err))
	}
	if _, _, err := client(t, &fakeAPI{status: 503}, nil).JSON(ctx, Request{User: "u"}, &out); !Retryable(err) {
		t.Errorf("503 should be retryable: %v", err)
	}
	if _, _, err := client(t, &fakeAPI{status: 429}, nil).JSON(ctx, Request{User: "u"}, &out); !Retryable(err) {
		t.Errorf("429 should be retryable: %v", err)
	}
	raw, _, err := client(t, &fakeAPI{content: `{"items":["a"`, finish: "length"}, nil).JSON(ctx, Request{User: "u"}, &out)
	if !errors.Is(err, ErrTruncated) || raw == "" || Retryable(err) {
		t.Errorf("truncated: %v", err)
	}
	if _, _, err := client(t, &fakeAPI{content: "sorry, I can't"}, nil).JSON(ctx, Request{User: "u"}, &out); Retryable(err) || err == nil {
		t.Errorf("bad output: %v", err)
	}
	down, _ := New(Config{BaseURL: "http://127.0.0.1:1/v1", Model: "m"})
	if _, _, err := down.JSON(ctx, Request{User: "u"}, &out); !Retryable(err) {
		t.Errorf("connection refused should be retryable: %v", err)
	}
	if Retryable(nil) {
		t.Error("nil retryable")
	}
}
