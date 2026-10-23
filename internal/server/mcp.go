package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/store"
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
	caller := &mcpgw.Caller{Principal: p, RequestID: r.Header.Get("X-Request-Id")}
	if !p.AllowsMCPServer(name) {
		s.metrics.ObserveMCPRequest(name, "other", telemetry.MCPDenied, time.Since(start))
		s.mcp.Record(caller, name, "", "", "", telemetry.MCPDenied, 0, time.Since(start))
		writeError(w, provider.ChatCompletions, http.StatusForbidden, codeMCPServerNotAllowed, "this key may not use MCP server "+name)
		return
	}
	ctx := mcpgw.WithCaller(r.Context(), caller)
	s.mcp.ServeHTTP(w, r.WithContext(ctx))
}

func (s *Server) mcpUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, provider.ChatCompletions, http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
}

// --- admin ---

func (s *Server) listMCPServers(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]any{"data": s.mcp.Servers()})
	return nil
}

func (s *Server) listMCPTools(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	switch q.Get("status") {
	case "", store.PinApproved, store.PinPending, store.PinChanged:
	default:
		return badRequest("status must be approved, pending or changed")
	}
	tools, err := s.mcp.Tools(r.Context(), q.Get("server"), q.Get("status"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": tools})
	return nil
}

func (s *Server) approveMCPTool(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Hash string `json:"hash"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if body.Hash == "" {
		return badRequest("hash is required: approve the definition you reviewed")
	}
	server, tool := r.PathValue("server"), r.PathValue("tool")
	pin, err := s.mcp.Approve(r.Context(), server, tool, body.Hash)
	if err != nil {
		return err
	}
	if err := s.audit(r.Context(), "mcp_tool.approve", "mcp_tool", server+"/"+tool, "", map[string]string{"hash": body.Hash}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, mcpgw.ToolPin{MCPToolPin: pin, Status: pin.Status()})
	return nil
}

func (s *Server) listMCPCalls(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	cq := store.MCPCallQuery{OrgID: q.Get("org_id"), TeamID: q.Get("team_id"), KeyID: q.Get("key_id"), Server: q.Get("server")}
	for name, dst := range map[string]*time.Time{"from": &cq.From, "to": &cq.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return badRequest("%s must be an RFC 3339 time", name)
			}
			*dst = t
		}
	}
	items, more, err := s.store.ListMCPCalls(r.Context(), cq, pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}
