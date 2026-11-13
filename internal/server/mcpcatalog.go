package server

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// The MCP catalog (docs/mcp-catalog.md): operators browse the virtual
// MCP servers, see which their teams may use, and request access; an
// operator who manages the organization's teams approves or denies, and
// approval adds the server to the team's allow list.

// Access of a team to a virtual server.
const (
	accessAllowed   = "allowed"
	accessPending   = "pending"
	accessAvailable = "available"
	// accessOrgBlocked means the organization's policy leaves the server
	// out, so only an owner can open it.
	accessOrgBlocked = "blocked_by_org"
)

type catalogEntry struct {
	virtualServer
	Endpoint   string   `json:"endpoint"`
	Guardrails []string `json:"guardrails"`
	// Access and RequestID describe the team asked about, if any.
	Access    string `json:"access,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// mcpCatalog lists the virtual MCP servers, with a team's access to each when team_id is given.
func (s *Server) mcpCatalog(w http.ResponseWriter, r *http.Request) error {
	var team *store.Team
	var org *store.Org
	pending := map[string]string{}
	if teamID := r.URL.Query().Get("team_id"); teamID != "" {
		var err error
		if team, org, err = s.readableTeam(r, teamID); err != nil {
			return err
		}
		reqs, err := s.store.ListMCPAccessRequests(r.Context(), store.MCPRequestFilter{TeamIDs: []string{team.ID}, Status: store.RequestPending})
		if err != nil {
			return err
		}
		for _, q := range reqs {
			pending[q.Server] = q.ID
		}
	}
	vs := s.rt.Load().cfg.MCP.VirtualServers
	out := []catalogEntry{}
	for _, name := range slices.Sorted(maps.Keys(vs)) {
		v := vs[name]
		e := catalogEntry{virtualServer: virtualServer{Name: name, Description: v.Description}, Endpoint: "/mcp/" + name, Guardrails: v.Guardrails}
		if e.Guardrails == nil {
			e.Guardrails = []string{}
		}
		for _, ref := range v.Servers {
			e.Servers = append(e.Servers, virtualServerUpstream{Server: ref.Server, Prefix: ref.NamePrefix(), Tools: ref.Tools})
		}
		if team != nil {
			e.Access = teamAccess(org, team, name)
			if id, ok := pending[name]; ok && e.Access != accessAllowed {
				e.Access, e.RequestID = accessPending, id
			}
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

func teamAccess(org *store.Org, team *store.Team, server string) string {
	switch {
	case !org.Policy.AllowsMCPServer(server):
		return accessOrgBlocked
	case team.Policy.AllowsMCPServer(server):
		return accessAllowed
	default:
		return accessAvailable
	}
}

// readableTeam loads a team and its organization if the operator may read the team.
func (s *Server) readableTeam(r *http.Request, teamID string) (*store.Team, *store.Org, error) {
	team, err := s.store.GetTeam(r.Context(), teamID)
	if err != nil {
		return nil, nil, err
	}
	if err := allow(r, config.PermTeamsRead, team.OrgID, team.ID); err != nil {
		return nil, nil, err
	}
	org, err := s.store.GetOrg(r.Context(), team.OrgID)
	if err != nil {
		return nil, nil, err
	}
	return team, org, nil
}

// createMCPRequest asks for a team's access to a virtual MCP server.
func (s *Server) createMCPRequest(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		TeamID string `json:"team_id"`
		Server string `json:"server"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if body.TeamID == "" || body.Server == "" {
		return badRequest("team_id and server are required")
	}
	if len(body.Reason) > 2000 {
		return badRequest("reason may be at most 2000 characters")
	}
	if _, ok := s.rt.Load().cfg.MCP.VirtualServers[body.Server]; !ok {
		return &apiError{http.StatusNotFound, "not_found", "no virtual MCP server " + body.Server}
	}
	team, org, err := s.readableTeam(r, body.TeamID)
	if err != nil {
		return err
	}
	if teamAccess(org, team, body.Server) == accessAllowed {
		return &apiError{http.StatusConflict, "conflict", team.Name + " may already use " + body.Server}
	}
	q := &store.MCPAccessRequest{OrgID: org.ID, TeamID: team.ID, Server: body.Server, Reason: body.Reason, RequestedBy: operatorFrom(r.Context()).actor()}
	if err := s.store.CreateMCPAccessRequest(r.Context(), q); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return &apiError{http.StatusConflict, "conflict", team.Name + " already has a pending request for " + body.Server}
		}
		return err
	}
	if err := s.audit(r.Context(), "mcp_request.create", "mcp_request", q.ID, org.ID, q); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, q)
	return nil
}

// listMCPRequests lists MCP access requests of the teams the operator can read, newest first.
func (s *Server) listMCPRequests(w http.ResponseWriter, r *http.Request) error {
	all, orgIDs, teamIDs := operatorFrom(r.Context()).scope(config.PermTeamsRead)
	f := store.MCPRequestFilter{Status: r.URL.Query().Get("status")}
	if !all {
		f.OrgIDs, f.TeamIDs = orgIDs, teamIDs
	}
	if org := r.URL.Query().Get("org_id"); org != "" {
		if !all && !slices.Contains(orgIDs, org) {
			return errForbidden
		}
		f.OrgIDs, f.TeamIDs = []string{org}, nil
	}
	out, err := s.store.ListMCPAccessRequests(r.Context(), f)
	if err != nil {
		return err
	}
	type row struct {
		*store.MCPAccessRequest
		TeamName string `json:"team_name"`
		// CanDecide tells the UI whether this operator may approve it.
		CanDecide bool `json:"can_decide"`
	}
	t := s.tenants.Load()
	op := operatorFrom(r.Context())
	rows := make([]row, 0, len(out))
	for _, q := range out {
		rw := row{MCPAccessRequest: q, CanDecide: q.Status == store.RequestPending && op.can(config.PermTeamsWrite, q.OrgID, "")}
		if tm := t.team(q.OrgID, q.TeamID); tm != nil {
			rw.TeamName = tm.Name
		}
		rows = append(rows, rw)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
	return nil
}

// approveMCPRequest approves a request and adds the server to the team's allowed MCP servers.
func (s *Server) approveMCPRequest(w http.ResponseWriter, r *http.Request) error {
	return s.decideMCPRequest(w, r, store.RequestApproved)
}

// denyMCPRequest denies a request with an optional note.
func (s *Server) denyMCPRequest(w http.ResponseWriter, r *http.Request) error {
	return s.decideMCPRequest(w, r, store.RequestDenied)
}

func (s *Server) decideMCPRequest(w http.ResponseWriter, r *http.Request, status string) error {
	var body struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return err
		}
	}
	q, err := s.store.GetMCPAccessRequest(r.Context(), r.PathValue("request_id"))
	if err != nil {
		return err
	}
	// Approving changes the team's policy, which a team-level grant may
	// not do (ADR 0032), so deciding needs teams.write for the whole
	// organization.
	if err := allow(r, config.PermTeamsWrite, q.OrgID, ""); err != nil {
		return err
	}
	if q.Status != store.RequestPending {
		return &apiError{http.StatusConflict, "conflict", "the request was already " + q.Status}
	}
	if status == store.RequestApproved {
		team, err := s.store.GetTeam(r.Context(), q.TeamID)
		if err != nil {
			return err
		}
		org, err := s.store.GetOrg(r.Context(), q.OrgID)
		if err != nil {
			return err
		}
		switch teamAccess(org, team, q.Server) {
		case accessOrgBlocked:
			return &apiError{http.StatusConflict, "conflict", "the organization's policy does not allow " + q.Server + ": an owner must add it to the organization first"}
		case accessAvailable:
			before := *team
			team.Policy.AllowedMCPServers = append(slices.Clone(team.Policy.AllowedMCPServers), q.Server)
			if err := s.store.UpdateTeam(r.Context(), team); err != nil {
				return err
			}
			if err := s.audit(r.Context(), "team.update", "team", team.ID, team.OrgID, map[string]any{"before": before, "after": team, "mcp_request": q.ID}); err != nil {
				return err
			}
		}
	}
	q, err = s.store.DecideMCPAccessRequest(r.Context(), q.ID, status, operatorFrom(r.Context()).actor(), strings.TrimSpace(body.Note))
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return &apiError{http.StatusConflict, "conflict", "the request was decided meanwhile"}
		}
		return err
	}
	action := "mcp_request.approve"
	if status == store.RequestDenied {
		action = "mcp_request.deny"
	}
	if err := s.audit(r.Context(), action, "mcp_request", q.ID, q.OrgID, q); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, q)
	return nil
}
