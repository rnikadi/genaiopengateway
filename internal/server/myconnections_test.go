package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

func TestMyMCPConnections(t *testing.T) {
	up := newUserUpstream(t)
	e := newAdminEnv(t, "")
	var org idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	parse := func(sso string) *config.Config {
		t.Helper()
		cfg, err := config.Parse([]byte(`
admin: {token: ` + adminToken + `}
store: {driver: sqlite, dsn: unused}
secrets: {encryption_key: "` + testEncryptionKey + `"}
` + sso + `
mcp:
  public_url: http://gw.example
  oauth: {issuer: "https://idp.example", organization: ` + org.ID + `, team_claim: groups}
  servers:
    gh: {url: "` + up.URL + `/mcp", user_auth: {client_id: gw-client, client_secret: gw-secret, scopes: [repo], authorization_url: "` + up.URL + `/authorize", token_url: "` + up.URL + `/token", revocation_url: "` + up.URL + `/revoke"}}
    linear: {url: "` + up.URL + `/mcp", user_auth: {client_id: gw-client, client_secret: gw-secret, authorization_url: "` + up.URL + `/authorize", token_url: "` + up.URL + `/token"}}
  virtual_servers:
    dev: {servers: [{server: gh}, {server: linear}]}
`))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	// Without sign-in at the MCP identity provider the page says so.
	if err := e.srv.Reload(parse("")); err != nil {
		t.Fatal(err)
	}
	if rec := do(e.h, "GET", "/mcp/connections", "", ""); rec.Code != 404 || !strings.Contains(rec.Body.String(), "not set up") {
		t.Fatalf("without sign-in: %d %s", rec.Code, rec.Body)
	}
	cfg := parse(`sso: {oidc: {issuer: "https://idp.example/", client_id: gw, client_secret: s3cret, redirect_url: "http://gw.example/ui/auth/callback"}, roles: [{email: o@example.com, role: owner}]}`)
	if err := e.srv.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	if got := myConnectionsURL(cfg); got != "http://gw.example/mcp/connections" {
		t.Fatalf("page URL: %q", got)
	}
	ctx := context.Background()
	for _, c := range [][2]string{{"gh", "alice-sub"}, {"gh", "bob-sub"}} {
		// The upstream knows the tokens, so a catalog refresh with one of
		// them does not end the connection.
		up.set(func(u *userUpstream) { u.access["at-"+c[1]], u.refresh["rt-"+c[1]] = c[1], c[1] })
		if err := e.srv.conns.save(ctx, c[0], c[1], []string{"repo"}, &oauth2.Token{AccessToken: "at-" + c[1], RefreshToken: "rt-" + c[1], Expiry: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	me := &myConn{Subject: "alice-sub", Email: "alice@example.com", CSRF: "tok", Expires: time.Now().Add(time.Minute).Unix()}
	req := func(method, path, cookie, origin string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://gw.example"+path, strings.NewReader(form.Encode()))
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: myConnCookie, Value: cookie})
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		return rec
	}
	good := sealMyConn(cfg.SSO.OIDC, me)

	// The page lists only the signed-in user's connections, never tokens.
	rec := req("GET", "/mcp/connections", good, "", nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "alice@example.com") || !strings.Contains(body, "Disconnect gh") || !strings.Contains(body, "linear") {
		t.Fatalf("page: %d %s", rec.Code, body)
	}
	for _, leak := range []string{"bob-sub", "at-alice", "rt-alice"} {
		if strings.Contains(body, leak) {
			t.Fatalf("the page shows %q", leak)
		}
	}

	// A cookie that is forged, expired or signed for another purpose does not sign in.
	expired := *me
	expired.Expires = time.Now().Add(-time.Minute).Unix()
	flow := sealFlow(cfg.SSO.OIDC, &authFlow{State: "alice-sub", Expires: time.Now().Add(time.Minute).Unix()})
	for name, cookie := range map[string]string{"forged": good[:len(good)-2] + "xx", "expired": sealMyConn(cfg.SSO.OIDC, &expired), "flow cookie": flow} {
		if rec := req("POST", "/mcp/connections/gh/disconnect", cookie, "http://gw.example", url.Values{"csrf": {"tok"}}); rec.Code != 401 {
			t.Fatalf("%s cookie: %d", name, rec.Code)
		}
	}
	// Another site cannot make the browser disconnect.
	for name, c := range map[string][2]string{"other origin": {"http://evil.example", "tok"}, "no origin": {"", "tok"}, "wrong token": {"http://gw.example", "nope"}} {
		if rec := req("POST", "/mcp/connections/gh/disconnect", good, c[0], url.Values{"csrf": {c[1]}}); rec.Code != 403 {
			t.Fatalf("%s: %d", name, rec.Code)
		}
	}
	if _, err := e.srv.store.GetMCPConnection(ctx, "gh", "alice-sub"); err != nil {
		t.Fatalf("the connection was removed by a refused request: %v", err)
	}

	// Disconnecting revokes at the upstream, deletes the row and is audited.
	rec = req("POST", "/mcp/connections/gh/disconnect", good, "http://gw.example", url.Values{"csrf": {"tok"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/mcp/connections?disconnected=gh" {
		t.Fatalf("disconnect: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if _, err := e.srv.store.GetMCPConnection(ctx, "gh", "alice-sub"); err != store.ErrNotFound {
		t.Fatalf("alice's connection after disconnect: %v", err)
	}
	if _, err := e.srv.store.GetMCPConnection(ctx, "gh", "bob-sub"); err != nil {
		t.Fatalf("bob's connection: %v", err)
	}
	up.mu.Lock()
	revoked := strings.Join(up.revoked, ",")
	up.mu.Unlock()
	if !strings.Contains(revoked, "rt-alice-sub") {
		t.Fatalf("revoked at the upstream: %q", revoked)
	}
	var audit struct {
		Data []store.AuditEntry `json:"data"`
	}
	e.call("GET", "/admin/v1/audit?limit=1", "", 200, &audit)
	if len(audit.Data) != 1 || audit.Data[0].Action != "mcp.connection.delete" || audit.Data[0].Actor != "alice-sub" || !strings.Contains(string(audit.Data[0].Details), `"by":"user"`) {
		t.Fatalf("audit: %+v", audit.Data)
	}
	rec = req("GET", "/mcp/connections?disconnected=gh", good, "", nil)
	if body := rec.Body.String(); !strings.Contains(body, "Disconnected gh") || !strings.Contains(body, "not connected any account") {
		t.Fatalf("page after disconnect: %s", body)
	}
	// Disconnecting again changes nothing.
	if rec := req("POST", "/mcp/connections/gh/disconnect", good, "http://gw.example", url.Values{"csrf": {"tok"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("second disconnect: %d", rec.Code)
	}
}
