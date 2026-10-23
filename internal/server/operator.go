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

// readsAll reports whether the operator may read every organization.
func (o *operator) readsAll() bool {
	return o.has(func(g store.Grant) bool {
		return g.Role == config.RoleOwner || (g.Role == config.RoleViewer && g.OrgID == "")
	})
}

func (o *operator) canRead(orgID string) bool {
	return o.readsAll() || (orgID != "" && o.has(func(g store.Grant) bool { return g.OrgID == orgID }))
}

func (o *operator) canWrite(orgID string) bool {
	return o.isOwner() || (orgID != "" && o.has(func(g store.Grant) bool { return g.Role == config.RoleAdmin && g.OrgID == orgID }))
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

// read and write check the caller's role for an organization; an empty
// orgID means data not tied to one organization.
func read(r *http.Request, orgID string) error {
	if !operatorFrom(r.Context()).canRead(orgID) {
		return errForbidden
	}
	return nil
}

func write(r *http.Request, orgID string) error {
	if !operatorFrom(r.Context()).canWrite(orgID) {
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
// naming organizations by id. Mappings to organizations that do not
// exist are skipped.
func (s *Server) resolveGrants(mappings []config.RoleMapping, email string, groups []string) []store.Grant {
	var orgs map[string]*store.Org
	if t := s.tenants.Load(); t != nil {
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
		IDHash: hashSession(value), Subject: o.Subject, Email: o.Email, Name: o.Name, Grants: o.Grants,
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
	return &operator{Subject: ss.Subject, Email: ss.Email, Name: ss.Name, Grants: ss.Grants, session: h}, nil
}

// sameOrigin is the CSRF check for requests authenticated by cookie that
// change something: browsers do not let other sites send a JSON content
// type or forge Origin.
func sameOrigin(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
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
