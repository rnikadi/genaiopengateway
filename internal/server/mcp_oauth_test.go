package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// fakeIssuer is an OpenID provider that signs tokens with one RSA key.
type fakeIssuer struct {
	*httptest.Server
	key *rsa.PrivateKey
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.URL, "jwks_uri": is.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	is.Server = httptest.NewServer(mux)
	t.Cleanup(is.Close)
	return is
}

func (is *fakeIssuer) token(t *testing.T, aud string, mod func(jwt.MapClaims)) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": is.URL, "sub": "user-1", "aud": aud, "exp": time.Now().Add(time.Hour).Unix(),
		"scope": "mcp", "groups": []any{"unrelated", "search"},
	}
	if mod != nil {
		mod(c)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(is.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMCPOAuth(t *testing.T) {
	up := newMCPUpstream(t, "alpha", true)
	is := newFakeIssuer(t)
	mcpCfg := `
mcp:
  servers: {alpha: {url: "` + up.URL + `"}}
  virtual_servers:
    eng: {servers: [{server: alpha}]}
    solo: {servers: [{server: alpha}]}
`
	e := newAdminEnv(t, mcpCfg)
	gw := httptest.NewServer(e.h)
	t.Cleanup(gw.Close)

	var org, team, locked idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"search"}`, 201, &team)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"locked","allowed_mcp_servers":["solo"]}`, 201, &locked)
	cfg, err := config.Parse([]byte(`
admin: {token: ` + adminToken + `}
store: {driver: sqlite, dsn: unused}
` + mcpCfg + `
  public_url: ` + gw.URL + `
  oauth:
    issuer: ` + is.URL + `
    organization: ` + org.ID + `
    team_claim: groups
    scopes: [mcp]
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e.srv.mcp.Refresh(ctx)
	resource := gw.URL + "/mcp/eng"

	post := func(token string) *http.Response {
		req, _ := http.NewRequest("POST", resource, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	metadataURL := gw.URL + "/.well-known/oauth-protected-resource/mcp/eng"
	resp := post("")
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != `Bearer resource_metadata="`+metadataURL+`", scope="mcp"` {
		t.Fatalf("no token: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	mr, err := http.Get(metadataURL)
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	_ = json.NewDecoder(mr.Body).Decode(&meta)
	mr.Body.Close()
	if meta.Resource != resource || len(meta.AuthorizationServers) != 1 || meta.AuthorizationServers[0] != is.URL || meta.ScopesSupported[0] != "mcp" {
		t.Fatalf("protected resource metadata %+v", meta)
	}
	if r, _ := http.Get(gw.URL + "/.well-known/oauth-protected-resource/mcp/nope"); r.StatusCode != 404 {
		t.Fatalf("metadata for an unknown server: %d", r.StatusCode)
	}

	for name, tc := range map[string]struct {
		token  string
		status int
		auth   string
	}{
		"other audience":   {is.token(t, gw.URL+"/mcp/solo", nil), 401, `error="invalid_token"`},
		"no scope":         {is.token(t, resource, func(c jwt.MapClaims) { c["scope"] = "openid" }), 403, `error="insufficient_scope"`},
		"no team":          {is.token(t, resource, func(c jwt.MapClaims) { c["groups"] = "strangers" }), 403, ""},
		"team not allowed": {is.token(t, resource, func(c jwt.MapClaims) { c["groups"] = "locked" }), 403, ""},
		"team by id":       {is.token(t, resource, func(c jwt.MapClaims) { c["groups"] = team.ID }), 200, ""},
		"gateway key":      {devKey, 401, ""},
	} {
		resp := post(tc.token)
		if resp.StatusCode != tc.status || !strings.Contains(resp.Header.Get("WWW-Authenticate"), tc.auth) {
			t.Errorf("%s: %d %q, want %d with %q", name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"), tc.status, tc.auth)
		}
	}

	cs := mcpClient(t, resource, is.token(t, resource+"/", nil), nil)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "alpha_echo", Arguments: map[string]any{"text": "sso"}})
	if err != nil || text(t, res.Content) != "alpha:sso" {
		t.Fatalf("call with an access token: %v, %v", res, err)
	}
	var calls struct {
		Data []struct {
			OrgID   string `json:"org_id"`
			TeamID  string `json:"team_id"`
			Subject string
			Status  string
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		e.call("GET", "/admin/v1/mcp/calls?org_id="+org.ID, "", 200, &calls)
		if len(calls.Data) >= 2 {
			break
		}
	}
	var sawCall, sawDenied bool
	for _, c := range calls.Data {
		if c.Subject != "user-1" {
			t.Errorf("call record without the token subject: %+v", c)
		}
		sawCall = sawCall || (c.TeamID == team.ID && c.Status == "ok")
		sawDenied = sawDenied || (c.TeamID == locked.ID && c.Status == "denied")
	}
	if !sawCall || !sawDenied {
		t.Fatalf("call records %+v", calls.Data)
	}
}
