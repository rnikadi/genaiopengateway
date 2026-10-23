package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
)

// Gateway error codes for MCP endpoints.
const (
	codeMCPServerNotFound   = "mcp_server_not_found"
	codeMCPServerNotAllowed = "mcp_server_not_allowed"
)

// serveMCP authenticates and authorizes a request to a virtual MCP
// server, then hands it to the MCP gateway. Failures here are HTTP
// errors; failures inside a request are JSON-RPC errors.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	name := r.PathValue("name")
	// MCP clients send credentials only as a bearer token.
	if v := r.Header.Get("Authorization"); len(v) < 7 || !strings.EqualFold(v[:7], "bearer ") {
		s.mcpUnauthorized(w)
		return
	}
	p, ok := s.authenticate(r)
	if !ok {
		s.mcpUnauthorized(w)
		return
	}
	if !s.mcp.Has(name) {
		writeError(w, provider.ChatCompletions, http.StatusNotFound, codeMCPServerNotFound, "MCP server "+name+" is not configured")
		return
	}
	if !p.AllowsMCPServer(name) {
		s.metrics.ObserveMCPRequest(name, "other", telemetry.MCPDenied, time.Since(start))
		writeError(w, provider.ChatCompletions, http.StatusForbidden, codeMCPServerNotAllowed, "this key may not use MCP server "+name)
		return
	}
	ctx := mcpgw.WithCaller(r.Context(), &mcpgw.Caller{Principal: p, RequestID: r.Header.Get("X-Request-Id")})
	s.mcp.ServeHTTP(w, r.WithContext(ctx))
}

func (s *Server) mcpUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, provider.ChatCompletions, http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
}
