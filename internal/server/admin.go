package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// adminActor names the admin token in the audit log.
const adminActor = "admin-token"

// maxAdminBody bounds admin request bodies.
const maxAdminBody = 1 << 20

func (s *Server) adminRoutes(mux *http.ServeMux) {
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request) error) {
		mux.Handle(pattern, s.adminAuth(fn))
	}
	h("POST /admin/v1/orgs", s.createOrg)
	h("GET /admin/v1/orgs", s.listOrgs)
	h("GET /admin/v1/orgs/{org_id}", s.getOrg)
	h("PATCH /admin/v1/orgs/{org_id}", s.updateOrg)
	h("DELETE /admin/v1/orgs/{org_id}", s.deleteOrg)
	h("POST /admin/v1/orgs/{org_id}/teams", s.createTeam)
	h("GET /admin/v1/orgs/{org_id}/teams", s.listTeams)
	h("GET /admin/v1/teams/{team_id}", s.getTeam)
	h("PATCH /admin/v1/teams/{team_id}", s.updateTeam)
	h("DELETE /admin/v1/teams/{team_id}", s.deleteTeam)
	h("GET /admin/v1/teams/{team_id}/credentials", s.listTeamCredentials)
	h("PUT /admin/v1/teams/{team_id}/credentials/{provider}", s.putTeamCredential)
	h("DELETE /admin/v1/teams/{team_id}/credentials/{provider}", s.deleteTeamCredential)
	h("POST /admin/v1/teams/{team_id}/keys", s.createKey)
	h("GET /admin/v1/teams/{team_id}/keys", s.listKeys)
	h("GET /admin/v1/keys/{key_id}", s.getKey)
	h("PATCH /admin/v1/keys/{key_id}", s.updateKey)
	h("POST /admin/v1/keys/{key_id}/revoke", s.revokeKey)
	h("POST /admin/v1/keys/{key_id}/rotate", s.rotateKey)
	h("GET /admin/v1/keys", s.listAllKeys)
	h("GET /admin/v1/usage", s.queryUsage)
	h("GET /admin/v1/audit", s.listAudit)
	h("GET /admin/v1/mcp/servers", s.listMCPServers)
	h("GET /admin/v1/mcp/tools", s.listMCPTools)
	h("POST /admin/v1/mcp/servers/{server}/tools/{tool}/approve", s.approveMCPTool)
	h("GET /admin/v1/mcp/calls", s.listMCPCalls)
	h("GET /admin/v1/guardrail-events", s.listGuardrailEvents)
	h("GET /admin/v1/mcp/virtual-servers", s.listVirtualServers)
	h("GET /admin/v1/me", s.me)
	h("GET /admin/v1/models", s.listConfiguredModels)
	h("GET /admin/v1/providers", s.listProviders)
	h("GET /admin/v1/guardrails", s.listGuardrails)
	h("GET /admin/v1/alerts/webhooks", s.listWebhooks)
	h("GET /admin/v1/settings", s.settings)
	h("GET /admin/v1/config", s.listConfig)
	h("PUT /admin/v1/config/{kind}/{name}", s.putConfig)
	h("DELETE /admin/v1/config/{kind}/{name}", s.deleteConfig)
	h("POST /admin/v1/config/{kind}/{name}/reset", s.resetConfig)
	h("POST /admin/v1/playground/chat/completions", s.playground(provider.ChatCompletions))
	h("POST /admin/v1/playground/images/generations", s.playground(provider.ImageGenerations))
	h("POST /admin/v1/playground/audio/speech", s.playground(provider.Speech))
	h("POST /admin/v1/playground/audio/transcriptions", s.playground(provider.Transcriptions))
	h("GET /admin/v1/alerts", s.listAlerts)
	h("POST /admin/v1/alerts/test", s.testAlert)
	h("GET /admin/v1/mcp/connections", s.listMCPConnections)
	h("DELETE /admin/v1/mcp/connections/{server}/{subject}", s.deleteMCPConnection)
	h("GET /admin/v1/requests", s.listRequests)
	h("GET /admin/v1/requests/{request_id}", s.getRequestBody)
	h("DELETE /admin/v1/requests", s.deleteRequestBodies)
	h("GET /admin/v1/batches", s.listAdminBatches)
	h("POST /admin/v1/batches/{id}/cancel", s.cancelAdminBatch)
	h("GET /admin/v1/videos", s.listAdminVideos)
	h("DELETE /admin/v1/sessions", s.deleteSessions)
	h("DELETE /admin/v1/cache", s.clearCache)
}

// apiError is an admin error with an HTTP status.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, "invalid_request", fmt.Sprintf(format, args...)}
}

// adminAuth authenticates the operator and turns handler errors into
// responses. Handlers check roles themselves. The admin API answers 404
// when it is disabled.
func (s *Server) adminAuth(fn func(http.ResponseWriter, *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.store == nil || !s.rt.Load().cfg.AdminEnabled() {
			http.NotFound(w, r)
			return
		}
		op, denied := s.adminOperator(r)
		if denied != nil {
			writeAdminError(w, denied.status, denied.code, denied.msg)
			return
		}
		r = r.WithContext(withOperator(r.Context(), op))
		w.Header().Set("Cache-Control", "no-store")
		err := fn(w, r)
		var ae *apiError
		switch {
		case err == nil:
		case errors.As(err, &ae):
			writeAdminError(w, ae.status, ae.code, ae.msg)
		case errors.Is(err, store.ErrNotFound):
			writeAdminError(w, http.StatusNotFound, "not_found", "not found")
		case errors.Is(err, store.ErrConflict):
			writeAdminError(w, http.StatusConflict, "conflict", err.Error())
		default:
			s.log.Error("admin request failed", "request_id", r.Header.Get("X-Request-Id"), "path", r.URL.Path, "err", err)
			writeAdminError(w, http.StatusInternalServerError, "internal_error", "internal error")
		}
	})
}

func writeAdminError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "code": code}})
}

// decode reads a JSON body strictly: unknown fields are errors.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func page(r *http.Request) (store.Page, error) {
	p := store.Page{After: r.URL.Query().Get("after")}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			return p, badRequest("limit must be between 1 and 500")
		}
		p.Limit = n
	}
	return p, nil
}

func writeList[T any](w http.ResponseWriter, items []T, more bool) error {
	writeJSON(w, http.StatusOK, map[string]any{"data": items, "has_more": more})
	return nil
}

// optional distinguishes an absent JSON field from an explicit null.
type optional[T any] struct {
	Set   bool
	Value *T
}

func (o *optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(b, []byte("null")) {
		o.Value = nil
		return nil
	}
	o.Value = new(T)
	return json.Unmarshal(b, o.Value)
}

// patch is the body of create and update requests for orgs, teams and
// keys. On create, absent fields take their zero values.
type patch struct {
	Name              *string                 `json:"name"`
	AllowedModels     *[]string               `json:"allowed_models"`
	AllowedMCPServers *[]string               `json:"allowed_mcp_servers"`
	RPM               *int64                  `json:"rpm"`
	TPM               *int64                  `json:"tpm"`
	Budget            optional[policy.Budget] `json:"budget"`
	LogBodies         optional[bool]          `json:"log_bodies"`
	Guardrails        *[]string               `json:"guardrails"`
	Cache             optional[policy.Cache]  `json:"cache"`
	ExpiresAt         optional[time.Time]     `json:"expires_at"`
	Owner             *string                 `json:"owner"`
	isKey             bool
}

// applyOwner sets a key's owner from a patch.
func applyOwner(p *patch, k *store.Key) error {
	if p.Owner == nil {
		return nil
	}
	o := strings.TrimSpace(*p.Owner)
	if len(o) > 200 {
		return badRequest("owner is at most 200 characters")
	}
	k.Owner = o
	return nil
}

// checkName validates the name of an organization, team or key.
func checkName(name string) error {
	switch {
	case name == "":
		return badRequest("name is required")
	case utf8.RuneCountInString(name) > maxNameLength:
		return badRequest("name is at most %d characters", maxNameLength)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return badRequest("name must not contain control characters")
	}
	return nil
}

const maxNameLength = 200

// apply updates name and policy in place and validates the result
// against the models currently configured.
func (s *Server) apply(p *patch, name *string, pol *policy.Policy, expires **time.Time) error {
	if p.Name != nil {
		*name = strings.TrimSpace(*p.Name)
	}
	if err := checkName(*name); err != nil {
		return err
	}
	if p.AllowedModels != nil {
		pol.AllowedModels = *p.AllowedModels
	}
	if p.AllowedMCPServers != nil {
		pol.AllowedMCPServers = *p.AllowedMCPServers
	}
	if p.RPM != nil {
		pol.RPM = *p.RPM
	}
	if p.TPM != nil {
		pol.TPM = *p.TPM
	}
	if p.Budget.Set {
		pol.Budget = p.Budget.Value
	}
	if p.LogBodies.Set {
		pol.LogBodies = p.LogBodies.Value
	}
	if p.Guardrails != nil {
		pol.Guardrails = slices.Compact(slices.Sorted(slices.Values(*p.Guardrails)))
	}
	if p.Cache.Set {
		pol.Cache = p.Cache.Value
	}
	if p.Owner != nil && !p.isKey {
		return badRequest("owner applies only to keys")
	}
	if p.ExpiresAt.Set {
		if !p.isKey {
			return badRequest("expires_at applies only to keys")
		}
		if p.ExpiresAt.Value != nil && p.ExpiresAt.Value.Before(time.Now()) {
			return badRequest("expires_at is in the past")
		}
		*expires = p.ExpiresAt.Value
	}
	if err := pol.Validate(); err != nil {
		return badRequest("%v", err)
	}
	cfg := s.rt.Load().cfg
	if pol.Cache != nil && pol.Cache.Mode == policy.CacheSemantic && cfg.Cache.Semantic.Model == "" {
		return badRequest("the semantic cache needs cache.semantic.model in the gateway configuration")
	}
	for _, m := range pol.AllowedModels {
		if _, ok := cfg.Models[m]; !ok {
			return badRequest("allowed model %q is not configured", m)
		}
	}
	for _, v := range pol.AllowedMCPServers {
		if _, ok := cfg.MCP.VirtualServers[v]; !ok {
			return badRequest("allowed MCP server %q is not configured", v)
		}
	}
	for _, g := range pol.Guardrails {
		if _, ok := cfg.Guardrails[g]; !ok {
			return badRequest("guardrail %q is not configured", g)
		}
	}
	return nil
}

// audit records a change and refreshes the key directory so the change
// applies on this replica at once.
func (s *Server) audit(ctx context.Context, action, resourceType, resourceID, orgID string, details any) error {
	raw, _ := json.Marshal(details)
	if err := s.store.AppendAudit(ctx, &store.AuditEntry{
		Actor: operatorFrom(ctx).actor(), Action: action, ResourceType: resourceType, ResourceID: resourceID, OrgID: orgID, Details: raw,
	}); err != nil {
		return err
	}
	if err := s.refresh(ctx); err != nil {
		s.log.Error("refreshing keys after admin change failed", "err", err)
	}
	return nil
}

// --- organizations ---

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	var p patch
	if err := decode(r, &p); err != nil {
		return err
	}
	o := &store.Org{}
	if err := s.apply(&p, &o.Name, &o.Policy, nil); err != nil {
		return err
	}
	if err := s.store.CreateOrg(r.Context(), o); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "org.create", "org", o.ID, o.ID, map[string]any{"after": o}); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, o)
	return nil
}

func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	op := operatorFrom(r.Context())
	if !op.readsAll() {
		// Operators with roles in a few organizations see just those.
		items := []*store.Org{}
		for _, id := range op.orgIDs() {
			o, err := s.store.GetOrg(r.Context(), id)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			items = append(items, o)
		}
		return writeList(w, items, false)
	}
	items, more, err := s.store.ListOrgs(r.Context(), pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}

func (s *Server) getOrg(w http.ResponseWriter, r *http.Request) error {
	if err := read(r, r.PathValue("org_id")); err != nil {
		return err
	}
	o, err := s.store.GetOrg(r.Context(), r.PathValue("org_id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, o)
	return nil
}

func (s *Server) updateOrg(w http.ResponseWriter, r *http.Request) error {
	if err := write(r, r.PathValue("org_id")); err != nil {
		return err
	}
	var p patch
	if err := decode(r, &p); err != nil {
		return err
	}
	o, err := s.store.GetOrg(r.Context(), r.PathValue("org_id"))
	if err != nil {
		return err
	}
	before := *o
	if err := s.apply(&p, &o.Name, &o.Policy, nil); err != nil {
		return err
	}
	// Role mappings find organizations by name (ADR 0029).
	if o.Name != before.Name && !operatorFrom(r.Context()).isOwner() {
		return &apiError{http.StatusForbidden, "forbidden", "only owners rename an organization"}
	}
	if err := s.store.UpdateOrg(r.Context(), o); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "org.update", "org", o.ID, o.ID, map[string]any{"before": before, "after": o}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, o)
	return nil
}

func (s *Server) deleteOrg(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	orgID := r.PathValue("org_id")
	if err := s.store.DeleteOrg(r.Context(), orgID); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "org.delete", "org", orgID, orgID, nil); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- teams ---

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) error {
	if err := write(r, r.PathValue("org_id")); err != nil {
		return err
	}
	var p patch
	if err := decode(r, &p); err != nil {
		return err
	}
	t := &store.Team{OrgID: r.PathValue("org_id")}
	if err := s.apply(&p, &t.Name, &t.Policy, nil); err != nil {
		return err
	}
	if err := s.store.CreateTeam(r.Context(), t); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "team.create", "team", t.ID, t.OrgID, map[string]any{"after": t}); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, t)
	return nil
}

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	if err := read(r, r.PathValue("org_id")); err != nil {
		return err
	}
	if _, err := s.store.GetOrg(r.Context(), r.PathValue("org_id")); err != nil {
		return err
	}
	items, more, err := s.store.ListTeams(r.Context(), r.PathValue("org_id"), pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}

func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) error {
	t, err := s.store.GetTeam(r.Context(), r.PathValue("team_id"))
	if err != nil {
		return err
	}
	if err := read(r, t.OrgID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, t)
	return nil
}

func (s *Server) updateTeam(w http.ResponseWriter, r *http.Request) error {
	var p patch
	if err := decode(r, &p); err != nil {
		return err
	}
	t, err := s.store.GetTeam(r.Context(), r.PathValue("team_id"))
	if err != nil {
		return err
	}
	if err := write(r, t.OrgID); err != nil {
		return err
	}
	before := *t
	if err := s.apply(&p, &t.Name, &t.Policy, nil); err != nil {
		return err
	}
	if err := s.store.UpdateTeam(r.Context(), t); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "team.update", "team", t.ID, t.OrgID, map[string]any{"before": before, "after": t}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, t)
	return nil
}

func (s *Server) deleteTeam(w http.ResponseWriter, r *http.Request) error {
	t, err := s.store.GetTeam(r.Context(), r.PathValue("team_id"))
	if err != nil {
		return err
	}
	if err := write(r, t.OrgID); err != nil {
		return err
	}
	if err := s.store.DeleteTeam(r.Context(), t.ID); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "team.delete", "team", t.ID, t.OrgID, nil); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- keys ---

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) error {
	p := patch{isKey: true}
	if err := decode(r, &p); err != nil {
		return err
	}
	t, err := s.store.GetTeam(r.Context(), r.PathValue("team_id"))
	if err != nil {
		return err
	}
	if err := write(r, t.OrgID); err != nil {
		return err
	}
	k := &store.Key{TeamID: t.ID}
	if err := s.apply(&p, &k.Name, &k.Policy, &k.ExpiresAt); err != nil {
		return err
	}
	if err := applyOwner(&p, k); err != nil {
		return err
	}
	secret, prefix, hash, err := auth.NewKey()
	if err != nil {
		return err
	}
	k.Prefix, k.SecretHash = prefix, hash
	if err := s.store.CreateKey(r.Context(), k); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "key.create", "key", k.ID, k.OrgID, map[string]any{"after": k}); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, struct {
		*store.Key
		Secret string `json:"key"`
	}{k, secret})
	return nil
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	t, err := s.store.GetTeam(r.Context(), r.PathValue("team_id"))
	if err != nil {
		return err
	}
	if err := read(r, t.OrgID); err != nil {
		return err
	}
	items, more, err := s.store.ListKeys(r.Context(), r.PathValue("team_id"), pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}

// listAllKeys lists keys across teams, limited to the organizations the
// operator can read (docs/virtual-keys.md).
func (s *Server) listAllKeys(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	f := store.KeyFilter{OrgID: q.Get("org_id"), TeamID: q.Get("team_id"), Query: q.Get("q"), Status: q.Get("status")}
	if f.Status != "" && !slices.Contains(store.KeyStatuses, f.Status) {
		return badRequest("status is one of %s", strings.Join(store.KeyStatuses, ", "))
	}
	if op := operatorFrom(r.Context()); !op.readsAll() {
		f.OrgIDs = op.orgIDs()
		if f.OrgIDs == nil {
			f.OrgIDs = []string{}
		}
	}
	items, more, err := s.store.ListAllKeys(r.Context(), f, pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}

// maxRotationGrace bounds how long a rotated secret keeps working.
const maxRotationGrace = 30 * 24 * time.Hour

// rotateKey gives a key a new secret; the old one works for a grace
// period, so applications can switch over without downtime.
func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Grace *string `json:"grace"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return err
		}
	}
	grace := 24 * time.Hour
	if body.Grace != nil {
		var err error
		if grace, err = config.ParseDuration(*body.Grace); err != nil {
			return badRequest("grace: %v", err)
		}
	}
	if grace < 0 || grace > maxRotationGrace {
		return badRequest("grace is between 0 and 30d")
	}
	k, err := s.store.GetKey(r.Context(), r.PathValue("key_id"))
	if err != nil {
		return err
	}
	if err := write(r, k.OrgID); err != nil {
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
	if err := s.audit(r.Context(), "key.rotate", "key", k.ID, k.OrgID, map[string]any{"previous_prefix": k.Prefix, "prefix": prefix, "previous_expires_at": until.UTC()}); err != nil {
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

func (s *Server) getKey(w http.ResponseWriter, r *http.Request) error {
	k, err := s.store.GetKey(r.Context(), r.PathValue("key_id"))
	if err != nil {
		return err
	}
	if err := read(r, k.OrgID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, k)
	return nil
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) error {
	p := patch{isKey: true}
	if err := decode(r, &p); err != nil {
		return err
	}
	k, err := s.store.GetKey(r.Context(), r.PathValue("key_id"))
	if err != nil {
		return err
	}
	if err := write(r, k.OrgID); err != nil {
		return err
	}
	if k.RevokedAt != nil {
		return &apiError{http.StatusConflict, "conflict", "key is revoked"}
	}
	before := *k
	if err := s.apply(&p, &k.Name, &k.Policy, &k.ExpiresAt); err != nil {
		return err
	}
	if err := applyOwner(&p, k); err != nil {
		return err
	}
	if err := s.store.UpdateKey(r.Context(), k); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "key.update", "key", k.ID, k.OrgID, map[string]any{"before": before, "after": k}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, k)
	return nil
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) error {
	k, err := s.store.GetKey(r.Context(), r.PathValue("key_id"))
	if err != nil {
		return err
	}
	if err := write(r, k.OrgID); err != nil {
		return err
	}
	if err := s.store.RevokeKey(r.Context(), k.ID); err != nil {
		return err
	}
	if err := s.audit(r.Context(), "key.revoke", "key", k.ID, k.OrgID, nil); err != nil {
		return err
	}
	k, err = s.store.GetKey(r.Context(), k.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, k)
	return nil
}

// --- usage and audit ---

func (s *Server) queryUsage(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	if err := s.readScope(r, q.Get("org_id"), q.Get("team_id"), q.Get("key_id")); err != nil {
		return err
	}
	now := time.Now().UTC()
	uq := store.UsageQuery{
		OrgID: q.Get("org_id"), TeamID: q.Get("team_id"), KeyID: q.Get("key_id"),
		From:    time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC),
		To:      now.Add(time.Millisecond),
		GroupBy: q.Get("group_by"),
	}
	if uq.GroupBy == "" {
		uq.GroupBy = "model"
	}
	for name, dst := range map[string]*time.Time{"from": &uq.From, "to": &uq.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return badRequest("%s must be an RFC 3339 time", name)
			}
			*dst = t
		}
	}
	switch uq.GroupBy {
	case "model", "team", "key", "day", "provider", "endpoint":
	default:
		return badRequest("group_by must be model, team, key, day, provider or endpoint")
	}
	rows, err := s.store.QueryUsage(r.Context(), uq)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": uq.From, "to": uq.To, "group_by": uq.GroupBy, "data": rows,
	})
	return nil
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	orgID := r.URL.Query().Get("org_id")
	if err := read(r, orgID); err != nil {
		return err
	}
	items, more, err := s.store.ListAudit(r.Context(), orgID, pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}

// readScope checks that the caller may read data filtered by organization,
// team or key. Without any filter it is data across all organizations.
func (s *Server) readScope(r *http.Request, orgID, teamID, keyID string) error {
	op := operatorFrom(r.Context())
	if op.readsAll() {
		return nil
	}
	ctx := r.Context()
	if keyID != "" {
		k, err := s.store.GetKey(ctx, keyID)
		if err != nil {
			return err
		}
		return read(r, k.OrgID)
	}
	if teamID != "" {
		t, err := s.store.GetTeam(ctx, teamID)
		if err != nil {
			return err
		}
		return read(r, t.OrgID)
	}
	if orgID == "" {
		return &apiError{http.StatusForbidden, "forbidden", "org_id is required: your roles are limited to some organizations"}
	}
	return read(r, orgID)
}

// --- operator and configuration ---

func (s *Server) me(w http.ResponseWriter, r *http.Request) error {
	cfg := s.rt.Load().cfg
	writeJSON(w, http.StatusOK, map[string]any{
		"operator":       operatorFrom(r.Context()),
		"sso":            cfg.SSO.OIDC != nil,
		"body_logging":   cfg.BodyLogging.Enabled,
		"semantic_cache": cfg.Cache.Semantic.Model != "",
		"guardrails":     slices.Sorted(maps.Keys(cfg.Guardrails)),
	})
	return nil
}
