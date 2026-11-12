package server

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// The developer portal (docs/developer-portal.md): people with keys.self
// on a team see what the team may use, and create, rotate and revoke
// keys they own, within the team's policy and sso.portal's limits. A
// portal key has no policy of its own, so the team's and organization's
// policies are its ceiling, as for every key.

func (s *Server) portalRoutes(mux *http.ServeMux) {
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request) error) {
		mux.Handle(pattern, s.adminAuth(fn))
	}
	h("GET /admin/v1/portal", s.portal)
	h("POST /admin/v1/portal/teams/{team_id}/keys", s.portalCreateKey)
	h("POST /admin/v1/portal/keys/{key_id}/rotate", s.portalRotateKey)
	h("POST /admin/v1/portal/keys/{key_id}/revoke", s.portalRevokeKey)
	h("POST /admin/v1/portal/teams/{team_id}/requests", s.portalRequestAccess)
}

// portalMaxGrace bounds how long a rotated portal key's old secret works.
const portalMaxGrace = 7 * 24 * time.Hour

// portalOwner returns who signed in, by email, which is who portal keys
// belong to. The admin token has no email, so it cannot own keys.
func portalOwner(r *http.Request) (string, error) {
	email := strings.ToLower(strings.TrimSpace(operatorFrom(r.Context()).Email))
	if email == "" {
		return "", &apiError{http.StatusForbidden, "forbidden", "the developer portal needs single sign-on with an email address"}
	}
	return email, nil
}

// portalTeams lists the teams where the operator holds keys.self.
func (s *Server) portalTeams(r *http.Request) ([]*store.Team, error) {
	op := operatorFrom(r.Context())
	seen := map[string]bool{}
	var out []*store.Team
	add := func(t *store.Team) {
		if t != nil && !seen[t.ID] && op.can(config.PermKeysSelf, t.OrgID, t.ID) {
			seen[t.ID] = true
			out = append(out, t)
		}
	}
	t := s.tenants.Load()
	if t == nil {
		return nil, nil
	}
	for _, g := range op.Grants {
		if g.Role != config.RoleOwner && !slices.Contains(op.permissions(g.Role), config.PermKeysSelf) {
			continue
		}
		for orgID, teams := range t.teams {
			if g.Role != config.RoleOwner && g.OrgID != "" && g.OrgID != orgID {
				continue
			}
			if g.TeamID != "" {
				add(teams[g.TeamID])
				continue
			}
			for _, team := range teams {
				add(team)
			}
		}
	}
	slices.SortFunc(out, func(a, b *store.Team) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

type portalModel struct {
	Name string `json:"name"`
}

type portalMCPServer struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Access is allowed, available (the team can ask) or blocked (the
	// organization does not allow it).
	Access  string `json:"access"`
	Pending bool   `json:"pending,omitempty"`
}

type portalUsage struct {
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

type portalTeam struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	OrgID      string            `json:"org_id"`
	OrgName    string            `json:"org_name"`
	Models     []portalModel     `json:"models"`
	MCPServers []portalMCPServer `json:"mcp_servers"`
	Budget     *policy.Budget    `json:"budget,omitempty"`
	RPM        int64             `json:"rpm,omitempty"`
	TPM        int64             `json:"tpm,omitempty"`
	Keys       []*store.Key      `json:"keys"`
	// Usage is this month's use of the person's own keys in the team.
	Usage []portalUsage `json:"usage"`
}

// portal returns, for each of the person's teams, what it may use, the
// person's keys and their use this month.
func (s *Server) portal(w http.ResponseWriter, r *http.Request) error {
	owner, err := portalOwner(r)
	if err != nil {
		return err
	}
	teams, err := s.portalTeams(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	cfg := s.rt.Load().cfg
	var pending []*store.MCPAccessRequest
	if len(teams) > 0 {
		if pending, err = s.store.ListMCPAccessRequests(ctx, store.MCPRequestFilter{Status: "pending", TeamIDs: teamIDs(teams), Limit: 500}); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := make([]portalTeam, 0, len(teams))
	for _, team := range teams {
		org, err := s.store.GetOrg(ctx, team.OrgID)
		if err != nil {
			return err
		}
		p := &auth.Principal{OrgID: org.ID, TeamID: team.ID, Policies: [3]policy.Policy{org.Policy, team.Policy, {}}}
		pt := portalTeam{ID: team.ID, Name: team.Name, OrgID: org.ID, OrgName: org.Name, Models: []portalModel{}, MCPServers: []portalMCPServer{}, Usage: []portalUsage{}}
		for _, name := range slices.Sorted(maps.Keys(cfg.Models)) {
			if p.AllowsModel(name) {
				pt.Models = append(pt.Models, portalModel{Name: name})
			}
		}
		for _, name := range slices.Sorted(maps.Keys(cfg.MCP.VirtualServers)) {
			ms := portalMCPServer{Name: name, Description: cfg.MCP.VirtualServers[name].Description, Access: teamAccess(org, team, name)}
			for _, q := range pending {
				if q.TeamID == team.ID && q.Server == name {
					ms.Pending = true
				}
			}
			pt.MCPServers = append(pt.MCPServers, ms)
		}
		// The tightest limits of the organization and the team apply.
		for _, pol := range []policy.Policy{org.Policy, team.Policy} {
			if pol.Budget != nil && (pt.Budget == nil || pol.Budget.AmountUSD < pt.Budget.AmountUSD) {
				pt.Budget = pol.Budget
			}
			if pol.RPM > 0 && (pt.RPM == 0 || pol.RPM < pt.RPM) {
				pt.RPM = pol.RPM
			}
			if pol.TPM > 0 && (pt.TPM == 0 || pol.TPM < pt.TPM) {
				pt.TPM = pol.TPM
			}
		}
		keys, _, err := s.store.ListAllKeys(ctx, store.KeyFilter{TeamID: team.ID, Owner: owner}, store.Page{Limit: 200})
		if err != nil {
			return err
		}
		pt.Keys = keys
		byModel := map[string]*portalUsage{}
		for _, k := range keys {
			rows, err := s.store.QueryUsage(ctx, store.UsageQuery{OrgID: org.ID, TeamID: team.ID, KeyID: k.ID, From: from, To: now.Add(time.Second), GroupBy: "model"})
			if err != nil {
				return err
			}
			for _, row := range rows {
				u := byModel[row.Group]
				if u == nil {
					u = &portalUsage{Model: row.Group}
					byModel[row.Group] = u
				}
				u.Requests += row.Requests
				u.Tokens += row.InputTokens + row.OutputTokens
				u.CostUSD += row.CostUSD
			}
		}
		for _, m := range slices.Sorted(maps.Keys(byModel)) {
			pt.Usage = append(pt.Usage, *byModel[m])
		}
		out = append(out, pt)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"owner": owner, "teams": out, "mcp_connections_url": myConnectionsURL(cfg),
		"limits": map[string]any{"max_keys_per_user": cfg.SSO.Portal.MaxKeysPerUser, "max_key_ttl": duration(cfg.SSO.Portal.MaxKeyTTL)},
	})
	return nil
}

func teamIDs(teams []*store.Team) []string {
	ids := make([]string, len(teams))
	for i, t := range teams {
		ids[i] = t.ID
	}
	return ids
}

// portalTeam loads a team the operator may use the portal for.
func (s *Server) portalTeam(r *http.Request, teamID string) (*store.Team, error) {
	team, err := s.store.GetTeam(r.Context(), teamID)
	if err != nil {
		return nil, err
	}
	if err := allow(r, config.PermKeysSelf, team.OrgID, team.ID); err != nil {
		return nil, err
	}
	return team, nil
}

// portalKey loads a key the operator owns, on a team they may use the
// portal for. Other keys are not found, so their existence is not told.
func (s *Server) portalKey(r *http.Request) (*store.Key, error) {
	owner, err := portalOwner(r)
	if err != nil {
		return nil, err
	}
	k, err := s.store.GetKey(r.Context(), r.PathValue("key_id"))
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(k.Owner, owner) || !operatorFrom(r.Context()).can(config.PermKeysSelf, k.OrgID, k.TeamID) {
		return nil, store.ErrNotFound
	}
	return k, nil
}

// portalCreateKey creates a key the person owns, with no policy of its
// own and an expiry within sso.portal.max_key_ttl.
func (s *Server) portalCreateKey(w http.ResponseWriter, r *http.Request) error {
	owner, err := portalOwner(r)
	if err != nil {
		return err
	}
	var body struct {
		Name      string `json:"name"`
		ExpiresIn string `json:"expires_in"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	team, err := s.portalTeam(r, r.PathValue("team_id"))
	if err != nil {
		return err
	}
	limits := s.rt.Load().cfg.SSO.Portal
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 100 {
		return badRequest("name is required, at most 100 characters")
	}
	ttl := limits.MaxKeyTTL.Std()
	if body.ExpiresIn != "" {
		if ttl, err = config.ParseDuration(body.ExpiresIn); err != nil {
			return badRequest("expires_in: %v", err)
		}
	}
	if ttl < time.Hour || ttl > limits.MaxKeyTTL.Std() {
		return badRequest("expires_in is between 1h and %s", duration(limits.MaxKeyTTL))
	}
	active, _, err := s.store.ListAllKeys(r.Context(), store.KeyFilter{Owner: owner, Status: "active"}, store.Page{Limit: limits.MaxKeysPerUser + 1})
	if err != nil {
		return err
	}
	if len(active) >= limits.MaxKeysPerUser {
		return &apiError{http.StatusConflict, "key_limit", "you have " + strconv.Itoa(len(active)) + " active keys, the most allowed; revoke one first"}
	}
	expires := time.Now().Add(ttl).UTC().Truncate(time.Second)
	k := &store.Key{TeamID: team.ID, Name: body.Name, Owner: owner, ExpiresAt: &expires}
	secret, prefix, hash, err := auth.NewKey()
	if err != nil {
		return err
	}
	k.Prefix, k.SecretHash = prefix, hash
	if err := s.store.CreateKey(r.Context(), k); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "key.create", "key", k.ID, k.OrgID, map[string]any{"after": k, "via": "portal"}); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, struct {
		*store.Key
		Secret string `json:"key"`
	}{k, secret})
	return nil
}

// portalRotateKey gives one of the person's keys a new secret; the old
// one works for the grace period, at most portalMaxGrace.
func (s *Server) portalRotateKey(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Grace string `json:"grace"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return err
		}
	}
	grace := time.Hour
	if body.Grace != "" {
		var err error
		if grace, err = config.ParseDuration(body.Grace); err != nil {
			return badRequest("grace: %v", err)
		}
	}
	if grace < 0 || grace > portalMaxGrace {
		return badRequest("grace is between 0 and 7d")
	}
	k, err := s.portalKey(r)
	if err != nil {
		return err
	}
	if k.RevokedAt != nil {
		return &apiError{http.StatusConflict, "conflict", "key is revoked"}
	}
	secret, prefix, hash, err := auth.NewKey()
	if err != nil {
		return err
	}
	until := time.Now().Add(grace)
	if err := s.store.RotateKey(r.Context(), k.ID, prefix, hash, until); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "key.rotate", "key", k.ID, k.OrgID, map[string]any{"previous_prefix": k.Prefix, "prefix": prefix, "previous_expires_at": until.UTC(), "via": "portal"}); err != nil {
		return err
	}
	if k, err = s.store.GetKey(r.Context(), k.ID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, struct {
		*store.Key
		Secret string `json:"key"`
	}{k, secret})
	return nil
}

// portalRevokeKey revokes one of the person's keys at once.
func (s *Server) portalRevokeKey(w http.ResponseWriter, r *http.Request) error {
	k, err := s.portalKey(r)
	if err != nil {
		return err
	}
	if err := s.store.RevokeKey(r.Context(), k.ID); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "key.revoke", "key", k.ID, k.OrgID, map[string]any{"via": "portal"}); err != nil {
		return err
	}
	if k, err = s.store.GetKey(r.Context(), k.ID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, k)
	return nil
}

// portalRequestAccess asks the team's admins for a virtual MCP server,
// as the MCP catalog does.
func (s *Server) portalRequestAccess(w http.ResponseWriter, r *http.Request) error {
	owner, err := portalOwner(r)
	if err != nil {
		return err
	}
	var body struct {
		Server string `json:"server"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Server == "" || len(body.Reason) > 2000 {
		return badRequest("server is required, and reason may be at most 2000 characters")
	}
	if _, ok := s.rt.Load().cfg.MCP.VirtualServers[body.Server]; !ok {
		return &apiError{http.StatusNotFound, "not_found", "no virtual MCP server " + body.Server}
	}
	team, err := s.portalTeam(r, r.PathValue("team_id"))
	if err != nil {
		return err
	}
	org, err := s.store.GetOrg(r.Context(), team.OrgID)
	if err != nil {
		return err
	}
	switch teamAccess(org, team, body.Server) {
	case accessAllowed:
		return &apiError{http.StatusConflict, "conflict", team.Name + " may already use " + body.Server}
	case accessOrgBlocked:
		return &apiError{http.StatusForbidden, "forbidden", org.Name + " does not allow " + body.Server}
	}
	q := &store.MCPAccessRequest{OrgID: org.ID, TeamID: team.ID, Server: body.Server, Reason: body.Reason, RequestedBy: owner}
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
