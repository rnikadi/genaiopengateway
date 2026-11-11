package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// operator is who is calling the admin API: the admin token, or a person
// signed in to the admin UI (ADR 0016).
type operator struct {
	Subject string        `json:"subject"`
	Email   string        `json:"email,omitempty"`
	Name    string        `json:"name,omitempty"`
	Grants  []store.Grant `json:"grants"`
	// session is the hash of the session cookie, empty for the token.
	session string
	// claims are what role mappings match, for SSO sign-ins only.
	claims *store.SessionClaims
	// customRoles are sso.custom_roles when the operator was resolved.
	customRoles map[string]config.CustomRole
}

// tokenOperator is the admin token, which acts as owner.
var tokenOperator = &operator{Subject: adminActor, Name: "Admin token", Grants: []store.Grant{{Role: config.RoleOwner}}}

type operatorKey struct{}

func withOperator(ctx context.Context, o *operator) context.Context {
	return context.WithValue(ctx, operatorKey{}, o)
}

// operatorFrom returns the caller; outside the admin API it is the token.
func operatorFrom(ctx context.Context) *operator {
	if o, ok := ctx.Value(operatorKey{}).(*operator); ok {
		return o
	}
	return tokenOperator
}

// actor names the operator in the audit log.
func (o *operator) actor() string {
	if o.Email != "" {
		return o.Email
	}
	return o.Subject
}

func (o *operator) has(match func(store.Grant) bool) bool {
	return slices.ContainsFunc(o.Grants, match)
}

func (o *operator) isOwner() bool {
	return o.has(func(g store.Grant) bool { return g.Role == config.RoleOwner })
}

// permissions returns what a grant's role may do (docs/roles.md).
func (o *operator) permissions(role string) []string {
	sso := &config.SSO{CustomRoles: o.customRoles}
	return sso.RolePermissions(role)
}

// can reports whether the operator holds a permission for an
// organization, or for one of its teams when teamID is set. A team grant
// covers only its team, so it never answers for the organization as a
// whole; an empty orgID asks about every organization.
func (o *operator) can(perm, orgID, teamID string) bool {
	return o.has(func(g store.Grant) bool {
		if g.Role == config.RoleOwner {
			return true
		}
		if !slices.Contains(o.permissions(g.Role), perm) {
			return false
		}
		return g.OrgID == "" || (orgID != "" && g.OrgID == orgID && (g.TeamID == "" || g.TeamID == teamID))
	})
}

// global reports whether the operator holds a permission in every
// organization.
func (o *operator) global(perm string) bool { return o.can(perm, "", "") }

// sees reports whether the operator has any role in an organization, so
// may see its name, including through a team grant.
func (o *operator) sees(orgID string) bool {
	return o.global(config.PermOrgRead) || o.has(func(g store.Grant) bool { return orgID != "" && g.OrgID == orgID })
}

// scope lists where the operator holds a permission: the organizations it
// holds it in as a whole, and the teams it holds it in alone. all is true
// when it holds it everywhere.
func (o *operator) scope(perm string) (all bool, orgIDs, teamIDs []string) {
	if o.global(perm) {
		return true, nil, nil
	}
	orgIDs, teamIDs = []string{}, []string{}
	for _, g := range o.Grants {
		if !slices.Contains(o.permissions(g.Role), perm) {
			continue
		}
		if g.TeamID != "" {
			if !slices.Contains(teamIDs, g.TeamID) {
				teamIDs = append(teamIDs, g.TeamID)
			}
		} else if !slices.Contains(orgIDs, g.OrgID) {
			orgIDs = append(orgIDs, g.OrgID)
		}
	}
	return false, orgIDs, teamIDs
}

// orgIDs lists the organizations the operator has a role in.
func (o *operator) orgIDs() []string {
	var ids []string
	for _, g := range o.Grants {
		if g.OrgID != "" && !slices.Contains(ids, g.OrgID) {
			ids = append(ids, g.OrgID)
		}
	}
	return ids
}

var errForbidden = &apiError{http.StatusForbidden, "forbidden", "your role does not allow this"}

// allow checks that the caller holds a permission for an organization or
// one of its teams; an empty orgID means every organization.
func allow(r *http.Request, perm, orgID, teamID string) error {
	if !operatorFrom(r.Context()).can(perm, orgID, teamID) {
		return errForbidden
	}
	return nil
}

func requireOwner(r *http.Request) error {
	if !operatorFrom(r.Context()).isOwner() {
		return errForbidden
	}
	return nil
}

// resolveGrants maps the identity provider's view of a user to roles,
// naming organizations and teams by id. Mappings to organizations or
// teams that do not exist are skipped.
func (s *Server) resolveGrants(mappings []config.RoleMapping, email string, groups []string) []store.Grant {
	t := s.tenants.Load()
	var orgs map[string]*store.Org
	if t != nil {
		orgs = t.orgs
	}
	var out []store.Grant
	for _, m := range mappings {
		if !(m.Group != "" && slices.Contains(groups, m.Group)) && !(m.Email != "" && email != "" && strings.EqualFold(m.Email, email)) {
			continue
		}
		g := store.Grant{Role: m.Role}
		if m.Org != "" {
			g.OrgID = orgID(orgs, m.Org)
			if g.OrgID == "" {
				s.log.Warn("a role mapping names an organization that does not exist", "org", m.Org)
				continue
			}
		}
		if m.Team != "" {
			team := t.team(g.OrgID, m.Team)
			if team == nil {
				s.log.Warn("a role mapping names a team that does not exist", "org", m.Org, "team", m.Team)
				continue
			}
			g.TeamID = team.ID
		}
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

func orgID(orgs map[string]*store.Org, nameOrID string) string {
	if o := orgs[nameOrID]; o != nil {
		return o.ID
	}
	for _, o := range orgs {
		if o.Name == nameOrID {
			return o.ID
		}
	}
	return ""
}

// --- sessions ---

const sessionCookie = "gog_session"

// secure reports whether the browser reached the gateway over HTTPS.
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// cookieName adds the __Host- prefix over HTTPS, which binds the cookie
// to this host and path / and requires Secure.
func cookieName(r *http.Request, name string) string {
	if secure(r) {
		return "__Host-" + name
	}
	return name
}

func setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge time.Duration) {
	c := &http.Cookie{
		Name: cookieName(r, name), Value: value, Path: "/", MaxAge: int(maxAge.Seconds()),
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode,
	}
	if maxAge < 0 {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

func readCookie(r *http.Request, name string) string {
	c, err := r.Cookie(cookieName(r, name))
	if err != nil {
		return ""
	}
	return c.Value
}

func hashSession(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// startSession stores a session and sets its cookie.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, o *operator) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	value := base64.RawURLEncoding.EncodeToString(b)
	ttl := s.rt.Load().cfg.SSO.SessionTTL.Std()
	now := time.Now()
	if err := s.store.CreateSession(r.Context(), &store.Session{
		IDHash: hashSession(value), Subject: o.Subject, Email: o.Email, Name: o.Name, Grants: o.Grants, Claims: o.claims,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}); err != nil {
		return err
	}
	setCookie(w, r, sessionCookie, value, ttl)
	return s.store.AppendAudit(r.Context(), &store.AuditEntry{
		Actor: o.actor(), Action: "operator.sign_in", ResourceType: "operator", ResourceID: o.actor(),
	})
}

// sessionOperator returns the operator of the request's session cookie.
func (s *Server) sessionOperator(r *http.Request) (*operator, error) {
	v := readCookie(r, sessionCookie)
	if v == "" {
		return nil, store.ErrNotFound
	}
	h := hashSession(v)
	ss, err := s.store.GetSession(r.Context(), h)
	if err != nil {
		return nil, err
	}
	o := &operator{Subject: ss.Subject, Email: ss.Email, Name: ss.Name, Grants: ss.Grants, session: h, customRoles: s.rt.Load().cfg.SSO.CustomRoles}
	// Roles follow the current sso.roles, so removing a mapping takes
	// effect at once rather than at the next sign-in.
	if cfg := s.rt.Load().cfg; ss.Claims != nil && cfg.SSO.OIDC != nil {
		o.Grants = s.resolveGrants(cfg.SSO.Roles, ss.Claims.Email, ss.Claims.Groups)
		if len(o.Grants) == 0 {
			return nil, store.ErrNotFound
		}
	}
	return o, nil
}

// sameOrigin is the CSRF check for requests authenticated by cookie that
// change something: browsers do not let other sites forge Origin, nor
// send a JSON content type. Multipart is allowed, on Origin alone, for
// the playground's audio uploads.
func sameOrigin(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" && mt != "multipart/form-data" {
		return false
	}
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	return r.Header.Get("Origin") == scheme+"://"+r.Host
}

// adminOperator authenticates an admin API request by the admin token or
// a session cookie.
func (s *Server) adminOperator(r *http.Request) (*operator, *apiError) {
	if presentedKey(r) != "" {
		token := s.rt.Load().cfg.Admin.Token.Value()
		if token != "" && subtle.ConstantTimeCompare([]byte(presentedKey(r)), []byte(token)) == 1 {
			return tokenOperator, nil
		}
		return nil, &apiError{http.StatusUnauthorized, "unauthorized", "missing or invalid admin token"}
	}
	o, err := s.sessionOperator(r)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, &apiError{http.StatusUnauthorized, "unauthorized", "sign in or send the admin token"}
	case err != nil:
		s.log.Error("loading an admin session failed", "err", err)
		return nil, &apiError{http.StatusServiceUnavailable, "unavailable", "sessions are unavailable"}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
		return nil, &apiError{http.StatusForbidden, "forbidden", "cross-origin request refused: send JSON from the gateway's own origin"}
	}
	return o, nil
}

// deleteSessions signs out everyone else, or with ?subject= one user,
// for when someone leaves or a session may be stolen. The caller's own
// session is kept.
func (s *Server) deleteSessions(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	ctx := r.Context()
	subject := r.URL.Query().Get("subject")
	n, err := s.store.DeleteOtherSessions(ctx, operatorFrom(ctx).session, subject)
	if err != nil {
		return err
	}
	if err := s.audit(ctx, "operator.sessions.delete", "operator", subject, "", map[string]any{"subject": subject, "deleted": n}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": n})
	return nil
}

func (s *Server) sessionPruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if _, err := s.store.PruneSessions(ctx, time.Now()); err != nil {
			s.log.Error("pruning admin sessions failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
