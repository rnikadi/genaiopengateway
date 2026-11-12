package server

// "Why was this denied?" (docs/why-denied.md): the gateway records each
// model request it refuses, and explains, for a key and a model or an MCP
// tool, every check a request goes through and which one says no.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/authz"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/limits"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/router"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// deniedCodes are the refusals worth explaining: the request was well
// formed and its key valid, but a policy, limit or guardrail said no.
var deniedCodes = map[string]bool{
	codeModelNotAllowed: true, codePolicyDenied: true, codeResidency: true,
	limits.CodeRateLimited: true, limits.CodeBudget: true, limits.CodeUnavailable: true,
	codeGuardrailBlocked: true,
}

// maxDenialReason bounds the stored message.
const maxDenialReason = 500

// recordDenial keeps a refused model request for the explainer.
func (s *Server) recordDenial(rec *requestLog) {
	if s.denials == nil || rec.KeyID == "" || rec.RequestID == "" || !deniedCodes[rec.ErrorCode] {
		return
	}
	reason := rec.ErrorMessage
	if len(reason) > maxDenialReason {
		reason = reason[:maxDenialReason]
	}
	s.denials.Add(store.Denial{
		RequestID: rec.RequestID, Time: time.Now().UTC(), OrgID: rec.OrgID, TeamID: rec.TeamID, KeyID: rec.KeyID,
		Endpoint: rec.Endpoint, Model: rec.Model, Status: rec.Status, Code: rec.ErrorCode, Reason: reason,
	})
}

func (s *Server) explainRoutes(h func(string, func(http.ResponseWriter, *http.Request) error)) {
	h("POST /admin/v1/explain", s.explain)
	h("GET /admin/v1/denials", s.listDenials)
}

// Check results.
const (
	checkPass = "pass"
	checkFail = "fail"
	checkWarn = "warn"
	checkInfo = "info"
)

// explainCheck is one step a request goes through.
type explainCheck struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Fix says what would change the answer, when it fails.
	Fix string `json:"fix,omitempty"`
	// Link is the admin UI page where the setting is.
	Link string `json:"link,omitempty"`
}

type explainKey struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	Owner    string `json:"owner,omitempty"`
	OrgID    string `json:"org_id,omitempty"`
	OrgName  string `json:"org_name,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
	TeamName string `json:"team_name,omitempty"`
}

type explanation struct {
	Allowed bool       `json:"allowed"`
	Summary string     `json:"summary"`
	Key     explainKey `json:"key"`
	Model   string     `json:"model,omitempty"`
	Action  string     `json:"action,omitempty"`
	Server  string     `json:"server,omitempty"`
	Tool    string     `json:"tool,omitempty"`
	// Denial is the recorded refusal, when asked about a request.
	Denial *store.Denial `json:"denial,omitempty"`
	// Guardrails lists what guardrails did to that request.
	Guardrails []*store.GuardrailEvent `json:"guardrail_events,omitempty"`
	Checks     []explainCheck          `json:"checks"`
}

// explainSubject is a key and what it is checked against.
type explainSubject struct {
	key     *store.Key
	p       *auth.Principal
	org     *store.Org
	team    *store.Team
	revoked bool
}

// explainSubjectFor loads a key the operator may explain: one they may
// read, or one of their own portal keys. Others are not found.
func (s *Server) explainSubjectFor(r *http.Request, keyID string) (*explainSubject, error) {
	ctx := r.Context()
	k, err := s.store.GetKey(ctx, keyID)
	if errors.Is(err, store.ErrNotFound) {
		// A key from the configuration file.
		if p, ok := s.keys.Load().ByKeyID(keyID); ok && p.OrgID == "" {
			if err := requireOwner(r); err != nil {
				return nil, store.ErrNotFound
			}
			return &explainSubject{key: &store.Key{ID: p.KeyID, Name: p.KeyName, Prefix: p.KeyPrefix, Policy: p.Policies[2], ExpiresAt: p.ExpiresAt}, p: p}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	op := operatorFrom(ctx)
	own := k.Owner != "" && op.Email != "" && strings.EqualFold(k.Owner, op.Email) && op.can(config.PermKeysSelf, k.OrgID, k.TeamID)
	if !own && !op.can(config.PermKeysRead, k.OrgID, k.TeamID) {
		return nil, store.ErrNotFound
	}
	org, err := s.store.GetOrg(ctx, k.OrgID)
	if err != nil {
		return nil, err
	}
	team, err := s.store.GetTeam(ctx, k.TeamID)
	if err != nil {
		return nil, err
	}
	p := &auth.Principal{
		OrgID: k.OrgID, TeamID: k.TeamID, KeyID: k.ID, KeyName: k.Name, KeyPrefix: k.Prefix, Owner: k.Owner,
		Policies: [3]policy.Policy{org.Policy, team.Policy, k.Policy}, ExpiresAt: k.ExpiresAt,
	}
	return &explainSubject{key: k, p: p, org: org, team: team, revoked: k.RevokedAt != nil}, nil
}

// explain answers whether a key may make a request now, and why not.
func (s *Server) explain(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		RequestID string `json:"request_id"`
		KeyID     string `json:"key_id"`
		Model     string `json:"model"`
		Action    string `json:"action"`
		Server    string `json:"server"`
		Tool      string `json:"tool"`
		User      string `json:"user"`
		Stream    bool   `json:"stream"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	ctx := r.Context()
	var denial *store.Denial
	if body.RequestID != "" {
		d, err := s.store.GetDenial(ctx, body.RequestID)
		if errors.Is(err, store.ErrNotFound) {
			return &apiError{http.StatusNotFound, "not_found", "the gateway kept no refusal for this request: it was served, is older than store.usage_retention, or was refused before its key was known"}
		}
		if err != nil {
			return err
		}
		denial = d
		body.KeyID, body.Model, body.Action, body.Server, body.Tool = d.KeyID, d.Model, d.Endpoint, "", ""
	}
	if body.KeyID == "" {
		return badRequest("key_id or request_id is required")
	}
	if (body.Model == "") == (body.Server == "") {
		return badRequest("give a model, or an MCP server and optionally a tool")
	}
	if body.Action == "" && body.Model != "" {
		body.Action = string(provider.ChatCompletions)
	}
	subj, err := s.explainSubjectFor(r, body.KeyID)
	if errors.Is(err, store.ErrNotFound) {
		return &apiError{http.StatusNotFound, "not_found", "no such key"}
	}
	if err != nil {
		return err
	}
	ex := &explanation{Key: explainKey{ID: subj.key.ID, Name: subj.key.Name, Prefix: subj.key.Prefix, Owner: subj.key.Owner, OrgID: subj.key.OrgID, TeamID: subj.key.TeamID},
		Model: body.Model, Action: body.Action, Server: body.Server, Tool: body.Tool, Denial: denial, Checks: []explainCheck{}}
	if subj.org != nil {
		ex.Key.OrgName, ex.Key.TeamName = subj.org.Name, subj.team.Name
	}
	if denial != nil {
		events, _, err := s.store.ListGuardrailEvents(ctx, store.GuardrailEventQuery{OrgID: denial.OrgID, RequestID: denial.RequestID}, store.Page{Limit: 50})
		if err != nil {
			return err
		}
		ex.Guardrails = events
	}

	s.explainKeyChecks(ex, subj)
	if body.Model != "" {
		s.explainModel(ctx, ex, subj, body.Action, body.User, body.Stream)
	} else {
		s.explainMCP(ctx, ex, subj, body.User)
	}
	ex.Allowed = !slices.ContainsFunc(ex.Checks, func(c explainCheck) bool { return c.Status == checkFail })
	ex.Summary = summary(ex)
	writeJSON(w, http.StatusOK, ex)
	return nil
}

func summary(ex *explanation) string {
	what := "model " + ex.Model
	if ex.Server != "" {
		what = "MCP server " + ex.Server
		if ex.Tool != "" {
			what = "tool " + ex.Tool + " on MCP server " + ex.Server
		}
	}
	if ex.Allowed {
		return fmt.Sprintf("Key %s may use %s now.", ex.Key.Name, what)
	}
	for _, c := range ex.Checks {
		if c.Status == checkFail {
			return fmt.Sprintf("Key %s may not use %s: %s", ex.Key.Name, what, c.Detail)
		}
	}
	return ""
}

func orgLink(id string) string  { return "#/orgs/" + id }
func teamLink(id string) string { return "#/teams/" + id }

// policyLink is where the policy of a level is edited.
func (subj *explainSubject) policyLink(level int) string {
	switch {
	case subj.org == nil:
		return "#/settings/startup"
	case level == 0:
		return orgLink(subj.org.ID)
	}
	return teamLink(subj.team.ID)
}

var levelTitles = [3]string{"organization", "team", "key"}

func (s *Server) explainKeyChecks(ex *explanation, subj *explainSubject) {
	k := subj.key
	c := explainCheck{ID: "key", Label: "The key works", Status: checkPass, Detail: "The key is active."}
	switch {
	case subj.revoked:
		c.Status, c.Detail, c.Fix = checkFail, "the key was revoked "+k.RevokedAt.UTC().Format(time.RFC3339)+".", "Create a new key; a revoked key cannot be restored."
	case subj.p.Expired(time.Now()):
		c.Status, c.Detail, c.Fix = checkFail, "the key expired "+k.ExpiresAt.UTC().Format(time.RFC3339)+".", "Create a new key, or rotate this one before it expires next time."
	case k.ExpiresAt != nil && time.Until(*k.ExpiresAt) < 7*24*time.Hour:
		c.Status, c.Detail = checkWarn, "The key works, and expires "+k.ExpiresAt.UTC().Format(time.RFC3339)+"."
	}
	if subj.team != nil {
		c.Link = teamLink(subj.team.ID)
	}
	ex.Checks = append(ex.Checks, c)
}

func (s *Server) explainModel(ctx context.Context, ex *explanation, subj *explainSubject, action, user string, stream bool) {
	rt := s.rt.Load()
	p := subj.p
	model := ex.Model

	c := explainCheck{ID: "model", Label: "The model exists", Status: checkPass, Detail: "Model " + model + " is configured.", Link: "#/models"}
	if !rt.router.Has(model) {
		c.Status, c.Detail, c.Fix = checkFail, "no model named "+model+" is configured.", "Check the name, or add the model on the Models page."
	}
	ex.Checks = append(ex.Checks, c)

	c = explainCheck{ID: "allowed_models", Label: "Allowed models", Status: checkPass, Detail: "Every level allows " + model + "."}
	for i, pol := range p.Policies {
		if !pol.AllowsModel(model) {
			c.Status, c.Link = checkFail, subj.policyLink(i)
			c.Detail = fmt.Sprintf("the %s's allowed models (%s) do not include %s.", levelTitles[i], strings.Join(pol.AllowedModels, ", "), model)
			c.Fix = fmt.Sprintf("Add %s to the %s's allowed models, or use one of them.", model, levelTitles[i])
			if i == 2 {
				c.Fix = "Edit the key's allowed models, or use one of them."
			}
			break
		}
	}
	ex.Checks = append(ex.Checks, c)

	ex.Checks = append(ex.Checks, s.explainCedar(subj, authz.Request{
		OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID, KeyName: p.KeyName, Action: action, Model: model, User: user, Stream: stream,
	}))

	if rt.router.Has(model) {
		c = explainCheck{ID: "residency", Label: "Data residency", Status: checkPass, Detail: "A target of the model serves this endpoint.", Link: "#/models"}
		levels := p.Residency()
		if len(levels) > 0 {
			c.Detail = "A target of the model is in a residency every level allows."
		}
		_, err := rt.router.Targets(router.WithResidency(ctx, levels), provider.Endpoint(action), model)
		switch {
		case errors.Is(err, router.ErrResidency):
			c.Status, c.Detail = checkFail, "no target of "+model+" is in a residency the key may use ("+residencyText(p, subj)+")."
			c.Fix = "Add a target in an allowed residency to the model, or widen the residency of the level that pins it."
		case errors.Is(err, router.ErrUnsupported):
			c.ID, c.Label, c.Status = "endpoint", "The model serves this endpoint", checkFail
			c.Detail, c.Fix = "no target of "+model+" serves "+action+".", "Use another model, or add a target whose provider serves "+action+"."
		}
		ex.Checks = append(ex.Checks, c)
	}

	ex.Checks = append(ex.Checks, s.explainLimits(ctx, subj)...)

	names := p.Guardrails()
	c = explainCheck{ID: "guardrails", Label: "Guardrails", Status: checkInfo, Link: "#/guardrails"}
	if len(names) == 0 {
		c.Detail = "No guardrails apply to this key."
	} else {
		c.Detail = "These guardrails check each request's content, so one may still be refused: " + strings.Join(names, ", ") + "."
	}
	if ex.Denial != nil {
		for _, e := range ex.Guardrails {
			if e.Action == "block" {
				c.Status, c.Detail = checkFail, fmt.Sprintf("guardrail %s blocked the request at the %s stage", e.Guardrail, e.Stage)
				if e.Reason != "" {
					c.Detail += ": " + e.Reason
				}
				c.Fix = "Change the request, or the guardrail's settings if it should have passed."
				break
			}
		}
	}
	ex.Checks = append(ex.Checks, c)
}

func residencyText(p *auth.Principal, subj *explainSubject) string {
	var parts []string
	for i, pol := range p.Policies {
		if len(pol.Residency) > 0 {
			parts = append(parts, levelTitles[i]+": "+strings.Join(pol.Residency, ", "))
		}
	}
	_ = subj
	return strings.Join(parts, "; ")
}

// explainCedar evaluates the organization's Cedar policies.
func (s *Server) explainCedar(subj *explainSubject, req authz.Request) explainCheck {
	c := explainCheck{ID: "cedar", Label: "Cedar policies", Status: checkPass}
	if subj.org != nil {
		c.Link = orgLink(subj.org.ID)
	}
	m := s.cedar.Load()
	if m == nil || (*m)[req.OrgID] == nil {
		c.Status, c.Detail = checkInfo, "The organization has no Cedar policies."
		return c
	}
	if denied := s.cedarDenied(req); denied != "" {
		c.Status, c.Detail = checkFail, denied
		c.Fix = "Change the organization's Cedar policies; Check on the Cedar panel tries a draft first."
		return c
	}
	c.Detail = "The organization's Cedar policies permit it."
	if req.User == "" {
		c.Detail += " Policies about users were checked without one."
	}
	return c
}

// explainLimits shows each level's rate limits and budget as they stand.
func (s *Server) explainLimits(ctx context.Context, subj *explainSubject) []explainCheck {
	usage, err := s.limiter.Peek(ctx, subj.p)
	if err != nil {
		return []explainCheck{{ID: "limits", Label: "Rate limits and budgets", Status: checkWarn, Detail: "The limiter could not be read: " + err.Error()}}
	}
	rate := explainCheck{ID: "rate_limits", Label: "Rate limits", Status: checkPass}
	budget := explainCheck{ID: "budgets", Label: "Budgets", Status: checkPass}
	var rates, budgets []string
	for i, u := range usage {
		if u.RPM > 0 {
			rates = append(rates, fmt.Sprintf("%s %d of %d requests this minute", u.Level, u.RPMUsed, u.RPM))
			if u.RPMUsed >= u.RPM && rate.Status == checkPass {
				rate.Status, rate.Link = checkFail, subj.policyLink(i)
				rate.Detail = fmt.Sprintf("the %s's limit of %d requests a minute is reached; it resets in %ds.", u.Level, u.RPM, limits.RetryAfterSeconds(u.Reset))
				rate.Fix = "Wait for the next minute, or raise the " + u.Level + "'s requests per minute."
			}
		}
		if u.TPM > 0 {
			rates = append(rates, fmt.Sprintf("%s %d of %d tokens this minute", u.Level, u.TPMUsed, u.TPM))
			if u.TPMUsed >= u.TPM && rate.Status == checkPass {
				rate.Status, rate.Link = checkFail, subj.policyLink(i)
				rate.Detail = fmt.Sprintf("the %s's limit of %d tokens a minute is reached; it resets in %ds.", u.Level, u.TPM, limits.RetryAfterSeconds(u.Reset))
				rate.Fix = "Wait for the next minute, or raise the " + u.Level + "'s tokens per minute."
			}
		}
		if b := u.Budget; b != nil {
			budgets = append(budgets, fmt.Sprintf("%s $%s of $%s %s", u.Level, money(u.Spent), money(b.AmountUSD), b.Period))
			if u.Spent >= b.AmountUSD && budget.Status == checkPass {
				budget.Status, budget.Link = checkFail, subj.policyLink(i)
				budget.Detail = fmt.Sprintf("the %s's %s budget of $%s is spent ($%s).", u.Level, b.Period, money(b.AmountUSD), money(u.Spent))
				budget.Fix = "Raise the " + u.Level + "'s budget, or wait for the next period."
				if b.Period == policy.PeriodTotal {
					budget.Fix = "Raise the " + u.Level + "'s budget; a total budget does not reset."
				}
			} else if u.Spent >= 0.9*b.AmountUSD && budget.Status == checkPass {
				budget.Status = checkWarn
			}
		}
	}
	if rate.Status == checkPass {
		rate.Detail = "No rate limits apply."
		if len(rates) > 0 {
			rate.Detail = "Within limits: " + strings.Join(rates, "; ") + "."
		}
	}
	if budget.Status != checkFail {
		budget.Detail = "No budgets apply."
		if len(budgets) > 0 {
			budget.Detail = "Spent so far: " + strings.Join(budgets, "; ") + "."
		}
	}
	return []explainCheck{rate, budget}
}

func money(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func (s *Server) explainMCP(ctx context.Context, ex *explanation, subj *explainSubject, user string) {
	rt := s.rt.Load()
	server, tool := ex.Server, ex.Tool
	vs, ok := rt.cfg.MCP.VirtualServers[server]
	c := explainCheck{ID: "mcp_server", Label: "The MCP server exists", Status: checkPass, Detail: "MCP server " + server + " is configured.", Link: "#/mcp?tab=servers"}
	if !ok {
		c.Status, c.Detail, c.Fix = checkFail, "no virtual MCP server named "+server+" is configured.", "Check the name on the MCP page."
		ex.Checks = append(ex.Checks, c)
		return
	}
	ex.Checks = append(ex.Checks, c)

	c = explainCheck{ID: "allowed_mcp_servers", Label: "Allowed MCP servers", Status: checkPass, Detail: "Every level allows " + server + "."}
	for i, pol := range subj.p.Policies {
		if !pol.AllowsMCPServer(server) {
			c.Status, c.Link = checkFail, subj.policyLink(i)
			c.Detail = fmt.Sprintf("the %s's allowed MCP servers (%s) do not include %s.", levelTitles[i], strings.Join(pol.AllowedMCPServers, ", "), server)
			c.Fix = "Request access from the MCP catalog, or ask an admin to add it to the " + levelTitles[i] + "'s allowed MCP servers."
			break
		}
	}
	ex.Checks = append(ex.Checks, c)

	if tool == "" {
		return
	}
	ex.Checks = append(ex.Checks, s.explainTool(ctx, rt, vs, server, tool))
	p := subj.p
	ex.Checks = append(ex.Checks, s.explainCedar(subj, authz.Request{
		OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID, KeyName: p.KeyName, Server: server, Tool: tool, User: user,
	}))
}

// explainTool finds the upstream tool behind a virtual server's tool name
// and whether an approval hides it.
func (s *Server) explainTool(ctx context.Context, rt *runtime, vs config.VirtualServer, server, tool string) explainCheck {
	c := explainCheck{ID: "tool", Label: "The tool is exposed and approved", Status: checkPass, Link: "#/mcp"}
	for _, ref := range vs.Servers {
		name, ok := strings.CutPrefix(tool, ref.NamePrefix())
		if !ok {
			continue
		}
		if ref.Tools != nil && !slices.Contains(ref.Tools, name) {
			continue
		}
		up := rt.cfg.MCP.Servers[ref.Server]
		if up.PinTools == config.PinOff {
			c.Detail = fmt.Sprintf("Tool %s of upstream server %s; its definitions are not pinned.", name, ref.Server)
			return c
		}
		pins, err := s.mcp.Tools(ctx, ref.Server, "")
		if err != nil {
			c.Status, c.Detail = checkWarn, "the tool approvals could not be read: "+err.Error()
			return c
		}
		for _, pin := range pins {
			if pin.Tool != name {
				continue
			}
			switch pin.Status {
			case store.PinChanged:
				c.Status, c.Detail = checkFail, fmt.Sprintf("tool %s of %s changed since it was approved, so it is hidden until an owner approves the new definition.", name, ref.Server)
				c.Fix = "Review the change under Needs review on the MCP page."
			case store.PinPending:
				if up.PinTools == config.PinStrict {
					c.Status, c.Detail = checkFail, fmt.Sprintf("tool %s of %s is new and %s requires approval of new tools.", name, ref.Server, ref.Server)
					c.Fix = "Approve it under Needs review on the MCP page."
				} else {
					c.Detail = fmt.Sprintf("Tool %s of %s; it has not been reviewed, which this server does not require.", name, ref.Server)
				}
			default:
				c.Detail = fmt.Sprintf("Tool %s of %s, approved.", name, ref.Server)
			}
			return c
		}
		c.Status, c.Detail = checkWarn, fmt.Sprintf("the gateway has not seen tool %s on %s; the server may not offer it.", name, ref.Server)
		return c
	}
	c.Status, c.Detail = checkFail, fmt.Sprintf("virtual server %s exposes no tool named %s.", server, tool)
	c.Fix = "Check the tool's name with tools/list, or add it to the virtual server."
	return c
}

// listDenials lists recorded refusals the operator may read.
func (s *Server) listDenials(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if err := s.readScope(r, config.PermUsageRead, q.Get("org_id"), q.Get("team_id"), q.Get("key_id")); err != nil {
		return err
	}
	dq := store.DenialQuery{OrgID: q.Get("org_id"), TeamID: q.Get("team_id"), KeyID: q.Get("key_id"), Code: q.Get("code")}
	for name, dst := range map[string]*time.Time{"from": &dq.From, "to": &dq.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return badRequest("%s must be an RFC 3339 time", name)
			}
			*dst = t
		}
	}
	rows, more, err := s.store.ListDenials(r.Context(), dq, pg)
	if err != nil {
		return err
	}
	return writeList(w, rows, more)
}
