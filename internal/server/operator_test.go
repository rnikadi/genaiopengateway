package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// session stores a session for the given grants and returns its cookie value.
func (e *adminEnv) session(email string, grants ...store.Grant) string {
	e.t.Helper()
	value := "cookie-" + email
	now := time.Now()
	if err := e.srv.store.CreateSession(context.Background(), &store.Session{
		IDHash: hashSession(value), Subject: "sub-" + email, Email: email, Grants: grants,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		e.t.Fatal(err)
	}
	return value
}

// browser makes a same-origin request with a session cookie, as the UI does.
func (e *adminEnv) browser(cookie, method, path, body string, header ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] == "" {
			req.Header.Del(header[i])
		} else {
			req.Header.Set(header[i], header[i+1])
		}
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestOperatorRoles(t *testing.T) {
	e := newAdminEnv(t, "")
	var acme, globex, acmeTeam, globexTeam, globexKey idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &acme)
	e.call("POST", "/admin/v1/orgs", `{"name":"globex"}`, 201, &globex)
	e.call("POST", "/admin/v1/orgs/"+acme.ID+"/teams", `{"name":"search"}`, 201, &acmeTeam)
	e.call("POST", "/admin/v1/orgs/"+globex.ID+"/teams", `{"name":"ads"}`, 201, &globexTeam)
	e.call("POST", "/admin/v1/teams/"+globexTeam.ID+"/keys", `{"name":"ci"}`, 201, &globexKey)

	owner := e.session("owner@example.com", store.Grant{Role: config.RoleOwner})
	viewAll := e.session("auditor@example.com", store.Grant{Role: config.RoleViewer})
	acmeAdmin := e.session("lead@example.com", store.Grant{Role: config.RoleAdmin, OrgID: acme.ID})
	acmeViewer := e.session("analyst@example.com", store.Grant{Role: config.RoleViewer, OrgID: acme.ID})

	for _, tc := range []struct {
		who          string
		cookie       string
		method, path string
		body         string
		want         int
	}{
		{"acme admin", acmeAdmin, "PATCH", "/admin/v1/orgs/" + acme.ID, `{"rpm":10}`, 200},
		{"acme admin", acmeAdmin, "PATCH", "/admin/v1/orgs/" + globex.ID, `{"rpm":10}`, 403},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/orgs/" + globex.ID, "", 403},
		{"acme admin", acmeAdmin, "POST", "/admin/v1/orgs", `{"name":"x"}`, 403},
		{"acme admin", acmeAdmin, "DELETE", "/admin/v1/orgs/" + acme.ID, "", 403},
		{"acme admin", acmeAdmin, "POST", "/admin/v1/orgs/" + acme.ID + "/teams", `{"name":"ml"}`, 201},
		{"acme admin", acmeAdmin, "POST", "/admin/v1/orgs/" + globex.ID + "/teams", `{"name":"ml"}`, 403},
		{"acme admin", acmeAdmin, "POST", "/admin/v1/teams/" + acmeTeam.ID + "/keys", `{"name":"dev"}`, 201},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/teams/" + globexTeam.ID + "/keys", "", 403},
		{"acme admin", acmeAdmin, "POST", "/admin/v1/keys/" + globexKey.ID + "/revoke", "", 403},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/usage", "", 403},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/usage?org_id=" + acme.ID, "", 200},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/usage?team_id=" + globexTeam.ID, "", 403},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/usage?key_id=" + globexKey.ID, "", 403},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/audit?org_id=" + acme.ID, "", 200},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/audit", "", 403},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/mcp/calls?org_id=" + globex.ID, "", 403},
		{"acme admin", acmeAdmin, "POST", "/admin/v1/mcp/servers/s/tools/t/approve", `{"hash":"x"}`, 403},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/models", "", 200},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/providers", "", 200},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/guardrails", "", 200},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/alerts/webhooks", "", 403},
		{"acme admin", acmeAdmin, "GET", "/admin/v1/settings", "", 403},
		{"viewer", viewAll, "GET", "/admin/v1/settings", "", 403},
		{"owner", owner, "GET", "/admin/v1/settings", "", 200},
		{"owner", owner, "GET", "/admin/v1/alerts/webhooks", "", 200},
		{"acme viewer", acmeViewer, "GET", "/admin/v1/orgs/" + acme.ID, "", 200},
		{"acme viewer", acmeViewer, "GET", "/admin/v1/teams/" + acmeTeam.ID + "/keys", "", 200},
		{"acme viewer", acmeViewer, "PATCH", "/admin/v1/orgs/" + acme.ID, `{"rpm":1}`, 403},
		{"acme viewer", acmeViewer, "POST", "/admin/v1/teams/" + acmeTeam.ID + "/keys", `{"name":"x"}`, 403},
		{"viewer", viewAll, "GET", "/admin/v1/usage", "", 200},
		{"viewer", viewAll, "GET", "/admin/v1/audit", "", 200},
		{"viewer", viewAll, "PATCH", "/admin/v1/orgs/" + globex.ID, `{"rpm":1}`, 403},
		{"owner", owner, "POST", "/admin/v1/orgs", `{"name":"initech"}`, 201},
		{"nobody", "", "GET", "/admin/v1/orgs", "", 401},
		{"unknown cookie", "forged", "GET", "/admin/v1/orgs", "", 401},
	} {
		if rec := e.browser(tc.cookie, tc.method, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s: %s %s = %d, want %d: %s", tc.who, tc.method, tc.path, rec.Code, tc.want, rec.Body)
		}
	}

	var orgs struct{ Data []struct{ ID string } }
	_ = json.Unmarshal(e.browser(acmeAdmin, "GET", "/admin/v1/orgs", "").Body.Bytes(), &orgs)
	if len(orgs.Data) != 1 || orgs.Data[0].ID != acme.ID {
		t.Errorf("an org admin lists only their organizations: %+v", orgs)
	}
	_ = json.Unmarshal(e.browser(viewAll, "GET", "/admin/v1/orgs", "").Body.Bytes(), &orgs)
	if len(orgs.Data) != 3 {
		t.Errorf("a gateway-wide viewer lists every organization: %+v", orgs)
	}

	var audit struct {
		Data []struct{ Actor, Action string }
	}
	e.call("GET", "/admin/v1/audit", "", 200, &audit)
	if audit.Data[0].Actor != "owner@example.com" || audit.Data[0].Action != "org.create" {
		t.Errorf("audit names the person: %+v", audit.Data[0])
	}
}

func TestOperatorCSRF(t *testing.T) {
	e := newAdminEnv(t, "")
	var org idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	c := e.session("owner@example.com", store.Grant{Role: config.RoleOwner})
	path := "/admin/v1/orgs/" + org.ID
	for name, h := range map[string][]string{
		"no origin":      {"Origin", ""},
		"foreign origin": {"Origin", "https://evil.example"},
		"form post":      {"Content-Type", "application/x-www-form-urlencoded"},
		"text body":      {"Content-Type", "text/plain"},
	} {
		if rec := e.browser(c, "PATCH", path, `{"rpm":1}`, h...); rec.Code != 403 {
			t.Errorf("%s: %d, want 403", name, rec.Code)
		}
	}
	if rec := e.browser(c, "GET", path, "", "Origin", ""); rec.Code != 200 {
		t.Errorf("reads need no origin: %d", rec.Code)
	}
	if rec := e.browser(c, "PATCH", path, `{"rpm":1}`); rec.Code != 200 {
		t.Errorf("same-origin JSON: %d %s", rec.Code, rec.Body)
	}
	// The admin token is not a cookie, so it needs no CSRF check.
	e.call("PATCH", path, `{"rpm":2}`, 200, nil)
}

func TestTokenSignIn(t *testing.T) {
	e := newAdminEnv(t, "")
	if rec := e.browser("", "POST", "/ui/auth/token", `{"token":"wrong"}`); rec.Code != 401 {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	if rec := e.browser("", "POST", "/ui/auth/token", `{"token":"`+adminToken+`"}`, "Origin", "https://evil.example"); rec.Code != 403 {
		t.Fatalf("cross-origin sign-in: %d", rec.Code)
	}
	rec := e.browser("", "POST", "/ui/auth/token", `{"token":"`+adminToken+`"}`)
	if rec.Code != 204 {
		t.Fatalf("sign-in: %d %s", rec.Code, rec.Body)
	}
	c := sessionFrom(t, rec)
	var me struct {
		Operator struct {
			Subject string
			Grants  []store.Grant
		}
		SSO bool
	}
	if err := json.Unmarshal(e.browser(c, "GET", "/admin/v1/me", "").Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Operator.Subject != adminActor || len(me.Operator.Grants) != 1 || me.Operator.Grants[0].Role != config.RoleOwner || me.SSO {
		t.Fatalf("me: %+v", me)
	}
	if rec := e.browser(c, "POST", "/ui/auth/logout", ""); rec.Code != 204 {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := e.browser(c, "GET", "/admin/v1/me", ""); rec.Code != 401 {
		t.Fatalf("a signed-out session still works: %d", rec.Code)
	}
}

func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				t.Fatalf("session cookie attributes: %+v", c)
			}
			return c.Value
		}
	}
	t.Fatalf("no session cookie in %v", rec.Result().Header)
	return ""
}

func TestUIServing(t *testing.T) {
	e := newAdminEnv(t, "")
	rec := e.browser("", "GET", "/ui/", "")
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") || rec.Header().Get("ETag") == "" {
		t.Fatalf("index: %d %v", rec.Code, rec.Header())
	}
	if again := e.browser("", "GET", "/ui/", "", "If-None-Match", rec.Header().Get("ETag")); again.Code != 304 {
		t.Fatalf("revalidation: %d", again.Code)
	}
	if rec := e.browser("", "GET", "/ui/nope.js", ""); rec.Code != 404 {
		t.Fatalf("missing file: %d", rec.Code)
	}
	off := newAdminEnv(t, "ui: {enabled: false}")
	if rec := off.browser("", "GET", "/ui/", ""); rec.Code != 404 {
		t.Fatalf("disabled UI: %d", rec.Code)
	}
}

// fakeOIDC is an identity provider that signs in whoever the test says,
// checking PKCE as a real one would.
type fakeOIDC struct {
	*fakeIssuer
	mu    sync.Mutex
	codes map[string]fakeCode
}

type fakeCode struct {
	nonce, challenge string
	claims           jwt.MapClaims
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	f := &fakeOIDC{fakeIssuer: newFakeIssuer(t), codes: map[string]fakeCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.URL, "jwks_uri": f.URL + "/jwks", "authorization_endpoint": f.URL + "/authorize",
			"token_endpoint": f.URL + "/token", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.Handle("/jwks", f.Config.Handler)
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		c, ok := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		claims := jwt.MapClaims{"iss": f.URL, "aud": "gateway", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": c.nonce}
		for k, v := range c.claims {
			claims[k] = v
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = "k1"
		id, _ := tok.SignedString(f.key)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": id})
	})
	f.Config.Handler = mux
	return f
}

// authorize plays the browser at the identity provider: it reads the
// sign-in request and returns the callback URL with a code.
func (f *fakeOIDC) authorize(t *testing.T, location string, claims jwt.MapClaims) url.Values {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, f.URL+"/authorize") {
		t.Fatalf("redirect to %q", location)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "gateway" || q.Get("nonce") == "" {
		t.Fatalf("authorization request %v", q)
	}
	code := randomString()
	f.mu.Lock()
	f.codes[code] = fakeCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), claims: claims}
	f.mu.Unlock()
	return url.Values{"code": {code}, "state": {q.Get("state")}}
}

func TestOIDCSignIn(t *testing.T) {
	idp := newFakeOIDC(t)
	e := newAdminEnv(t, `
sso:
  oidc:
    issuer: `+idp.URL+`
    client_id: gateway
    client_secret: secret
    redirect_url: http://example.com/ui/auth/callback
  roles:
    - {group: platform, role: owner}
    - {group: search-leads, role: admin, org: acme}
    - {email: Analyst@Example.com, role: viewer}
`)
	var acme idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &acme)

	// signIn runs the flow and returns the callback response.
	signIn := func(ret string, claims jwt.MapClaims, tamper func(url.Values)) *httptest.ResponseRecorder {
		t.Helper()
		rec := e.browser("", "GET", "/ui/auth/login?return="+url.QueryEscape(ret), "")
		if rec.Code != 302 {
			t.Fatalf("login: %d %s", rec.Code, rec.Body)
		}
		var flow *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == authCookie {
				flow = c
			}
		}
		cb := idp.authorize(t, rec.Header().Get("Location"), claims)
		if tamper != nil {
			tamper(cb)
		}
		req := httptest.NewRequest("GET", "/ui/auth/callback?"+cb.Encode(), nil)
		req.AddCookie(flow)
		out := httptest.NewRecorder()
		e.h.ServeHTTP(out, req)
		return out
	}

	rec := signIn("/ui/#/orgs", jwt.MapClaims{"sub": "u1", "email": "lead@example.com", "name": "Lead", "groups": []any{"search-leads"}}, nil)
	if rec.Code != 302 || rec.Header().Get("Location") != "/ui/#/orgs" {
		t.Fatalf("callback: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	lead := sessionFrom(t, rec)
	var me struct {
		Operator struct {
			Email  string
			Grants []store.Grant
		}
		SSO bool
	}
	_ = json.Unmarshal(e.browser(lead, "GET", "/admin/v1/me", "").Body.Bytes(), &me)
	if me.Operator.Email != "lead@example.com" || len(me.Operator.Grants) != 1 || me.Operator.Grants[0] != (store.Grant{Role: config.RoleAdmin, OrgID: acme.ID}) || !me.SSO {
		t.Fatalf("me: %+v", me)
	}
	if rec := e.browser(lead, "PATCH", "/admin/v1/orgs/"+acme.ID, `{"rpm":5}`); rec.Code != 200 {
		t.Fatalf("org admin edits their org: %d", rec.Code)
	}

	rec = signIn("https://evil.example/", jwt.MapClaims{"sub": "u2", "email": "analyst@example.com"}, nil)
	if rec.Code != 302 || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("a foreign return target must be dropped: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	if rec := signIn("/ui/", jwt.MapClaims{"sub": "u3", "email": "stranger@example.com", "groups": []any{"sales"}}, nil); rec.Code != 403 {
		t.Fatalf("a user with no role: %d", rec.Code)
	}
	if rec := signIn("/ui/", jwt.MapClaims{"sub": "u4", "groups": "platform"}, func(v url.Values) { v.Set("state", "forged") }); rec.Code != 400 {
		t.Fatalf("a forged state: %d", rec.Code)
	}
	if rec := signIn("/ui/", jwt.MapClaims{"sub": "u5", "groups": "platform", "nonce": "replayed"}, nil); rec.Code != 401 {
		t.Fatalf("a token for another sign-in: %d", rec.Code)
	}
	if rec := signIn("/ui/", jwt.MapClaims{"sub": "u6", "groups": "platform"}, func(v url.Values) { v.Set("code", "guessed") }); rec.Code != 502 {
		t.Fatalf("an unknown code: %d", rec.Code)
	}
	if rec := e.browser("", "POST", "/ui/auth/token", `{"token":"`+adminToken+`"}`); rec.Code != 404 {
		t.Fatalf("token sign-in is off with OIDC: %d", rec.Code)
	}
	// The admin token still works for automation.
	e.call("GET", "/admin/v1/orgs", "", 200, nil)

	var audit struct {
		Data []struct{ Actor, Action string }
	}
	e.call("GET", "/admin/v1/audit", "", 200, &audit)
	var signIns []string
	for _, a := range audit.Data {
		if a.Action == "operator.sign_in" {
			signIns = append(signIns, a.Actor)
		}
	}
	if len(signIns) != 2 {
		t.Fatalf("sign-ins in the audit log: %v", signIns)
	}
}
