// Package httpserver wires Kenfold's HTTP surface: health probes and the MCP
// Streamable HTTP endpoint.
package httpserver

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath is where the MCP Streamable HTTP endpoint is mounted.
const MCPPath = "/mcp"

// Pinger reports whether a dependency (the database) is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// New returns the root handler. allowedHosts is the Host header allowlist for
// /mcp; an empty list rejects every MCP request.
//
// SECURITY: Phase 0 has no authentication. Only bind to loopback; auth
// (API keys, then OAuth 2.1 for remote clients such as ChatGPT) lands in later phases.
func New(mcpServer *mcp.Server, db Pinger, logger *slog.Logger, allowedHosts []string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusOK, "ok")
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			logger.Warn("readiness check failed", "err", err)
			writeStatus(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		writeStatus(w, http.StatusOK, "ready")
	})

	// Stateless: every POST is self-contained, so any replica can serve any
	// request. This is required for the 2026-07-28 protocol revision; older
	// clients still work because each request gets a default session.
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			Logger:                       logger,
			PropagateRequestCancellation: true,
		},
	)
	// Layered browser-attack defenses for a server on the user's machine:
	//   - Host allowlist: blocks DNS rebinding. The SDK's built-in check only
	//     covers connections arriving on a loopback address, which is not the
	//     case behind Docker port publishing, so we enforce it ourselves.
	//   - Cross-origin protection: blocks CSRF-style cross-site POSTs.
	mux.Handle(MCPPath, requireHost(allowedHosts, http.NewCrossOriginProtection().Handler(mcpHandler)))

	return mux
}

// requireHost rejects requests whose Host header (port ignored) is not allowed.
func requireHost(allowed []string, next http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, h := range allowed {
		set[normalizeHost(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !set[normalizeHost(r.Host)] {
			http.Error(w, "forbidden: host not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// normalizeHost strips an optional port and IPv6 brackets and lowercases.
func normalizeHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

func writeStatus(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"status":"` + status + `"}` + "\n"))
}
