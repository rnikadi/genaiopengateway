package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Per-user upstream MCP credentials (docs/mcp-user-credentials.md): the
// gateway is an OAuth client of each user_auth upstream, runs the connect
// flow in the user's browser, and keeps each user's tokens encrypted in
// the store.

const (
	// connectTTL bounds how long a connect link works.
	connectTTL = 10 * time.Minute
	// tokenCacheTTL bounds how long a decrypted token is reused from
	// memory, so a revocation on another replica takes effect soon.
	tokenCacheTTL = 30 * time.Second
	// refreshBefore is how long before expiry a token is refreshed.
	refreshBefore = time.Minute
	connectCookie = "gog_connect"
)

type connKey struct{ server, subject string }

type cachedToken struct {
	access  string
	expires *time.Time
	loaded  time.Time
}

func (t cachedToken) usable(now time.Time) bool {
	return now.Sub(t.loaded) < tokenCacheTTL && (t.expires == nil || t.expires.Sub(now) > refreshBefore)
}

// endpoints are an upstream's OAuth endpoints, configured or discovered.
type endpoints struct {
	cfg                 config.UserAuth
	url                 string
	auth, token, revoke string
}

// connections implements mcpgw.Credentials.
type connections struct {
	s *Server

	mu        sync.Mutex
	locks     map[connKey]*sync.Mutex
	cache     map[connKey]cachedToken
	touched   map[connKey]time.Time
	endpoints map[string]*endpoints
}

func newConnections(s *Server) *connections {
	return &connections{
		s: s, locks: map[connKey]*sync.Mutex{}, cache: map[connKey]cachedToken{},
		touched: map[connKey]time.Time{}, endpoints: map[string]*endpoints{},
	}
}

// connAD binds a sealed token to its connection and kind, so a ciphertext
// copied to another row does not decrypt.
func connAD(server, subject, kind string) []byte {
	return []byte("mcp_connection\x00" + server + "\x00" + subject + "\x00" + kind)
}

func (c *connections) cached(k connKey) (cachedToken, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.cache[k]
	return t, ok && t.usable(time.Now())
}

func (c *connections) lock(k connKey) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.locks[k]
	if l == nil {
		l = &sync.Mutex{}
		c.locks[k] = l
	}
	return l
}

func (c *connections) forget(k connKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, k)
	delete(c.touched, k)
}

// Token returns a user's access token, refreshing it under a lock per
// connection, so concurrent calls refresh once.
func (c *connections) Token(ctx context.Context, server, subject string, force bool) (string, error) {
	k := connKey{server, subject}
	if t, ok := c.cached(k); ok && !force {
		c.touch(k)
		return t.access, nil
	}
	l := c.lock(k)
	l.Lock()
	defer l.Unlock()
	if t, ok := c.cached(k); ok && !force {
		return t.access, nil
	}
	conn, err := c.s.store.GetMCPConnection(ctx, server, subject)
	if errors.Is(err, store.ErrNotFound) {
		c.forget(k)
		return "", mcpgw.ErrNotConnected
	}
	if err != nil {
		return "", err
	}
	box, err := secretBox(c.s.rt.Load().cfg.Secrets)
	if err != nil {
		return "", err
	}
	access, err := box.Open(conn.AccessToken, connAD(server, subject, "access"))
	if err != nil {
		// Sealed with a key that is no longer configured.
		c.s.log.Warn("an MCP connection cannot be decrypted; the user must connect again", "server", server, "subject", subject)
		c.remove(ctx, k, "undecryptable")
		return "", mcpgw.ErrNotConnected
	}
	var refresh []byte
	if conn.RefreshToken != "" {
		if refresh, err = box.Open(conn.RefreshToken, connAD(server, subject, "refresh")); err != nil {
			refresh = nil
		}
	}
	now := time.Now()
	expiring := conn.ExpiresAt != nil && conn.ExpiresAt.Sub(now) <= refreshBefore
	if force || expiring || !box.Current(conn.AccessToken) {
		expired := conn.ExpiresAt != nil && !conn.ExpiresAt.After(now)
		switch {
		case len(refresh) > 0:
			tok, err := c.refresh(ctx, server, string(refresh))
			var re *oauth2.RetrieveError
			switch {
			case errors.As(err, &re) && (re.ErrorCode == "invalid_grant" || (re.Response != nil && re.Response.StatusCode == http.StatusUnauthorized)):
				c.s.metrics.ObserveTokenRefresh(server, "invalid_grant")
				c.remove(ctx, k, "invalid_grant")
				return "", mcpgw.ErrNotConnected
			case err != nil:
				c.s.metrics.ObserveTokenRefresh(server, "error")
				c.s.log.Warn("refreshing an MCP user token failed", "server", server, "err", err)
				if force || expired {
					return "", fmt.Errorf("refreshing the token failed: %w", err)
				}
			default:
				c.s.metrics.ObserveTokenRefresh(server, "ok")
				if tok.RefreshToken == "" {
					tok.RefreshToken = string(refresh)
				}
				if err := c.save(ctx, server, subject, conn.Scopes, tok); err != nil {
					return "", err
				}
				access = []byte(tok.AccessToken)
				conn.ExpiresAt = expiry(tok)
			}
		case force || expired:
			// Nothing to refresh with: the connection is spent.
			c.remove(ctx, k, "expired")
			return "", mcpgw.ErrNotConnected
		}
	}
	c.mu.Lock()
	c.cache[k] = cachedToken{access: string(access), expires: conn.ExpiresAt, loaded: now}
	c.mu.Unlock()
	c.touch(k)
	return string(access), nil
}

func expiry(t *oauth2.Token) *time.Time {
	if t.Expiry.IsZero() {
		return nil
	}
	e := t.Expiry.UTC()
	return &e
}

// save encrypts and stores a user's tokens with the current key.
func (c *connections) save(ctx context.Context, server, subject string, scopes []string, tok *oauth2.Token) error {
	box, err := secretBox(c.s.rt.Load().cfg.Secrets)
	if err != nil {
		return err
	}
	conn := &store.MCPConnection{
		Server: server, Subject: subject, Scopes: scopes, ExpiresAt: expiry(tok),
		AccessToken: box.Seal([]byte(tok.AccessToken), connAD(server, subject, "access")),
	}
	if tok.RefreshToken != "" {
		conn.RefreshToken = box.Seal([]byte(tok.RefreshToken), connAD(server, subject, "refresh"))
	}
	if s, ok := tok.Extra("scope").(string); ok && s != "" {
		conn.Scopes = strings.Fields(s)
	}
	c.forget(connKey{server, subject})
	return c.s.store.PutMCPConnection(ctx, conn)
}

// remove deletes a connection the gateway can no longer use.
func (c *connections) remove(ctx context.Context, k connKey, reason string) {
	c.forget(k)
	if err := c.s.store.DeleteMCPConnection(context.WithoutCancel(ctx), k.server, k.subject); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.s.log.Error("deleting an MCP connection failed", "server", k.server, "err", err)
		return
	}
	c.s.log.Info("MCP connection removed", "server", k.server, "subject", k.subject, "reason", reason)
}

// Broken forgets a connection whose token the upstream refuses even
// after a refresh.
func (c *connections) Broken(ctx context.Context, server, subject string) {
	c.remove(ctx, connKey{server, subject}, "refused by the upstream")
}

// touch records use of a connection, at most once a minute.
func (c *connections) touch(k connKey) {
	now := time.Now()
	c.mu.Lock()
	if now.Sub(c.touched[k]) < time.Minute {
		c.mu.Unlock()
		return
	}
	c.touched[k] = now
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.s.store.TouchMCPConnection(ctx, k.server, k.subject, now); err != nil {
			c.s.log.Warn("recording MCP connection use failed", "err", err)
		}
	}()
}

// RecentSubject names the user whose connection was used last.
func (c *connections) RecentSubject(ctx context.Context, server string) string {
	subject, err := c.s.store.RecentMCPSubject(ctx, server)
	if err != nil {
		c.s.log.Warn("finding a connected MCP user failed", "server", server, "err", err)
	}
	return subject
}

func hashID(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// ConnectURL stores a connect request and returns its link.
func (c *connections) ConnectURL(ctx context.Context, server, subject, virtualServer string) (string, error) {
	id := randomString()
	err := c.s.store.CreateConnectRequest(context.WithoutCancel(ctx), &store.ConnectRequest{
		IDHash: hashID(id), Server: server, Subject: subject, VirtualServer: virtualServer,
		Verifier: oauth2.GenerateVerifier(), ExpiresAt: time.Now().Add(connectTTL),
	})
	if err != nil {
		return "", err
	}
	return c.s.rt.Load().cfg.MCP.PublicURL + "/mcp/connect/" + id, nil
}

// oauthConfig is the gateway's client configuration for an upstream.
func (c *connections) oauthConfig(ctx context.Context, server string) (*oauth2.Config, *endpoints, error) {
	cfg := c.s.rt.Load().cfg
	sc, ok := cfg.MCP.Servers[server]
	if !ok || sc.UserAuth == nil {
		return nil, nil, fmt.Errorf("server %q does not use user_auth", server)
	}
	ep, err := c.discover(ctx, server, sc)
	if err != nil {
		return nil, nil, err
	}
	ua := sc.UserAuth
	return &oauth2.Config{
		ClientID: ua.ClientID, ClientSecret: ua.ClientSecret.Value(), Scopes: ua.Scopes,
		Endpoint:    oauth2.Endpoint{AuthURL: ep.auth, TokenURL: ep.token},
		RedirectURL: cfg.MCP.PublicURL + "/mcp/connect/callback",
	}, ep, nil
}

func (c *connections) oauthContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: c.s.client.Transport, Timeout: 30 * time.Second})
}

func (c *connections) refresh(ctx context.Context, server, refreshToken string) (*oauth2.Token, error) {
	oc, _, err := c.oauthConfig(ctx, server)
	if err != nil {
		return nil, err
	}
	return oc.TokenSource(c.oauthContext(ctx), &oauth2.Token{RefreshToken: refreshToken}).Token()
}

// discover returns an upstream's OAuth endpoints: those configured, and
// the rest from its protected resource metadata (RFC 9728) and its
// authorization server's metadata (RFC 8414, or OpenID discovery).
func (c *connections) discover(ctx context.Context, name string, sc config.MCPServer) (*endpoints, error) {
	ua := sc.UserAuth
	c.mu.Lock()
	ep := c.endpoints[name]
	c.mu.Unlock()
	if ep != nil && reflect.DeepEqual(ep.cfg, *ua) && ep.url == sc.URL {
		return ep, nil
	}
	ep = &endpoints{cfg: *ua, url: sc.URL, auth: ua.AuthorizationURL, token: ua.TokenURL, revoke: ua.RevocationURL}
	if ep.auth == "" || ep.token == "" {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var prm struct {
			AuthorizationServers []string `json:"authorization_servers"`
		}
		if err := c.getMetadata(ctx, sc.URL, "oauth-protected-resource", &prm); err != nil || len(prm.AuthorizationServers) == 0 {
			return nil, fmt.Errorf("discovering the authorization server of %s failed (set user_auth.authorization_url and token_url): %v", name, err)
		}
		var asm struct {
			AuthorizationEndpoint string `json:"authorization_endpoint"`
			TokenEndpoint         string `json:"token_endpoint"`
			RevocationEndpoint    string `json:"revocation_endpoint"`
		}
		as := prm.AuthorizationServers[0]
		if err := c.getMetadata(ctx, as, "oauth-authorization-server", &asm); err != nil || asm.TokenEndpoint == "" {
			if err := c.getMetadata(ctx, as, "openid-configuration", &asm); err != nil {
				return nil, fmt.Errorf("reading the metadata of authorization server %s failed: %w", as, err)
			}
		}
		if ep.auth == "" {
			ep.auth = asm.AuthorizationEndpoint
		}
		if ep.token == "" {
			ep.token = asm.TokenEndpoint
		}
		if ep.revoke == "" {
			ep.revoke = asm.RevocationEndpoint
		}
		if ep.auth == "" || ep.token == "" {
			return nil, fmt.Errorf("the authorization server of %s publishes no authorization or token endpoint", name)
		}
	}
	c.mu.Lock()
	c.endpoints[name] = ep
	c.mu.Unlock()
	return ep, nil
}

// getMetadata reads a well-known document for a URL: first with the
// URL's path inserted after the well-known segment, then at the root.
func (c *connections) getMetadata(ctx context.Context, rawURL, suffix string, out any) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	var candidates []string
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		candidates = append(candidates, u.Scheme+"://"+u.Host+"/.well-known/"+suffix+p)
	}
	candidates = append(candidates, u.Scheme+"://"+u.Host+"/.well-known/"+suffix)
	var last error
	for _, cand := range candidates {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cand, nil)
		req.Header.Set("Accept", "application/json")
		resp, err := (&http.Client{Transport: c.s.client.Transport}).Do(req)
		if err != nil {
			last = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err == nil && resp.StatusCode == http.StatusOK && json.Unmarshal(body, out) == nil {
			return nil
		}
		last = fmt.Errorf("GET %s: status %d", cand, resp.StatusCode)
	}
	return last
}

// revoke asks the upstream to revoke a token (RFC 7009), when it has a
// revocation endpoint. Failure only means the token lives until it
// expires, so it is logged.
func (c *connections) revoke(ctx context.Context, server string, conn *store.MCPConnection) {
	oc, ep, err := c.oauthConfig(ctx, server)
	if err != nil || ep.revoke == "" {
		return
	}
	box, err := secretBox(c.s.rt.Load().cfg.Secrets)
	if err != nil {
		return
	}
	token, hint := conn.RefreshToken, "refresh_token"
	if token == "" {
		token, hint = conn.AccessToken, "access_token"
	}
	plain, err := box.Open(token, connAD(server, conn.Subject, strings.TrimSuffix(hint, "_token")))
	if err != nil {
		return
	}
	form := url.Values{"token": {string(plain)}, "token_type_hint": {hint}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ep.revoke, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(oc.ClientID), url.QueryEscape(oc.ClientSecret))
	resp, err := (&http.Client{Transport: c.s.client.Transport, Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		c.s.log.Warn("revoking an MCP user token failed", "server", server, "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.s.log.Warn("revoking an MCP user token failed", "server", server, "status", resp.StatusCode)
	}
}

// --- the connect flow in the browser ---

// verifySubject reports whether the connect page signs the user in to
// check their identity: SSO must be configured with the identity
// provider that issues MCP tokens, or the subjects would not compare.
func verifySubject(cfg *config.Config) bool {
	sso, mo := cfg.SSO.OIDC, cfg.MCP.OAuth
	return sso != nil && mo != nil && strings.TrimRight(sso.Issuer, "/") == strings.TrimRight(mo.Issuer, "/")
}

// connectStart serves the link a user opens: it checks who they are
// when it can, then sends them to the upstream's consent page.
func (s *Server) connectStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cr, err := s.store.GetConnectRequest(r.Context(), hashID(id), time.Now())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("reading a connect request failed", "err", err)
		}
		connectPage(w, http.StatusGone, "This link has expired or was already used",
			"Go back to your MCP client and try again; it will give you a new link.")
		return
	}
	cfg := s.rt.Load().cfg
	if verifySubject(cfg) {
		c, err := s.oidc(r.Context(), cfg.SSO.OIDC)
		if err != nil {
			s.log.Error("reaching the identity provider failed", "err", err)
			connectPage(w, http.StatusBadGateway, "The identity provider cannot be reached", "Try again shortly.")
			return
		}
		s.signIn(w, r, cfg.SSO.OIDC, c, &authFlow{Return: "/ui/", Connect: id})
		return
	}
	s.connectUpstream(w, r, id, cr)
}

// connectSignedIn continues a connect flow once the identity provider
// says who the browser's user is. Only the user whose MCP call asked for
// the connection may complete it.
func (s *Server) connectSignedIn(w http.ResponseWriter, r *http.Request, id, subject string) {
	cr, err := s.store.GetConnectRequest(r.Context(), hashID(id), time.Now())
	if err != nil {
		connectPage(w, http.StatusGone, "This link has expired or was already used",
			"Go back to your MCP client and try again; it will give you a new link.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(subject), []byte(cr.Subject)) != 1 {
		s.metrics.ObserveConnectFlow(cr.Server, "denied")
		s.log.Warn("connect link opened by another user", "server", cr.Server, "subject", cr.Subject, "signed_in", subject)
		connectPage(w, http.StatusForbidden, "This link belongs to someone else",
			"A connect link only works for the person whose MCP client asked for it. Ask your client for a new link.")
		return
	}
	s.connectUpstream(w, r, id, cr)
}

// connectUpstream marks the request opened and redirects to the
// upstream's authorization endpoint with PKCE.
func (s *Server) connectUpstream(w http.ResponseWriter, r *http.Request, id string, cr *store.ConnectRequest) {
	oc, _, err := s.conns.oauthConfig(r.Context(), cr.Server)
	if err != nil {
		s.log.Error("preparing the connect flow failed", "server", cr.Server, "err", err)
		connectPage(w, http.StatusBadGateway, "The "+cr.Server+" sign-in cannot be reached", "Try again shortly, or ask an administrator.")
		return
	}
	state := randomString()
	if err := s.store.OpenConnectRequest(r.Context(), hashID(id), hashID(state), time.Now()); err != nil {
		connectPage(w, http.StatusGone, "This link has expired or was already used",
			"Go back to your MCP client and try again; it will give you a new link.")
		return
	}
	// The callback must come back to this browser.
	setCookie(w, r, connectCookie, state, connectTTL)
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(cr.Verifier)}
	if sc, ok := s.rt.Load().cfg.MCP.Servers[cr.Server]; ok {
		opts = append(opts, oauth2.SetAuthURLParam("resource", sc.URL)) // RFC 8707
	}
	http.Redirect(w, r, oc.AuthCodeURL(state, opts...), http.StatusFound)
}

// connectCallback finishes the flow: it exchanges the code and stores the
// user's tokens.
func (s *Server) connectCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	cookie := readCookie(r, connectCookie)
	setCookie(w, r, connectCookie, "", -1)
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie)) != 1 {
		connectPage(w, http.StatusBadRequest, "This sign-in was started in another browser",
			"Open the connect link from your MCP client again, in this browser.")
		return
	}
	cr, err := s.store.TakeConnectRequest(r.Context(), hashID(state), time.Now())
	if err != nil {
		s.metrics.ObserveConnectFlow("unknown", "expired")
		connectPage(w, http.StatusGone, "This sign-in has expired",
			"Go back to your MCP client and try again; it will give you a new link.")
		return
	}
	if e := q.Get("error"); e != "" {
		s.metrics.ObserveConnectFlow(cr.Server, "denied")
		connectPage(w, http.StatusForbidden, cr.Server+" did not allow the connection", "The sign-in answered: "+e+".")
		return
	}
	oc, _, err := s.conns.oauthConfig(r.Context(), cr.Server)
	if err != nil {
		connectPage(w, http.StatusBadGateway, "The "+cr.Server+" sign-in cannot be reached", "Try again shortly.")
		return
	}
	tok, err := oc.Exchange(s.conns.oauthContext(r.Context()), q.Get("code"), oauth2.VerifierOption(cr.Verifier))
	if err != nil {
		s.metrics.ObserveConnectFlow(cr.Server, "error")
		s.log.Warn("exchanging an MCP connect code failed", "server", cr.Server, "err", err)
		connectPage(w, http.StatusBadGateway, cr.Server+" did not accept the sign-in", "Go back to your MCP client and try again.")
		return
	}
	scopes := []string{}
	if sc, ok := s.rt.Load().cfg.MCP.Servers[cr.Server]; ok && sc.UserAuth != nil {
		scopes = sc.UserAuth.Scopes
	}
	ctx := context.WithoutCancel(r.Context())
	if err := s.conns.save(ctx, cr.Server, cr.Subject, scopes, tok); err != nil {
		s.log.Error("saving an MCP connection failed", "server", cr.Server, "err", err)
		connectPage(w, http.StatusInternalServerError, "The connection could not be saved", "Try again shortly.")
		return
	}
	s.metrics.ObserveConnectFlow(cr.Server, "completed")
	details, _ := json.Marshal(map[string]any{"server": cr.Server, "subject": cr.Subject, "virtual_server": cr.VirtualServer})
	if err := s.store.AppendAudit(ctx, &store.AuditEntry{
		Actor: cr.Subject, Action: "mcp.connection.create", ResourceType: "mcp_connection", ResourceID: cr.Server + "/" + cr.Subject, Details: details,
	}); err != nil {
		s.log.Error("writing audit entry failed", "action", "mcp.connection.create", "err", err)
	}
	s.log.Info("MCP connection created", "server", cr.Server, "subject", cr.Subject)
	go s.mcp.RefreshServer(context.Background(), cr.Server)
	connectPage(w, http.StatusOK, "Your "+cr.Server+" account is connected",
		"You can close this page and go back to your MCP client. Try your request again.")
}

var connectTmpl = template.Must(template.New("connect").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#f5f6f8;color:#1d2330}
main{background:#fff;border:1px solid #e3e6eb;border-radius:10px;padding:32px;max-width:440px;margin:16px}h1{font-size:20px;margin:0 0 8px}p{margin:0;color:#525b6b}
@media (prefers-color-scheme:dark){body{background:#14171c;color:#e6e8eb}main{background:#1c2027;border-color:#2c313a}p{color:#a3abb8}}</style>
</head><body><main><h1>{{.Title}}</h1><p>{{.Text}}</p></main></body></html>`))

// connectPage renders a small page for the connect flow.
func connectPage(w http.ResponseWriter, status int, title, text string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = connectTmpl.Execute(w, map[string]string{"Title": title, "Text": text})
}

// --- admin API ---

func (s *Server) listMCPConnections(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	pg, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	items, more, err := s.store.ListMCPConnections(r.Context(), q.Get("server"), q.Get("subject"), pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}

// deleteMCPConnection revokes a connection at the upstream when it can,
// then deletes it.
func (s *Server) deleteMCPConnection(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	ctx := r.Context()
	server, subject := r.PathValue("server"), r.PathValue("subject")
	conn, err := s.store.GetMCPConnection(ctx, server, subject)
	if err != nil {
		return err
	}
	s.conns.revoke(ctx, server, conn)
	if err := s.store.DeleteMCPConnection(ctx, server, subject); err != nil {
		return err
	}
	s.conns.forget(connKey{server, subject})
	if err := s.audit(ctx, "mcp.connection.delete", "mcp_connection", server+"/"+subject, "", map[string]any{"server": server, "subject": subject}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// connectionsLoop prunes expired connect requests and updates the
// connection gauge, hourly.
func (s *Server) connectionsLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if _, err := s.store.PruneConnectRequests(ctx, time.Now()); err != nil && ctx.Err() == nil {
			s.log.Error("pruning MCP connect requests failed", "err", err)
		}
		if counts, err := s.store.CountMCPConnections(ctx); err == nil {
			for name, sc := range s.rt.Load().cfg.MCP.Servers {
				if sc.UserAuth != nil {
					s.metrics.SetMCPConnections(name, counts[name])
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
