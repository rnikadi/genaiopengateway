package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Operator sign-in with a SAML 2.0 identity provider (docs/saml.md). The
// gateway is the service provider: it sends the browser to the identity
// provider with an AuthnRequest over the redirect binding and takes the
// signed response on its assertion consumer service over the POST
// binding. Roles and sessions then work as with OIDC.

const samlCookie = "gog_saml"

// samlProvider is the service provider for the current settings, with
// the identity provider's metadata.
type samlProvider struct {
	cfg config.SAML
	sp  *saml.ServiceProvider
}

// samlSP builds the service provider, fetching the identity provider's
// metadata on first use and again when the settings change.
func (s *Server) samlSP(ctx context.Context, cfg *config.SAML) (*saml.ServiceProvider, error) {
	s.ssoMu.Lock()
	defer s.ssoMu.Unlock()
	if p := s.saml; p != nil && reflect.DeepEqual(p.cfg, *cfg) {
		return p.sp, nil
	}
	var md *saml.EntityDescriptor
	var err error
	if cfg.IDPMetadata != "" {
		md, err = samlsp.ParseMetadata([]byte(cfg.IDPMetadata))
	} else {
		u, perr := url.Parse(cfg.IDPMetadataURL)
		if perr != nil {
			return nil, perr
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		md, err = samlsp.FetchMetadata(ctx, s.client, *u)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the identity provider's metadata: %w", err)
	}
	acs, err := url.Parse(cfg.ACSURL)
	if err != nil {
		return nil, err
	}
	meta := *acs
	meta.Path = "/ui/auth/saml/metadata"
	sp := &saml.ServiceProvider{
		EntityID: cfg.EntityID, AcsURL: *acs, MetadataURL: meta, IDPMetadata: md,
		HTTPClient: s.client, AuthnNameIDFormat: saml.UnspecifiedNameIDFormat,
		// Both refuse what a careful service provider should: assertions
		// signed with SHA-1, and responses that answer no request of ours.
		RejectSHA1Signatures: true,
		ReplayCache:          samlReplayCache{s.store},
	}
	s.saml = &samlProvider{cfg: *cfg, sp: sp}
	return sp, nil
}

// samlReplayCache refuses an assertion used before, on any replica.
type samlReplayCache struct{ st *store.Store }

func (c samlReplayCache) Consume(assertionID string, expiresAt time.Time) error {
	err := c.st.ConsumeAssertion(context.Background(), assertionID, expiresAt)
	if errors.Is(err, store.ErrReplayed) {
		return saml.ErrAssertionReplayed
	}
	return err
}

// samlFlow is what the sign-in remembers between the redirect and the
// response: the request id the response must answer, and where to go
// afterwards.
type samlFlow struct {
	RequestID string `json:"i"`
	Return    string `json:"r"`
}

// samlCookieOptions lets the identity provider's cross-site POST carry
// the flow cookie: SameSite=None needs Secure, so it applies over HTTPS.
func setSAMLCookie(w http.ResponseWriter, r *http.Request, value string, maxAge time.Duration) {
	c := &http.Cookie{
		Name: cookieName(r, samlCookie), Value: value, Path: "/", MaxAge: int(maxAge.Seconds()),
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode,
	}
	if secure(r) {
		c.SameSite = http.SameSiteNoneMode
	}
	if maxAge < 0 {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// samlMetadata serves the gateway's SAML service provider metadata for the identity provider.
func (s *Server) samlMetadata(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg.SSO.SAML
	if cfg == nil {
		http.NotFound(w, r)
		return
	}
	sp, err := s.samlSP(r.Context(), cfg)
	if err != nil {
		s.log.Error("reading the SAML identity provider failed", "err", err)
		http.Error(w, "The identity provider's metadata cannot be read.", http.StatusBadGateway)
		return
	}
	raw, err := xml.MarshalIndent(sp.Metadata(), "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, _ = w.Write(append([]byte(xml.Header), raw...))
}

// samlLogin sends the browser to the SAML identity provider with an AuthnRequest.
func (s *Server) samlLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg.SSO.SAML
	if cfg == nil {
		http.NotFound(w, r)
		return
	}
	sp, err := s.samlSP(r.Context(), cfg)
	if err != nil {
		s.log.Error("reading the SAML identity provider failed", "err", err)
		http.Error(w, "The identity provider cannot be reached. Try again shortly.", http.StatusBadGateway)
		return
	}
	req, err := sp.MakeAuthenticationRequest(sp.GetSSOBindingLocation(saml.HTTPRedirectBinding), saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		s.log.Error("making a SAML request failed", "err", err)
		http.Error(w, "The identity provider has no redirect sign-in endpoint.", http.StatusBadGateway)
		return
	}
	to, err := req.Redirect("", sp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	raw, _ := json.Marshal(samlFlow{RequestID: req.ID, Return: returnPath(r.URL.Query().Get("return"))})
	setSAMLCookie(w, r, base64.RawURLEncoding.EncodeToString(raw), authFlowTTL)
	http.Redirect(w, r, to.String(), http.StatusFound)
}

// samlACS takes the identity provider's signed response, resolves roles and starts a session.
func (s *Server) samlACS(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg
	mc := cfg.SSO.SAML
	if mc == nil {
		http.NotFound(w, r)
		return
	}
	fail := func(status int, msg string, err error) {
		s.log.Warn("operator sign-in failed", "reason", msg, "err", err)
		http.Error(w, msg, status)
	}
	var f samlFlow
	raw, err := base64.RawURLEncoding.DecodeString(readCookie(r, samlCookie))
	setSAMLCookie(w, r, "", -1)
	if err != nil || json.Unmarshal(raw, &f) != nil || f.RequestID == "" {
		fail(http.StatusBadRequest, "The sign-in expired or was started elsewhere. Start again from the gateway.", err)
		return
	}
	sp, err := s.samlSP(r.Context(), mc)
	if err != nil {
		fail(http.StatusBadGateway, "The identity provider cannot be reached. Try again shortly.", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)
	if err := r.ParseForm(); err != nil {
		fail(http.StatusBadRequest, "The identity provider's response cannot be read.", err)
		return
	}
	assertion, err := sp.ParseResponse(r, []string{f.RequestID})
	if err != nil {
		var ire *saml.InvalidResponseError
		if errors.As(err, &ire) {
			err = ire.PrivateErr
		}
		fail(http.StatusUnauthorized, "The identity provider's response is not valid.", err)
		return
	}
	o := samlOperator(mc, assertion)
	o.claims.Groups = samlAttribute(assertion, mc.GroupsAttribute)
	groups, active, err := s.directoryGroups(r.Context(), o.claims)
	if err != nil {
		fail(http.StatusServiceUnavailable, "The directory of provisioned users cannot be read. Try again shortly.", err)
		return
	}
	if !active {
		fail(http.StatusForbidden, fmt.Sprintf("%s was deactivated in the directory.", o.actor()), nil)
		return
	}
	o.Grants = s.resolveGrants(cfg.SSO.Roles, o.claims.Email, groups)
	if len(o.Grants) == 0 {
		fail(http.StatusForbidden, fmt.Sprintf("%s has no role on this gateway. Ask an owner to map one of your groups to a role.", o.actor()), nil)
		return
	}
	if err := s.startSession(w, r, o); err != nil {
		fail(http.StatusInternalServerError, "The session could not be saved.", err)
		return
	}
	http.Redirect(w, r, f.Return, http.StatusFound)
}

// samlOperator reads who signed in from an assertion: the NameID is the
// subject, and the email is the email attribute or, without one, a NameID
// that looks like an address.
func samlOperator(cfg *config.SAML, a *saml.Assertion) *operator {
	o := &operator{claims: &store.SessionClaims{}}
	if a.Subject != nil && a.Subject.NameID != nil {
		o.Subject = a.Subject.NameID.Value
	}
	email := ""
	if cfg.EmailAttribute != "" {
		if v := samlAttribute(a, cfg.EmailAttribute); len(v) > 0 {
			email = v[0]
		}
	} else {
		for _, name := range []string{"email", "mail", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress"} {
			if v := samlAttribute(a, name); len(v) > 0 {
				email = v[0]
				break
			}
		}
		if email == "" && strings.Contains(o.Subject, "@") {
			email = o.Subject
		}
	}
	o.Email, o.claims.Email = email, email
	names := []string{cfg.NameAttribute}
	if cfg.NameAttribute == "" {
		names = []string{"displayName", "name", "http://schemas.microsoft.com/identity/claims/displayname"}
	}
	for _, name := range names {
		if v := samlAttribute(a, name); len(v) > 0 {
			o.Name = v[0]
			break
		}
	}
	if o.Subject == "" {
		o.Subject = email
	}
	return o
}

// samlAttribute returns an attribute's values, matched by name or
// friendly name.
func samlAttribute(a *saml.Assertion, name string) []string {
	var out []string
	for _, st := range a.AttributeStatements {
		for _, attr := range st.Attributes {
			if attr.Name != name && attr.FriendlyName != name {
				continue
			}
			for _, v := range attr.Values {
				if v.Value != "" {
					out = append(out, v.Value)
				}
			}
		}
	}
	return out
}
