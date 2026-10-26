package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/oauth"
)

// Operator sign-in for the admin UI (docs/admin-ui.md#sign-in).

const (
	authCookie = "gog_auth"
	// authFlowTTL bounds how long a sign-in may take at the identity provider.
	authFlowTTL = 10 * time.Minute
)

// oidcClient is the identity provider's discovered configuration.
type oidcClient struct {
	cfg      config.OIDC
	provider *oidc.Provider
}

// oidc discovers the identity provider on first use, and again when its
// settings change.
func (s *Server) oidc(ctx context.Context, cfg *config.OIDC) (*oidcClient, error) {
	s.ssoMu.Lock()
	defer s.ssoMu.Unlock()
	if c := s.sso; c != nil && reflect.DeepEqual(c.cfg, *cfg) {
		return c, nil
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, s.client), 10*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	s.sso = &oidcClient{cfg: *cfg, provider: p}
	return s.sso, nil
}

func (c *oidcClient) oauth2() *oauth2.Config {
	return &oauth2.Config{
		ClientID: c.cfg.ClientID, ClientSecret: c.cfg.ClientSecret.Value(), RedirectURL: c.cfg.RedirectURL,
		Endpoint: c.provider.Endpoint(), Scopes: c.cfg.Scopes,
	}
}

// authFlow is what the sign-in remembers between the redirect to the
// identity provider and the callback, in a signed cookie.
type authFlow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
	Expires  int64  `json:"e"`
	// Connect, when set, is the id of an MCP connect request whose user
	// signs in only to prove who they are (docs/mcp-user-credentials.md).
	Connect string `json:"c,omitempty"`
}

// flowKey signs the flow cookie. It is derived from the client secret, so
// every replica can check a flow another replica started.
func flowKey(cfg *config.OIDC) []byte {
	sum := sha256.Sum256([]byte("genaiopengateway sign-in flow\x00" + cfg.ClientSecret.Value()))
	return sum[:]
}

func sealFlow(cfg *config.OIDC, f *authFlow) string {
	raw, _ := json.Marshal(f)
	m := hmac.New(sha256.New, flowKey(cfg))
	m.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func openFlow(cfg *config.OIDC, v string) (*authFlow, error) {
	body, sig, ok := strings.Cut(v, ".")
	raw, err1 := base64.RawURLEncoding.DecodeString(body)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err1 != nil || err2 != nil {
		return nil, errors.New("malformed sign-in cookie")
	}
	m := hmac.New(sha256.New, flowKey(cfg))
	m.Write(raw)
	if !hmac.Equal(mac, m.Sum(nil)) {
		return nil, errors.New("sign-in cookie signature mismatch")
	}
	var f authFlow
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if time.Now().Unix() > f.Expires {
		return nil, errors.New("sign-in took too long")
	}
	return &f, nil
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// returnPath accepts only paths within the UI, so a sign-in link cannot
// send the user to another site.
func returnPath(p string) string {
	if strings.HasPrefix(p, "/ui/") && !strings.ContainsAny(p, "\\\r\n") && !strings.HasPrefix(p, "/ui//") {
		return p
	}
	return "/ui/"
}

// authConfig tells the sign-in page which methods are available.
func (s *Server) authConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := s.rt.Load().cfg
	writeJSON(w, http.StatusOK, map[string]bool{"oidc": cfg.SSO.OIDC != nil, "token": cfg.SSO.OIDC == nil && cfg.Admin.Token.Value() != ""})
}

// login starts the authorization code flow with PKCE.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg.SSO.OIDC
	if cfg == nil {
		http.NotFound(w, r)
		return
	}
	c, err := s.oidc(r.Context(), cfg)
	if err != nil {
		s.log.Error("reaching the identity provider failed", "issuer", cfg.Issuer, "err", err)
		http.Error(w, "The identity provider cannot be reached. Try again shortly.", http.StatusBadGateway)
		return
	}
	s.signIn(w, r, cfg, c, &authFlow{Return: returnPath(r.URL.Query().Get("return"))})
}

// signIn sends the browser to the identity provider with a new flow.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, cfg *config.OIDC, c *oidcClient, f *authFlow) {
	f.State, f.Nonce, f.Verifier = randomString(), randomString(), oauth2.GenerateVerifier()
	f.Expires = time.Now().Add(authFlowTTL).Unix()
	setCookie(w, r, authCookie, sealFlow(cfg, f), authFlowTTL)
	http.Redirect(w, r, c.oauth2().AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

// callback finishes the flow: it exchanges the code, verifies the ID
// token, resolves roles and starts a session.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg
	oc := cfg.SSO.OIDC
	if oc == nil {
		http.NotFound(w, r)
		return
	}
	fail := func(status int, msg string, err error) {
		s.log.Warn("operator sign-in failed", "reason", msg, "err", err)
		http.Error(w, msg, status)
	}
	f, err := openFlow(oc, readCookie(r, authCookie))
	setCookie(w, r, authCookie, "", -1)
	q := r.URL.Query()
	switch {
	case err != nil:
		fail(http.StatusBadRequest, "The sign-in expired or was started elsewhere. Start again from the gateway.", err)
		return
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1:
		fail(http.StatusBadRequest, "The sign-in state does not match. Start again from the gateway.", nil)
		return
	case q.Get("error") != "":
		fail(http.StatusForbidden, "The identity provider refused the sign-in: "+q.Get("error"), errors.New(q.Get("error_description")))
		return
	}
	c, err := s.oidc(r.Context(), oc)
	if err != nil {
		fail(http.StatusBadGateway, "The identity provider cannot be reached. Try again shortly.", err)
		return
	}
	ctx := oidc.ClientContext(r.Context(), s.client)
	tok, err := c.oauth2().Exchange(ctx, q.Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		fail(http.StatusBadGateway, "The identity provider did not accept the sign-in code.", err)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := c.provider.Verifier(&oidc.Config{ClientID: oc.ClientID}).Verify(ctx, rawID)
	if err != nil {
		fail(http.StatusUnauthorized, "The identity provider's ID token is not valid.", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.Nonce)) != 1 {
		fail(http.StatusUnauthorized, "The ID token was not issued for this sign-in.", nil)
		return
	}
	if f.Connect != "" {
		s.connectSignedIn(w, r, f.Connect, idt.Subject)
		return
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		fail(http.StatusUnauthorized, "The ID token's claims cannot be read.", err)
		return
	}
	if _, ok := claims[oc.GroupsClaim]; !ok {
		// Some providers put groups only in UserInfo.
		if ui, err := c.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
			extra := map[string]any{}
			if ui.Claims(&extra) == nil && extra[oc.GroupsClaim] != nil {
				claims[oc.GroupsClaim] = extra[oc.GroupsClaim]
			}
		}
	}
	o := &operator{Subject: idt.Subject}
	o.Email, _ = claims["email"].(string)
	if o.Name, _ = claims["name"].(string); o.Name == "" {
		o.Name, _ = claims["preferred_username"].(string)
	}
	o.Grants = s.resolveGrants(cfg.SSO.Roles, verifiedEmail(claims), oauth.Strings(claims[oc.GroupsClaim]))
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

// verifiedEmail is the email that role mappings may match: none when the
// identity provider says it is unverified (ADR 0029). Providers that never
// send email_verified, such as Entra ID, are trusted.
func verifiedEmail(claims map[string]any) string {
	email, _ := claims["email"].(string)
	switch v := claims["email_verified"].(type) {
	case bool:
		if !v {
			return ""
		}
	case string:
		if strings.EqualFold(v, "false") {
			return ""
		}
	}
	return email
}

// tokenLogin signs in with the admin token when OIDC is not configured.
func (s *Server) tokenLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg
	token := cfg.Admin.Token.Value()
	if cfg.SSO.OIDC != nil || token == "" {
		writeAdminError(w, http.StatusNotFound, "not_found", "sign in with the identity provider")
		return
	}
	if !sameOrigin(r) {
		writeAdminError(w, http.StatusForbidden, "forbidden", "cross-origin request refused")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(token)) != 1 {
		writeAdminError(w, http.StatusUnauthorized, "unauthorized", "invalid admin token")
		return
	}
	if err := s.startSession(w, r, tokenOperator); err != nil {
		s.log.Error("saving an admin session failed", "err", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "the session could not be saved")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// logout ends the session.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeAdminError(w, http.StatusForbidden, "forbidden", "cross-origin request refused")
		return
	}
	if v := readCookie(r, sessionCookie); v != "" {
		if err := s.store.DeleteSession(r.Context(), hashSession(v)); err != nil {
			s.log.Error("deleting an admin session failed", "err", err)
		}
	}
	setCookie(w, r, sessionCookie, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
