package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

type portalView struct {
	Owner string
	Teams []struct {
		ID, Name, OrgName string
		Models            []struct{ Name string }
		MCPServers        []struct {
			Name, Access string
			Pending      bool
		} `json:"mcp_servers"`
		Keys []struct {
			ID, Name, Owner string
			ExpiresAt       *time.Time `json:"expires_at"`
			RevokedAt       *time.Time `json:"revoked_at"`
		}
	}
}

func TestDeveloperPortal(t *testing.T) {
	e := newAdminEnv(t, `
mcp:
  servers:
    up: {url: "http://127.0.0.1:9/mcp"}
  virtual_servers:
    docs: {description: Docs search, servers: [{server: up}]}
    wiki: {servers: [{server: up}]}
sso:
  portal: {max_keys_per_user: 2, max_key_ttl: 30d}
`)
	var acme, search, ads, adminKey idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","allowed_models":["chat","embed"]}`, 201, &acme)
	e.call("POST", "/admin/v1/orgs/"+acme.ID+"/teams", `{"name":"search","allowed_models":["chat"],"allowed_mcp_servers":["wiki"]}`, 201, &search)
	e.call("POST", "/admin/v1/orgs/"+acme.ID+"/teams", `{"name":"ads"}`, 201, &ads)
	e.call("POST", "/admin/v1/teams/"+search.ID+"/keys", `{"name":"service","owner":"ada@example.com"}`, 201, &adminKey)

	dev := e.session("Ada@Example.com", store.Grant{Role: config.RoleDeveloper, OrgID: acme.ID, TeamID: search.ID})
	other := e.session("bob@example.com", store.Grant{Role: config.RoleDeveloper, OrgID: acme.ID, TeamID: search.ID})
	viewer := e.session("vic@example.com", store.Grant{Role: config.RoleViewer, OrgID: acme.ID})

	// The admin UI loads these for every signed-in person.
	for _, path := range []string{"/admin/v1/me", "/admin/v1/models", "/admin/v1/mcp/virtual-servers", "/admin/v1/orgs?limit=500"} {
		if rec := e.browser(dev, "GET", path, ""); rec.Code != 200 {
			t.Errorf("a developer loading %s = %d %s", path, rec.Code, rec.Body)
		}
	}

	// The developer sees only their team, its models and their keys.
	var v portalView
	rec := e.browser(dev, "GET", "/admin/v1/portal", "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("portal = %d %s", rec.Code, rec.Body)
	}
	if v.Owner != "ada@example.com" || len(v.Teams) != 1 || v.Teams[0].Name != "search" || len(v.Teams[0].Models) != 1 || v.Teams[0].Models[0].Name != "chat" {
		t.Fatalf("portal view = %+v", v)
	}
	if ms := v.Teams[0].MCPServers; len(ms) != 2 || ms[0].Name != "docs" || ms[0].Access != accessAvailable || ms[1].Access != accessAllowed {
		t.Fatalf("MCP servers = %+v", ms)
	}
	if k := v.Teams[0].Keys; len(k) != 1 || k[0].ID != adminKey.ID {
		t.Fatalf("keys = %+v", k)
	}

	// Creating keys: within the team, the expiry ceiling and the count.
	var created struct {
		ID, Key, Owner string
		ExpiresAt      time.Time `json:"expires_at"`
		AllowedModels  []string  `json:"allowed_models"`
	}
	rec = e.browser(dev, "POST", "/admin/v1/portal/teams/"+search.ID+"/keys", `{"name":"laptop","expires_in":"7d"}`)
	if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &created) != nil || !strings.HasPrefix(created.Key, "sk-") || created.Owner != "ada@example.com" {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if d := time.Until(created.ExpiresAt); d < 6*24*time.Hour || d > 7*24*time.Hour {
		t.Errorf("expires in %s, want 7d", d)
	}
	for _, tc := range []struct {
		who, cookie, path, body string
		want                    int
	}{
		{"too long", dev, "/admin/v1/portal/teams/" + search.ID + "/keys", `{"name":"x","expires_in":"31d"}`, 400},
		{"no name", dev, "/admin/v1/portal/teams/" + search.ID + "/keys", `{"name":" "}`, 400},
		{"another team", dev, "/admin/v1/portal/teams/" + ads.ID + "/keys", `{"name":"x"}`, 403},
		{"a viewer", viewer, "/admin/v1/portal/teams/" + search.ID + "/keys", `{"name":"x"}`, 403},
		{"over the limit", dev, "/admin/v1/portal/teams/" + search.ID + "/keys", `{"name":"third"}`, 409},
		{"the token", "", "/admin/v1/portal/teams/" + search.ID + "/keys", `{"name":"x"}`, 401},
	} {
		if rec := e.browser(tc.cookie, "POST", tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.who, rec.Code, rec.Body, tc.want)
		}
	}
	if rec := do(e.h, "POST", "/admin/v1/portal/teams/"+search.ID+"/keys", adminToken, `{"name":"x"}`); rec.Code != 403 {
		t.Errorf("the admin token, which has no email = %d", rec.Code)
	}

	// The new key works, for the team's models only.
	if rec := chat(e.h, created.Key, "chat"); rec.Code != 200 {
		t.Fatalf("chat with the portal key = %d %s", rec.Code, rec.Body)
	}
	if rec := chat(e.h, created.Key, "embed"); rec.Code != http.StatusForbidden {
		t.Errorf("a model outside the team's policy = %d", rec.Code)
	}

	// Only the owner rotates or revokes; others do not learn the key exists.
	for _, c := range []string{other, viewer} {
		if rec := e.browser(c, "POST", "/admin/v1/portal/keys/"+created.ID+"/revoke", ""); rec.Code != 404 {
			t.Errorf("revoking someone else's key = %d", rec.Code)
		}
	}
	var rotated idResp
	rec = e.browser(dev, "POST", "/admin/v1/portal/keys/"+created.ID+"/rotate", `{"grace":"0s"}`)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &rotated) != nil || rotated.Key == created.Key {
		t.Fatalf("rotate = %d %s", rec.Code, rec.Body)
	}
	if rec := e.browser(dev, "POST", "/admin/v1/portal/keys/"+created.ID+"/rotate", `{"grace":"30d"}`); rec.Code != 400 {
		t.Errorf("a 30d grace = %d", rec.Code)
	}
	if rec := chat(e.h, rotated.Key, "chat"); rec.Code != 200 {
		t.Errorf("the rotated key = %d", rec.Code)
	}
	if rec := e.browser(dev, "POST", "/admin/v1/portal/keys/"+created.ID+"/revoke", ""); rec.Code != 200 {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if rec := chat(e.h, rotated.Key, "chat"); rec.Code != http.StatusUnauthorized {
		t.Errorf("a revoked key = %d", rec.Code)
	}
	// Revoking frees a place under the limit.
	if rec := e.browser(dev, "POST", "/admin/v1/portal/teams/"+search.ID+"/keys", `{"name":"again"}`); rec.Code != 201 {
		t.Errorf("create after revoking = %d %s", rec.Code, rec.Body)
	}

	// Access requests go to the MCP request queue.
	if rec := e.browser(dev, "POST", "/admin/v1/portal/teams/"+search.ID+"/requests", `{"server":"docs","reason":"search the docs"}`); rec.Code != 201 {
		t.Fatalf("request = %d %s", rec.Code, rec.Body)
	}
	rec = e.browser(dev, "GET", "/admin/v1/portal", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if !v.Teams[0].MCPServers[0].Pending {
		t.Errorf("the request is not shown as pending: %+v", v.Teams[0].MCPServers)
	}
	var reqs struct {
		Data []struct {
			Server      string
			RequestedBy string `json:"requested_by"`
		}
	}
	e.call("GET", "/admin/v1/mcp/requests?status=pending", "", 200, &reqs)
	if len(reqs.Data) != 1 || reqs.Data[0].RequestedBy != "ada@example.com" {
		t.Fatalf("requests = %+v", reqs)
	}
	var audit struct {
		Data []struct{ Action, Actor string }
	}
	e.call("GET", "/admin/v1/audit", "", 200, &audit)
	found := false
	for _, a := range audit.Data {
		found = found || (a.Action == "key.revoke" && strings.EqualFold(a.Actor, "ada@example.com"))
	}
	if !found {
		t.Errorf("the revoke is not in the audit log as the developer: %+v", audit.Data)
	}
}

func TestDeveloperRoleNeedsAnOrg(t *testing.T) {
	_, err := config.Parse([]byte(`
store: {driver: sqlite, dsn: x}
sso:
  oidc: {issuer: "https://idp.example.com", client_id: c, client_secret: s, redirect_url: "https://gw.example.com/ui/auth/callback"}
  roles: [{group: devs, role: developer}]
`))
	if err == nil || !strings.Contains(err.Error(), "developer needs an org") {
		t.Fatalf("err = %v", err)
	}
}

func chat(h http.Handler, key, model string) *httptest.ResponseRecorder {
	return do(h, "POST", "/v1/chat/completions", key, `{"model":"`+model+`"}`)
}
