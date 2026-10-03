package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/mcpserver"
)

// client is a lazily connected MCP client for one hook invocation.
type client struct {
	o  Options
	cs *mcp.ClientSession
}

func newClient(o Options) *client { return &client{o: o} }

// toolError is an error the server returned as a tool result (isError). Most
// are permanent (validation, secret rejection); internal errors are transient.
type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

// transient reports whether retrying later could succeed: transport failures
// and internal server errors, but not rejected input.
func transient(err error) bool {
	var te *toolError
	if errors.As(err, &te) {
		return strings.Contains(te.msg, "internal error")
	}
	return true
}

func (c *client) connect(ctx context.Context) (*mcp.ClientSession, error) {
	if c.cs != nil {
		return c.cs, nil
	}
	hc := c.o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	if c.o.APIKey != "" {
		base := hc.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		copied := *hc
		copied.Transport = bearer{token: c.o.APIKey, base: base}
		checkRedirect := hc.CheckRedirect
		copied.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && (req.URL.Scheme != via[0].URL.Scheme || !strings.EqualFold(req.URL.Host, via[0].URL.Host)) {
				return errors.New("refusing to redirect an authenticated request to a different origin")
			}
			if checkRedirect != nil {
				return checkRedirect(req, via)
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
		hc = &copied
	}
	tr := &mcp.StreamableClientTransport{
		Endpoint:             c.o.URL,
		HTTPClient:           hc,
		DisableStandaloneSSE: true,
		MaxRetries:           -1, // hooks have short budgets; fail fast and spool instead
	}
	cl := mcp.NewClient(&mcp.Implementation{Name: c.o.ClientName, Version: c.o.Version}, nil)
	cs, err := cl.Connect(ctx, tr, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", c.o.URL, err)
	}
	c.cs = cs
	return cs, nil
}

// call invokes tool and decodes its structured result into out (if non-nil).
// session, when set, is passed as _meta so the server records it as the
// memory's source_session.
func (c *client) call(ctx context.Context, tool string, args map[string]any, session string, out any) error {
	cs, err := c.connect(ctx)
	if err != nil {
		return err
	}
	params := &mcp.CallToolParams{Name: tool, Arguments: args}
	if session != "" {
		params.Meta = mcp.Meta{mcpserver.MetaSessionID: session}
	}
	res, err := cs.CallTool(ctx, params)
	if err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	if res.IsError {
		msg := tool + " failed"
		if len(res.Content) > 0 {
			if t, ok := res.Content[0].(*mcp.TextContent); ok {
				msg = t.Text
			}
		}
		return &toolError{msg: msg}
	}
	if out == nil {
		return nil
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (c *client) close() {
	if c.cs != nil {
		_ = c.cs.Close()
	}
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}
