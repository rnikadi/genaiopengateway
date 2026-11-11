package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/authz"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Cedar policies per organization (ADR 0009, docs/cedar.md): an owner
// writes them, every save is a version, and requests of an organization
// that has any are allowed only when they permit it.

// maxCedarBytes bounds a policy document.
const maxCedarBytes = 256 << 10

// orgCedar is an organization's policies in force. A stored document
// that no longer parses denies every request rather than allowing it.
type orgCedar struct {
	policies *authz.Policies
	err      error
}

// loadCedar parses the policies in force of every organization.
func (s *Server) loadCedar(ctx context.Context) error {
	stored, err := s.store.LoadCedarPolicies(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]*orgCedar, len(stored))
	for org, p := range stored {
		ps, err := authz.Parse(p.Text)
		if err != nil {
			s.log.Error("an organization's Cedar policies do not parse; denying its requests", "org", org, "version", p.Version, "err", err)
		} else {
			ps.Version = p.Version
		}
		m[org] = &orgCedar{policies: ps, err: err}
	}
	s.cedar.Store(&m)
	return nil
}

// cedarDenied reports why an organization's policies refuse a request,
// or "" when they permit it or the organization has none.
func (s *Server) cedarDenied(req authz.Request) string {
	m := s.cedar.Load()
	if m == nil {
		return ""
	}
	oc := (*m)[req.OrgID]
	if oc == nil {
		return ""
	}
	if oc.err != nil {
		return "this organization's Cedar policies cannot be read, so its requests are denied"
	}
	if t := s.tenants.Load(); t != nil {
		if team := t.team(req.OrgID, req.TeamID); team != nil {
			req.TeamName = team.Name
		}
	}
	req.Time = time.Now()
	d := oc.policies.Decide(req)
	for _, e := range d.Errors {
		s.log.Warn("a Cedar policy failed to evaluate and was skipped", "org", req.OrgID, "err", e)
	}
	if d.Allowed {
		return ""
	}
	return d.Reason()
}

// modelDenied is cedarDenied for a model request.
func (s *Server) modelDenied(r *http.Request, p *auth.Principal, endpoint, model, user string, stream bool) string {
	return s.cedarDenied(authz.Request{
		OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID, KeyName: p.KeyName,
		Action: endpoint, Model: model, User: user, IP: r.RemoteAddr, Stream: stream,
	})
}

// authorizeTool is the MCP gateway's hook for a tool call.
func (s *Server) authorizeTool(_ context.Context, c *mcpgw.Caller, server, tool string) string {
	if c == nil || c.Principal == nil {
		return ""
	}
	p := c.Principal
	return s.cedarDenied(authz.Request{
		OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID, KeyName: p.KeyName,
		Server: server, Tool: tool, User: c.Subject, IP: c.RemoteAddr,
	})
}

// cedarView is an organization's policies as the admin API shows them.
type cedarView struct {
	*store.CedarPolicies
	Policies int `json:"policies"`
}

// getCedar returns an organization's Cedar policies in force, or a version given by ?version=.
func (s *Server) getCedar(w http.ResponseWriter, r *http.Request) error {
	orgID := r.PathValue("org_id")
	if err := allow(r, config.PermOrgRead, orgID, ""); err != nil {
		return err
	}
	var version int64
	if v := r.URL.Query().Get("version"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return badRequest("version must be a positive integer")
		}
		version = n
	}
	p, err := s.store.GetCedarPolicies(r.Context(), orgID, version)
	if errors.Is(err, store.ErrNotFound) && version == 0 {
		if _, err := s.store.GetOrg(r.Context(), orgID); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, cedarView{CedarPolicies: &store.CedarPolicies{OrgID: orgID}})
		return nil
	}
	if err != nil {
		return err
	}
	v := cedarView{CedarPolicies: p}
	if ps, err := authz.Parse(p.Text); err == nil && p.Text != "" {
		v.Policies = ps.Count()
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

// listCedarVersions lists the versions of an organization's Cedar policies, newest first.
func (s *Server) listCedarVersions(w http.ResponseWriter, r *http.Request) error {
	orgID := r.PathValue("org_id")
	if err := allow(r, config.PermOrgRead, orgID, ""); err != nil {
		return err
	}
	out, err := s.store.ListCedarVersions(r.Context(), orgID, 100)
	if err != nil {
		return err
	}
	if out == nil {
		out = []*store.CedarPolicies{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

// putCedar saves a new version of an organization's Cedar policies; only owners may.
func (s *Server) putCedar(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if len(body.Text) > maxCedarBytes {
		return badRequest("policies may be at most %d bytes", maxCedarBytes)
	}
	if strings.TrimSpace(body.Text) == "" {
		return badRequest("text is empty: DELETE removes an organization's policies")
	}
	ps, err := authz.Parse(body.Text)
	if err != nil {
		return badRequest("the policies do not parse: %v", err)
	}
	return s.saveCedar(w, r, body.Text, ps.Count())
}

// deleteCedar removes an organization's Cedar policies, as a new empty version; only owners may.
func (s *Server) deleteCedar(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	return s.saveCedar(w, r, "", 0)
}

func (s *Server) saveCedar(w http.ResponseWriter, r *http.Request, text string, n int) error {
	orgID := r.PathValue("org_id")
	p, err := s.store.SaveCedarPolicies(r.Context(), orgID, text, operatorFrom(r.Context()).actor())
	if err != nil {
		return err
	}
	action := "cedar.update"
	if text == "" {
		action = "cedar.delete"
	}
	if err := s.audit(r.Context(), action, "cedar", orgID, orgID, map[string]any{"version": p.Version, "policies": n}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, cedarView{CedarPolicies: p, Policies: n})
	return nil
}

// checkCedar evaluates a request against an organization's Cedar policies, or against draft text, without sending it.
func (s *Server) checkCedar(w http.ResponseWriter, r *http.Request) error {
	orgID := r.PathValue("org_id")
	if err := allow(r, config.PermOrgRead, orgID, ""); err != nil {
		return err
	}
	var body struct {
		// Text, when set, is a draft to check instead of the policies in force.
		Text   *string    `json:"text"`
		KeyID  string     `json:"key_id"`
		TeamID string     `json:"team_id"`
		Action string     `json:"action"`
		Model  string     `json:"model"`
		Server string     `json:"server"`
		Tool   string     `json:"tool"`
		User   string     `json:"user"`
		IP     string     `json:"ip"`
		Stream bool       `json:"stream"`
		Time   *time.Time `json:"time"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	if (body.Tool == "") == (body.Model == "") {
		return badRequest("give either model, with action, or server and tool")
	}
	if body.Tool != "" && body.Server == "" {
		return badRequest("server is required with tool")
	}
	if body.Model != "" && body.Action == "" {
		body.Action = "chat.completions"
	}
	req := authz.Request{
		OrgID: orgID, TeamID: body.TeamID, KeyID: body.KeyID, Action: body.Action, Model: body.Model,
		Server: body.Server, Tool: body.Tool, User: body.User, IP: body.IP, Stream: body.Stream, Time: time.Now(),
	}
	if body.Time != nil {
		req.Time = *body.Time
	}
	if body.KeyID != "" {
		k, err := s.store.GetKey(r.Context(), body.KeyID)
		if err != nil {
			return err
		}
		if k.OrgID != orgID {
			return &apiError{http.StatusNotFound, "not_found", "no such key in this organization"}
		}
		req.TeamID, req.KeyName = k.TeamID, k.Name
	}
	if t := s.tenants.Load(); t != nil && req.TeamID != "" {
		if team := t.team(orgID, req.TeamID); team != nil {
			req.TeamID, req.TeamName = team.ID, team.Name
		}
	}
	var ps *authz.Policies
	if body.Text != nil {
		var err error
		if ps, err = authz.Parse(*body.Text); err != nil {
			return badRequest("the policies do not parse: %v", err)
		}
	} else {
		p, err := s.store.GetCedarPolicies(r.Context(), orgID, 0)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if p == nil || p.Text == "" {
			writeJSON(w, http.StatusOK, map[string]any{"allowed": true, "policies": []string{}, "reason": "this organization has no Cedar policies"})
			return nil
		}
		if ps, err = authz.Parse(p.Text); err != nil {
			return badRequest("the stored policies do not parse: %v", err)
		}
	}
	d := ps.Decide(req)
	out := map[string]any{"allowed": d.Allowed, "policies": nonNil(d.Policies), "errors": nonNil(d.Errors)}
	if !d.Allowed {
		out["reason"] = d.Reason()
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
