package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/oauth"
	"github.com/rnikadi/genaiopengateway/internal/policy"
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
	rt := s.rt.Load()
	caller, fail := s.mcpCaller(r, rt, name)
	if fail != nil {
		s.mcpAuthError(w, rt, name, fail)
		return
	}
	if !s.mcp.Has(name) {
		writeError(w, provider.ChatCompletions, http.StatusNotFound, codeMCPServerNotFound, "MCP server "+name+" is not configured")
		return
	}
	if !caller.Principal.AllowsMCPServer(name) {
		s.metrics.ObserveMCPRequest(name, "other", telemetry.MCPDenied, time.Since(start))
		s.mcp.Record(caller, name, "", "", "", telemetry.MCPDenied, 0, time.Since(start))
		writeError(w, provider.ChatCompletions, http.StatusForbidden, codeMCPServerNotAllowed, "you may not use MCP server "+name)
		return
	}
	ctx := mcpgw.WithCaller(r.Context(), caller)
	s.mcp.ServeHTTP(w, r.WithContext(ctx))
}

// authFailure is why an MCP request was not authenticated.
type authFailure struct {
	status int
	msg    string
	// oauthError is the RFC 6750 error code for the challenge, if any.
	oauthError string
}

// mcpCaller authenticates a bearer token: a gateway key, or an OAuth
// access token when OAuth is configured.
func (s *Server) mcpCaller(r *http.Request, rt *runtime, name string) (*mcpgw.Caller, *authFailure) {
	rid := r.Header.Get("X-Request-Id")
	v := r.Header.Get("Authorization")
	// MCP clients send credentials only as a bearer token.
	if len(v) < 7 || !strings.EqualFold(v[:7], "bearer ") {
		return nil, &authFailure{http.StatusUnauthorized, "missing credentials", ""}
	}
	if p, ok := s.authenticate(r); ok {
		return &mcpgw.Caller{Principal: p, RequestID: rid}, nil
	}
	oc := rt.cfg.MCP.OAuth
	if rt.oauth == nil {
		return nil, &authFailure{http.StatusUnauthorized, "invalid API key", ""}
	}
	resources := []string{rt.cfg.MCP.PublicURL + "/mcp/" + name}
	if oc.Audience != "" {
		resources = append(resources, oc.Audience)
	}
	tok, err := rt.oauth.Verify(r.Context(), strings.TrimSpace(v[7:]), resources)
	switch {
	case errors.Is(err, oauth.ErrInsufficientScope):
		return nil, &authFailure{http.StatusForbidden, "the access token lacks a required scope", "insufficient_scope"}
	case err != nil:
		s.log.Info("rejected MCP access token", "request_id", rid, "virtual_server", name, "err", err)
		return nil, &authFailure{http.StatusUnauthorized, "invalid API key or access token", "invalid_token"}
	}
	p, err := s.oauthPrincipal(tok, oc)
	if err != nil {
		s.log.Warn("MCP access token maps to no team", "request_id", rid, "subject", tok.Subject, "err", err)
		return nil, &authFailure{http.StatusForbidden, err.Error(), ""}
	}
	return &mcpgw.Caller{Principal: p, Subject: tok.Subject, RequestID: rid}, nil
}

// tenants indexes organizations, and teams by organization and by name
// or id, as last loaded from the store.
type tenants struct {
	orgs  map[string]*store.Org
	teams map[string]map[string]*store.Team
}

// oauthPrincipal maps a token to the configured organization and to the
// first team, named or identified by the team claim, that exists in it.
// Limits and policies apply at organization and team level.
func (s *Server) oauthPrincipal(tok *oauth.Token, oc *config.OAuth) (*auth.Principal, error) {
	t := s.tenants.Load()
	if t == nil {
		return nil, errors.New("tenants are not loaded yet")
	}
	org := t.orgs[oc.Organization]
	if org == nil {
		return nil, fmt.Errorf("organization %s is not configured", oc.Organization)
	}
	claimed := oauth.Strings(tok.Claims[oc.TeamClaim])
	for _, v := range claimed {
		if team := t.teams[org.ID][v]; team != nil {
			return &auth.Principal{
				OrgID: org.ID, TeamID: team.ID, KeyName: "oauth",
				Policies: [3]policy.Policy{org.Policy, team.Policy, {}},
			}, nil
		}
	}
	return nil, fmt.Errorf("no team in the organization matches the %s claim", oc.TeamClaim)
}

// mcpAuthError answers an authentication failure. With OAuth configured,
// the challenge points clients at the protected resource metadata, so
// they can find the authorization server (RFC 9728).
func (s *Server) mcpAuthError(w http.ResponseWriter, rt *runtime, name string, f *authFailure) {
	challenge := "Bearer"
	if oc := rt.cfg.MCP.OAuth; oc != nil {
		params := []string{fmt.Sprintf("resource_metadata=%q", rt.cfg.MCP.PublicURL+"/.well-known/oauth-protected-resource/mcp/"+name)}
		if len(oc.Scopes) > 0 {
			params = append(params, fmt.Sprintf("scope=%q", strings.Join(oc.Scopes, " ")))
		}
		if f.oauthError != "" {
			params = append(params, fmt.Sprintf("error=%q", f.oauthError))
		}
		challenge += " " + strings.Join(params, ", ")
	}
	w.Header().Set("WWW-Authenticate", challenge)
	code := codeInvalidKey
	if f.status == http.StatusForbidden {
		code = codeMCPServerNotAllowed
	}
	writeError(w, provider.ChatCompletions, f.status, code, f.msg)
}

// mcpResourceMetadata serves RFC 9728 protected resource metadata for a
// virtual server, when OAuth is configured.
func (s *Server) mcpResourceMetadata(w http.ResponseWriter, r *http.Request) {
	rt := s.rt.Load()
	name := r.PathValue("name")
	oc := rt.cfg.MCP.OAuth
	if oc == nil || !s.mcp.Has(name) {
		http.NotFound(w, r)
		return
	}
	sdkauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               rt.cfg.MCP.PublicURL + "/mcp/" + name,
		AuthorizationServers:   []string{oc.Issuer},
		ScopesSupported:        oc.Scopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "GenAIOpenGateway MCP server " + name,
	}).ServeHTTP(w, r)
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
