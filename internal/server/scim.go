package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/store"
)

// SCIM 2.0 provisioning (RFC 7643, RFC 7644; docs/scim.md). The identity
// provider creates, updates and removes operators and their groups here,
// so role mappings follow the directory without waiting for a sign-in.

const (
	scimUserSchema  = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimGroupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimListSchema  = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimErrorSchema = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimPatchSchema = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	// scimMaxResults bounds a page of a list.
	scimMaxResults = 200
	scimActor      = "scim"
)

type scimError struct {
	status   int
	scimType string
	detail   string
}

func (e *scimError) Error() string { return e.detail }

func scimBad(scimType, format string, args ...any) *scimError {
	return &scimError{http.StatusBadRequest, scimType, fmt.Sprintf(format, args...)}
}

func writeSCIM(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// scimHandler authenticates the identity provider's bearer token and
// answers errors in SCIM's shape.
func (s *Server) scimHandler(h func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.rt.Load().cfg.SSO.SCIM
		if !cfg.Enabled() || s.store == nil {
			http.NotFound(w, r)
			return
		}
		got := presentedKey(r)
		if r.Header.Get("Authorization") == "" || subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Token.Value())) != 1 {
			writeSCIM(w, http.StatusUnauthorized, map[string]any{"schemas": []string{scimErrorSchema}, "status": "401", "detail": "missing or invalid SCIM token"})
			return
		}
		err := h(w, r)
		if err == nil {
			return
		}
		var se *scimError
		switch {
		case errors.As(err, &se):
		case errors.Is(err, store.ErrNotFound):
			se = &scimError{http.StatusNotFound, "", err.Error()}
		case errors.Is(err, store.ErrConflict):
			se = &scimError{http.StatusConflict, "uniqueness", err.Error()}
		default:
			s.log.Error("SCIM request failed", "path", r.URL.Path, "err", err)
			se = &scimError{http.StatusInternalServerError, "", "internal error"}
		}
		body := map[string]any{"schemas": []string{scimErrorSchema}, "status": strconv.Itoa(se.status), "detail": se.detail}
		if se.scimType != "" {
			body["scimType"] = se.scimType
		}
		writeSCIM(w, se.status, body)
	}
}

func (s *Server) scimRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /scim/v2/ServiceProviderConfig", s.scimHandler(s.scimServiceProviderConfig))
	mux.HandleFunc("GET /scim/v2/ResourceTypes", s.scimHandler(s.scimResourceTypes))
	mux.HandleFunc("GET /scim/v2/Schemas", s.scimHandler(s.scimSchemas))
	mux.HandleFunc("GET /scim/v2/Users", s.scimHandler(s.scimListUsers))
	mux.HandleFunc("POST /scim/v2/Users", s.scimHandler(s.scimCreateUser))
	mux.HandleFunc("GET /scim/v2/Users/{id}", s.scimHandler(s.scimGetUser))
	mux.HandleFunc("PUT /scim/v2/Users/{id}", s.scimHandler(s.scimReplaceUser))
	mux.HandleFunc("PATCH /scim/v2/Users/{id}", s.scimHandler(s.scimPatchUser))
	mux.HandleFunc("DELETE /scim/v2/Users/{id}", s.scimHandler(s.scimDeleteUser))
	mux.HandleFunc("GET /scim/v2/Groups", s.scimHandler(s.scimListGroups))
	mux.HandleFunc("POST /scim/v2/Groups", s.scimHandler(s.scimCreateGroup))
	mux.HandleFunc("GET /scim/v2/Groups/{id}", s.scimHandler(s.scimGetGroup))
	mux.HandleFunc("PUT /scim/v2/Groups/{id}", s.scimHandler(s.scimReplaceGroup))
	mux.HandleFunc("PATCH /scim/v2/Groups/{id}", s.scimHandler(s.scimPatchGroup))
	mux.HandleFunc("DELETE /scim/v2/Groups/{id}", s.scimHandler(s.scimDeleteGroup))
}

// scimBase is the URL of the SCIM endpoints, for meta.location.
func scimBase(r *http.Request) string {
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/scim/v2"
}

func scimMeta(r *http.Request, kind, id string, created, updated time.Time) map[string]any {
	return map[string]any{
		"resourceType": kind, "created": created.Format(time.RFC3339), "lastModified": updated.Format(time.RFC3339),
		"location": scimBase(r) + "/" + kind + "s/" + id,
	}
}

func scimRefs(refs []store.SCIMRef, kind string, r *http.Request) []map[string]string {
	out := make([]map[string]string, len(refs))
	for i, ref := range refs {
		out[i] = map[string]string{"value": ref.ID, "display": ref.Display, "$ref": scimBase(r) + "/" + kind + "s/" + ref.ID}
	}
	return out
}

func scimUserJSON(r *http.Request, u *store.SCIMUser) map[string]any {
	out := map[string]any{
		"schemas": []string{scimUserSchema}, "id": u.ID, "userName": u.UserName, "active": u.Active,
		"groups": scimRefs(u.Groups, "Group", r), "meta": scimMeta(r, "User", u.ID, u.CreatedAt, u.UpdatedAt),
	}
	if u.ExternalID != "" {
		out["externalId"] = u.ExternalID
	}
	if u.DisplayName != "" {
		out["displayName"] = u.DisplayName
		out["name"] = map[string]string{"formatted": u.DisplayName}
	}
	if u.Email != "" {
		out["emails"] = []map[string]any{{"value": u.Email, "primary": true}}
	}
	return out
}

func scimGroupJSON(r *http.Request, g *store.SCIMGroup) map[string]any {
	out := map[string]any{
		"schemas": []string{scimGroupSchema}, "id": g.ID, "displayName": g.DisplayName,
		"members": scimRefs(g.Members, "User", r), "meta": scimMeta(r, "Group", g.ID, g.CreatedAt, g.UpdatedAt),
	}
	if g.ExternalID != "" {
		out["externalId"] = g.ExternalID
	}
	return out
}

func scimDecode(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxAdminBody)).Decode(v); err != nil {
		return scimBad("invalidSyntax", "invalid JSON body: %v", err)
	}
	return nil
}

// scimFilter parses the one filter form identity providers send when
// they look a resource up: attribute eq "value".
var scimFilterRE = regexp.MustCompile(`^\s*([A-Za-z.]+)\s+(?i:eq)\s+"((?:[^"\\]|\\.)*)"\s*$`)

func scimFilter(raw string, attrs map[string]string) (store.SCIMFilter, error) {
	if strings.TrimSpace(raw) == "" {
		return store.SCIMFilter{}, nil
	}
	m := scimFilterRE.FindStringSubmatch(raw)
	if m == nil {
		return store.SCIMFilter{}, scimBad("invalidFilter", `only filters of the form attribute eq "value" are supported`)
	}
	attr, ok := attrs[strings.ToLower(m[1])]
	if !ok {
		return store.SCIMFilter{}, scimBad("invalidFilter", "filtering on %s is not supported", m[1])
	}
	var value string
	if err := json.Unmarshal([]byte(`"`+m[2]+`"`), &value); err != nil {
		return store.SCIMFilter{}, scimBad("invalidFilter", "the filter value is not a valid string")
	}
	return store.SCIMFilter{Attr: attr, Value: value}, nil
}

// scimPage reads startIndex (1-based) and count.
func scimPage(r *http.Request) (start, count int, err error) {
	start, count = 1, 100
	q := r.URL.Query()
	if v := q.Get("startIndex"); v != "" {
		if start, err = strconv.Atoi(v); err != nil {
			return 0, 0, scimBad("invalidValue", "startIndex must be a number")
		}
		start = max(start, 1)
	}
	if v := q.Get("count"); v != "" {
		if count, err = strconv.Atoi(v); err != nil {
			return 0, 0, scimBad("invalidValue", "count must be a number")
		}
		count = min(max(count, 0), scimMaxResults)
	}
	return start, count, nil
}

func scimList(w http.ResponseWriter, start, total int, items []map[string]any) {
	if items == nil {
		items = []map[string]any{}
	}
	writeSCIM(w, http.StatusOK, map[string]any{
		"schemas": []string{scimListSchema}, "totalResults": total, "startIndex": start, "itemsPerPage": len(items), "Resources": items,
	})
}

// scimAudit records a provisioning change in the audit log.
func (s *Server) scimAudit(ctx context.Context, action, kind, id string, details any) error {
	raw, _ := json.Marshal(details)
	return s.store.AppendAudit(ctx, &store.AuditEntry{Actor: scimActor, Action: action, ResourceType: kind, ResourceID: id, Details: raw})
}

// --- discovery ---

// scimServiceProviderConfig describes what this SCIM service supports.
func (s *Server) scimServiceProviderConfig(w http.ResponseWriter, _ *http.Request) error {
	no := map[string]bool{"supported": false}
	writeSCIM(w, http.StatusOK, map[string]any{
		"schemas":          []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"documentationUri": "https://github.com/rnikadi/genaiopengateway/blob/main/docs/scim.md",
		"patch":            map[string]bool{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": scimMaxResults},
		"changePassword":   no, "sort": no, "etag": no,
		"authenticationSchemes": []map[string]any{{
			"type": "oauthbearertoken", "name": "Bearer token", "description": "The token in sso.scim.token", "primary": true,
		}},
	})
	return nil
}

// scimResourceTypes lists the User and Group resource types.
func (s *Server) scimResourceTypes(w http.ResponseWriter, r *http.Request) error {
	types := []map[string]any{
		{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"}, "id": "User", "name": "User", "endpoint": "/Users", "schema": scimUserSchema},
		{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"}, "id": "Group", "name": "Group", "endpoint": "/Groups", "schema": scimGroupSchema},
	}
	scimList(w, 1, len(types), types)
	return nil
}

// scimSchemas lists the attributes of users and groups the gateway keeps.
func (s *Server) scimSchemas(w http.ResponseWriter, _ *http.Request) error {
	attr := func(name, typ string, required bool) map[string]any {
		return map[string]any{"name": name, "type": typ, "multiValued": false, "required": required, "mutability": "readWrite", "returned": "default", "uniqueness": "none"}
	}
	multi := func(name string) map[string]any {
		a := attr(name, "complex", false)
		a["multiValued"] = true
		return a
	}
	userName := attr("userName", "string", true)
	userName["uniqueness"] = "server"
	schemas := []map[string]any{
		{"id": scimUserSchema, "name": "User", "attributes": []map[string]any{userName, attr("displayName", "string", false), attr("externalId", "string", false), attr("active", "boolean", false), multi("emails"), multi("groups")}},
		{"id": scimGroupSchema, "name": "Group", "attributes": []map[string]any{attr("displayName", "string", true), attr("externalId", "string", false), multi("members")}},
	}
	scimList(w, 1, len(schemas), schemas)
	return nil
}

// --- users ---

// scimUserIn is the part of a SCIM user the gateway keeps.
type scimUserIn struct {
	UserName    *string `json:"userName"`
	ExternalID  *string `json:"externalId"`
	DisplayName *string `json:"displayName"`
	Name        *struct {
		Formatted  string `json:"formatted"`
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} `json:"name"`
	Emails []struct {
		Value   string `json:"value"`
		Primary any    `json:"primary"`
	} `json:"emails"`
	Active any `json:"active"`
}

// scimBool reads a boolean that some identity providers send as a string.
func scimBool(v any) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case string:
		return strconv.ParseBool(strings.ToLower(b))
	}
	return false, scimBad("invalidValue", "expected a boolean, got %v", v)
}

// apply copies the attributes present in the input onto u.
func (in *scimUserIn) apply(u *store.SCIMUser) error {
	if in.UserName != nil {
		u.UserName = strings.TrimSpace(*in.UserName)
	}
	if in.ExternalID != nil {
		u.ExternalID = *in.ExternalID
	}
	if in.Name != nil {
		if n := in.Name.Formatted; n != "" {
			u.DisplayName = n
		} else if n := strings.TrimSpace(in.Name.GivenName + " " + in.Name.FamilyName); n != "" {
			u.DisplayName = n
		}
	}
	if in.DisplayName != nil {
		u.DisplayName = *in.DisplayName
	}
	if in.Emails != nil {
		u.Email = ""
		for i, e := range in.Emails {
			if p, _ := scimBool(e.Primary); p || i == 0 {
				u.Email = e.Value
			}
		}
	}
	if in.Active != nil {
		b, err := scimBool(in.Active)
		if err != nil {
			return err
		}
		u.Active = b
	}
	if u.UserName == "" {
		return scimBad("invalidValue", "userName is required")
	}
	return nil
}

var scimUserFilters = map[string]string{"username": "user_name", "externalid": "external_id", "emails.value": "email", "emails": "email", "displayname": "display_name"}

// scimListUsers lists provisioned users, optionally filtered by userName, externalId or email.
func (s *Server) scimListUsers(w http.ResponseWriter, r *http.Request) error {
	f, err := scimFilter(r.URL.Query().Get("filter"), scimUserFilters)
	if err != nil {
		return err
	}
	start, count, err := scimPage(r)
	if err != nil {
		return err
	}
	users, total, err := s.store.ListSCIMUsers(r.Context(), f, start-1, count)
	if err != nil {
		return err
	}
	items := make([]map[string]any, len(users))
	for i, u := range users {
		items[i] = scimUserJSON(r, u)
	}
	scimList(w, start, total, items)
	return nil
}

// scimCreateUser provisions a user.
func (s *Server) scimCreateUser(w http.ResponseWriter, r *http.Request) error {
	var in scimUserIn
	if err := scimDecode(r, &in); err != nil {
		return err
	}
	u := &store.SCIMUser{Active: true}
	if err := in.apply(u); err != nil {
		return err
	}
	if err := s.store.CreateSCIMUser(r.Context(), u); err != nil {
		return err
	}
	if err := s.scimAudit(r.Context(), "scim.user.create", "scim_user", u.ID, u); err != nil {
		return err
	}
	writeSCIM(w, http.StatusCreated, scimUserJSON(r, u))
	return nil
}

// scimGetUser returns one provisioned user with its groups.
func (s *Server) scimGetUser(w http.ResponseWriter, r *http.Request) error {
	u, err := s.store.GetSCIMUser(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeSCIM(w, http.StatusOK, scimUserJSON(r, u))
	return nil
}

// scimReplaceUser replaces a user's attributes.
func (s *Server) scimReplaceUser(w http.ResponseWriter, r *http.Request) error {
	var in scimUserIn
	if err := scimDecode(r, &in); err != nil {
		return err
	}
	u, err := s.store.GetSCIMUser(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	next := &store.SCIMUser{ID: u.ID, Active: true, Groups: u.Groups, CreatedAt: u.CreatedAt}
	if err := in.apply(next); err != nil {
		return err
	}
	return s.saveSCIMUser(w, r, u, next)
}

// scimPatchUser changes some of a user's attributes, such as active.
func (s *Server) scimPatchUser(w http.ResponseWriter, r *http.Request) error {
	ops, err := scimPatchOps(r)
	if err != nil {
		return err
	}
	u, err := s.store.GetSCIMUser(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	next := *u
	for _, op := range ops {
		if op.Op == "remove" {
			switch strings.ToLower(op.Path) {
			case "externalid":
				next.ExternalID = ""
			case "displayname", "name", "name.formatted":
				next.DisplayName = ""
			case "emails":
				next.Email = ""
			default:
				return scimBad("noTarget", "removing %q is not supported", op.Path)
			}
			continue
		}
		// add and replace: a path names one attribute, no path sends an
		// object of them.
		value := op.Value
		if op.Path != "" {
			path := op.Path
			if strings.HasPrefix(strings.ToLower(path), "emails[") {
				path = "emails"
				value = json.RawMessage(`[{"value":` + string(op.Value) + `,"primary":true}]`)
			}
			if strings.EqualFold(path, "name.formatted") {
				path, value = "name", json.RawMessage(`{"formatted":`+string(op.Value)+`}`)
			}
			value = json.RawMessage(`{` + strconv.Quote(scimUserAttr(path)) + `:` + string(value) + `}`)
		}
		var in scimUserIn
		if err := json.Unmarshal(value, &in); err != nil {
			return scimBad("invalidValue", "%s: %v", op.Path, err)
		}
		if err := in.apply(&next); err != nil {
			return err
		}
	}
	return s.saveSCIMUser(w, r, u, &next)
}

// scimUserAttr spells a user attribute as scimUserIn expects it.
func scimUserAttr(path string) string {
	for _, a := range []string{"userName", "externalId", "displayName", "name", "emails", "active"} {
		if strings.EqualFold(a, path) {
			return a
		}
	}
	return path
}

// saveSCIMUser stores a changed user. Deactivating a user signs them
// out of the admin UI at once.
func (s *Server) saveSCIMUser(w http.ResponseWriter, r *http.Request, before, after *store.SCIMUser) error {
	ctx := r.Context()
	if err := s.store.UpdateSCIMUser(ctx, after); err != nil {
		return err
	}
	if before.Active && !after.Active {
		if _, err := s.store.DeleteSessionsByEmail(ctx, before.UserName, before.Email); err != nil {
			return err
		}
	}
	if err := s.scimAudit(ctx, "scim.user.update", "scim_user", after.ID, map[string]any{"before": before, "after": after}); err != nil {
		return err
	}
	u, err := s.store.GetSCIMUser(ctx, after.ID)
	if err != nil {
		return err
	}
	writeSCIM(w, http.StatusOK, scimUserJSON(r, u))
	return nil
}

// scimDeleteUser deprovisions a user and signs them out.
func (s *Server) scimDeleteUser(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	u, err := s.store.GetSCIMUser(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	if err := s.store.DeleteSCIMUser(ctx, u.ID); err != nil {
		return err
	}
	if _, err := s.store.DeleteSessionsByEmail(ctx, u.UserName, u.Email); err != nil {
		return err
	}
	if err := s.scimAudit(ctx, "scim.user.delete", "scim_user", u.ID, map[string]any{"user_name": u.UserName}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- groups ---

type scimMember struct {
	Value string `json:"value"`
}

type scimGroupIn struct {
	DisplayName *string       `json:"displayName"`
	ExternalID  *string       `json:"externalId"`
	Members     *[]scimMember `json:"members"`
}

func memberIDs(ms []scimMember) []string {
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.Value != "" {
			ids = append(ids, m.Value)
		}
	}
	return ids
}

// scimMemberErr turns an unknown member into a SCIM 400.
func scimMemberErr(err error) error {
	if errors.Is(err, store.ErrNotFound) && strings.Contains(err.Error(), "no user") {
		return scimBad("invalidValue", "%v", err)
	}
	return err
}

var scimGroupFilters = map[string]string{"displayname": "display_name", "externalid": "external_id"}

// scimListGroups lists provisioned groups, optionally filtered by displayName.
func (s *Server) scimListGroups(w http.ResponseWriter, r *http.Request) error {
	f, err := scimFilter(r.URL.Query().Get("filter"), scimGroupFilters)
	if err != nil {
		return err
	}
	start, count, err := scimPage(r)
	if err != nil {
		return err
	}
	withMembers := !strings.Contains(r.URL.Query().Get("excludedAttributes"), "members")
	groups, total, err := s.store.ListSCIMGroups(r.Context(), f, start-1, count, withMembers)
	if err != nil {
		return err
	}
	items := make([]map[string]any, len(groups))
	for i, g := range groups {
		items[i] = scimGroupJSON(r, g)
	}
	scimList(w, start, total, items)
	return nil
}

// scimCreateGroup provisions a group with its members.
func (s *Server) scimCreateGroup(w http.ResponseWriter, r *http.Request) error {
	var in scimGroupIn
	if err := scimDecode(r, &in); err != nil {
		return err
	}
	if in.DisplayName == nil || strings.TrimSpace(*in.DisplayName) == "" {
		return scimBad("invalidValue", "displayName is required")
	}
	g := &store.SCIMGroup{DisplayName: strings.TrimSpace(*in.DisplayName)}
	if in.ExternalID != nil {
		g.ExternalID = *in.ExternalID
	}
	var members []string
	if in.Members != nil {
		members = memberIDs(*in.Members)
	}
	ctx := r.Context()
	if err := s.store.CreateSCIMGroup(ctx, g, members); err != nil {
		return scimMemberErr(err)
	}
	if err := s.scimAudit(ctx, "scim.group.create", "scim_group", g.ID, map[string]any{"display_name": g.DisplayName, "members": members}); err != nil {
		return err
	}
	g, err := s.store.GetSCIMGroup(ctx, g.ID)
	if err != nil {
		return err
	}
	writeSCIM(w, http.StatusCreated, scimGroupJSON(r, g))
	return nil
}

// scimGetGroup returns one provisioned group with its members.
func (s *Server) scimGetGroup(w http.ResponseWriter, r *http.Request) error {
	g, err := s.store.GetSCIMGroup(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeSCIM(w, http.StatusOK, scimGroupJSON(r, g))
	return nil
}

// scimReplaceGroup replaces a group's name and members.
func (s *Server) scimReplaceGroup(w http.ResponseWriter, r *http.Request) error {
	var in scimGroupIn
	if err := scimDecode(r, &in); err != nil {
		return err
	}
	g, err := s.store.GetSCIMGroup(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if in.DisplayName == nil || strings.TrimSpace(*in.DisplayName) == "" {
		return scimBad("invalidValue", "displayName is required")
	}
	g.DisplayName = strings.TrimSpace(*in.DisplayName)
	g.ExternalID = ""
	if in.ExternalID != nil {
		g.ExternalID = *in.ExternalID
	}
	members := []string{}
	if in.Members != nil {
		members = memberIDs(*in.Members)
	}
	return s.saveSCIMGroup(w, r, g, members, nil, nil)
}

// scimMemberPath matches members[value eq "id"], how a single member is
// removed.
var scimMemberPath = regexp.MustCompile(`^(?i:members)\[\s*(?i:value)\s+(?i:eq)\s+"([^"]+)"\s*\]$`)

// scimPatchGroup renames a group or adds, removes and replaces members.
func (s *Server) scimPatchGroup(w http.ResponseWriter, r *http.Request) error {
	ops, err := scimPatchOps(r)
	if err != nil {
		return err
	}
	g, err := s.store.GetSCIMGroup(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	var replace, add, remove []string
	for _, op := range ops {
		path := op.Path
		if m := scimMemberPath.FindStringSubmatch(path); m != nil {
			if op.Op != "remove" {
				return scimBad("invalidPath", "%s supports only remove", path)
			}
			remove = append(remove, m[1])
			continue
		}
		var in scimGroupIn
		switch strings.ToLower(path) {
		case "":
			if err := json.Unmarshal(op.Value, &in); err != nil {
				return scimBad("invalidValue", "%v", err)
			}
		case "members":
			var ms []scimMember
			if len(op.Value) > 0 {
				if err := json.Unmarshal(op.Value, &ms); err != nil {
					return scimBad("invalidValue", "members: %v", err)
				}
			}
			in.Members = &ms
		case "displayname":
			var name string
			if err := json.Unmarshal(op.Value, &name); err != nil {
				return scimBad("invalidValue", "displayName: %v", err)
			}
			in.DisplayName = &name
		case "externalid":
			var ext string
			if op.Op != "remove" {
				if err := json.Unmarshal(op.Value, &ext); err != nil {
					return scimBad("invalidValue", "externalId: %v", err)
				}
			}
			in.ExternalID = &ext
		default:
			return scimBad("invalidPath", "changing %q is not supported", path)
		}
		if in.DisplayName != nil {
			if strings.TrimSpace(*in.DisplayName) == "" {
				return scimBad("invalidValue", "displayName cannot be empty")
			}
			g.DisplayName = strings.TrimSpace(*in.DisplayName)
		}
		if in.ExternalID != nil {
			g.ExternalID = *in.ExternalID
		}
		if in.Members != nil {
			ids := memberIDs(*in.Members)
			switch op.Op {
			case "add":
				add = append(add, ids...)
			case "remove":
				if len(op.Value) == 0 {
					replace, add, remove = []string{}, nil, nil
				} else {
					remove = append(remove, ids...)
				}
			case "replace":
				replace, add, remove = ids, nil, nil
			}
		}
	}
	return s.saveSCIMGroup(w, r, g, replace, add, remove)
}

func (s *Server) saveSCIMGroup(w http.ResponseWriter, r *http.Request, g *store.SCIMGroup, replace, add, remove []string) error {
	ctx := r.Context()
	if err := s.store.UpdateSCIMGroup(ctx, g, replace, add, remove); err != nil {
		return scimMemberErr(err)
	}
	if err := s.scimAudit(ctx, "scim.group.update", "scim_group", g.ID, map[string]any{
		"display_name": g.DisplayName, "members": replace, "added": add, "removed": remove,
	}); err != nil {
		return err
	}
	g, err := s.store.GetSCIMGroup(ctx, g.ID)
	if err != nil {
		return err
	}
	writeSCIM(w, http.StatusOK, scimGroupJSON(r, g))
	return nil
}

// scimDeleteGroup removes a group; its members lose the roles it mapped.
func (s *Server) scimDeleteGroup(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g, err := s.store.GetSCIMGroup(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	if err := s.store.DeleteSCIMGroup(ctx, g.ID); err != nil {
		return err
	}
	if err := s.scimAudit(ctx, "scim.group.delete", "scim_group", g.ID, map[string]any{"display_name": g.DisplayName}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- PATCH ---

type scimPatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// scimPatchOps reads a PatchOp body. Operation names are compared
// without case, since Entra ID capitalizes them.
func scimPatchOps(r *http.Request) ([]scimPatchOp, error) {
	var body struct {
		Operations []scimPatchOp `json:"Operations"`
	}
	if err := scimDecode(r, &body); err != nil {
		return nil, err
	}
	if len(body.Operations) == 0 {
		return nil, scimBad("invalidValue", "Operations is required")
	}
	for i := range body.Operations {
		op := &body.Operations[i]
		op.Op = strings.ToLower(op.Op)
		if op.Op != "add" && op.Op != "replace" && op.Op != "remove" {
			return nil, scimBad("invalidValue", "op must be add, replace or remove")
		}
		if op.Op != "remove" && len(op.Value) == 0 {
			return nil, scimBad("invalidValue", "%s needs a value", op.Op)
		}
	}
	return body.Operations, nil
}

// --- admin API ---

// listDirectory lists provisioned users with their groups, and the groups, for owners.
func (s *Server) listDirectory(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	ctx := r.Context()
	const most = 5000
	users, nUsers, err := s.store.ListSCIMUsers(ctx, store.SCIMFilter{}, 0, most)
	if err != nil {
		return err
	}
	groups, nGroups, err := s.store.ListSCIMGroups(ctx, store.SCIMFilter{}, 0, most, false)
	if err != nil {
		return err
	}
	if users == nil {
		users = []*store.SCIMUser{}
	}
	if groups == nil {
		groups = []*store.SCIMGroup{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.rt.Load().cfg.SSO.SCIM.Enabled(), "users": users, "groups": groups,
		"total_users": nUsers, "total_groups": nGroups,
	})
	return nil
}

// --- role resolution ---

// directoryUser looks up a provisioned user by the email of a sign-in.
// It returns nil when SCIM is off or the user was never provisioned, in
// which case the identity provider's claims decide.
func (s *Server) directoryUser(ctx context.Context, email string) (*store.SCIMUser, error) {
	if !s.rt.Load().cfg.SSO.SCIM.Enabled() || email == "" || s.store == nil {
		return nil, nil
	}
	u, err := s.store.FindSCIMUser(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return u, err
}

// directoryGroups is the group names role mappings match for a user:
// the provisioned memberships when the user was provisioned, else the
// groups claim. ok is false when the user was deactivated.
func (s *Server) directoryGroups(ctx context.Context, claims *store.SessionClaims) (groups []string, ok bool, err error) {
	u, err := s.directoryUser(ctx, claims.Email)
	if err != nil || u == nil {
		return claims.Groups, err == nil, err
	}
	if !u.Active {
		return nil, false, nil
	}
	groups = make([]string, len(u.Groups))
	for i, g := range u.Groups {
		groups[i] = g.Display
	}
	return groups, true, nil
}
