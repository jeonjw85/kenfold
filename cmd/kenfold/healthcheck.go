package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// healthcheck uses the server's readiness endpoint so it also works in the
// distroless image, which has no shell or curl.
func (c *cli) healthcheck(ctx context.Context) error {
	host, port, err := net.SplitHostPort(c.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("KENFOLD_HTTP_ADDR: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/readyz"}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		// Always contact the local listener directly, even when the process
		// inherits HTTP_PROXY from its deployment environment.
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("readiness check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server is not ready: %s", resp.Status)
	}
	fmt.Fprintln(c.out, "ready")
	return nil
}
