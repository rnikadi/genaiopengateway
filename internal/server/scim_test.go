package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/store"
)

const scimToken = "scim-token-0123456789abcdef0123456789"

func scimEnv(t *testing.T) *adminEnv {
	return newAdminEnv(t, `
sso:
  oidc: {issuer: "https://id.example.com", client_id: gw, client_secret: s, redirect_url: "http://example.com/ui/auth/callback"}
  roles: [{group: gateway-owners, role: owner}]
  scim: {token: `+scimToken+`}
`)
}

// scim calls the SCIM API as the identity provider and decodes the reply.
func (e *adminEnv) scim(method, path, body string, want int) map[string]any {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+scimToken)
	req.Header.Set("Content-Type", "application/scim+json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body)
	}
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
		}
	}
	return out
}

// ssoSession stores a session as an OIDC sign-in does, with the claims
// role mappings match.
func (e *adminEnv) ssoSession(email string, groups ...string) string {
	e.t.Helper()
	value := "cookie-sso-" + email
	now := time.Now()
	if err := e.srv.store.CreateSession(context.Background(), &store.Session{
		IDHash: hashSession(value), Subject: "sub-" + email, Email: email,
		Claims:    &store.SessionClaims{Email: email, Groups: groups},
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		e.t.Fatal(err)
	}
	return value
}

func TestSCIMAuth(t *testing.T) {
	off := newAdminEnv(t, "")
	if rec := do(off.h, "GET", "/scim/v2/Users", scimToken, ""); rec.Code != 404 {
		t.Fatalf("SCIM without a token configured = %d, want 404", rec.Code)
	}
	e := scimEnv(t)
	for _, key := range []string{"", "wrong", adminToken} {
		if rec := do(e.h, "GET", "/scim/v2/Users", key, ""); rec.Code != 401 || !strings.Contains(rec.Body.String(), scimErrorSchema) {
			t.Errorf("token %q = %d %s, want a SCIM 401", key, rec.Code, rec.Body)
		}
	}
	cfg := e.scim("GET", "/scim/v2/ServiceProviderConfig", "", 200)
	if cfg["patch"].(map[string]any)["supported"] != true {
		t.Errorf("service provider config: %v", cfg)
	}
	e.scim("GET", "/scim/v2/ResourceTypes", "", 200)
	e.scim("GET", "/scim/v2/Schemas", "", 200)
}

// TestSCIMOktaFlow follows what Okta sends: look a user up by userName,
// create them, push a group, then deactivate the user with PATCH.
func TestSCIMOktaFlow(t *testing.T) {
	e := scimEnv(t)
	list := e.scim("GET", `/scim/v2/Users?filter=userName+eq+%22ada%40example.com%22&startIndex=1&count=100`, "", 200)
	if list["totalResults"].(float64) != 0 {
		t.Fatalf("empty directory: %v", list)
	}
	u := e.scim("POST", "/scim/v2/Users", `{
		"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"],
		"userName": "Ada@example.com",
		"name": {"givenName": "Ada", "familyName": "Lovelace"},
		"emails": [{"primary": true, "value": "ada@example.com", "type": "work"}],
		"externalId": "00u1",
		"active": true
	}`, 201)
	uid := u["id"].(string)
	if u["displayName"] != "Ada Lovelace" || u["meta"].(map[string]any)["location"] != "http://example.com/scim/v2/Users/"+uid {
		t.Fatalf("created user: %v", u)
	}
	e.scim("POST", "/scim/v2/Users", `{"userName": "ada@EXAMPLE.com"}`, 409)
	list = e.scim("GET", `/scim/v2/Users?filter=userName%20eq%20%22ada@example.com%22`, "", 200)
	if list["totalResults"].(float64) != 1 {
		t.Fatalf("lookup without case: %v", list)
	}

	// Before provisioning a group, the signed-in user's claims have no
	// mapped group: the directory decides for provisioned users.
	cookie := e.ssoSession("ada@example.com", "gateway-owners")
	if rec := e.browser(cookie, "GET", "/admin/v1/orgs", ""); rec.Code != 401 {
		t.Fatalf("a provisioned user without groups = %d, want 401", rec.Code)
	}
	g := e.scim("POST", "/scim/v2/Groups", `{"schemas": ["urn:ietf:params:scim:schemas:core:2.0:Group"], "displayName": "gateway-owners", "members": []}`, 201)
	gid := g["id"].(string)
	e.scim("PATCH", "/scim/v2/Groups/"+gid, `{"schemas": ["`+scimPatchSchema+`"], "Operations": [{"op": "add", "path": "members", "value": [{"value": "`+uid+`", "display": "ada@example.com"}]}]}`, 200)
	if rec := e.browser(cookie, "GET", "/admin/v1/orgs", ""); rec.Code != 200 {
		t.Fatalf("a member of a mapped group = %d, want 200: %s", rec.Code, rec.Body)
	}
	got := e.scim("GET", "/scim/v2/Users/"+uid, "", 200)
	if groups := got["groups"].([]any); len(groups) != 1 || groups[0].(map[string]any)["display"] != "gateway-owners" {
		t.Fatalf("user groups: %v", got)
	}

	e.scim("PATCH", "/scim/v2/Users/"+uid, `{"schemas": ["`+scimPatchSchema+`"], "Operations": [{"op": "replace", "value": {"active": false}}]}`, 200)
	if rec := e.browser(cookie, "GET", "/admin/v1/orgs", ""); rec.Code != 401 {
		t.Fatalf("a deactivated user = %d, want 401", rec.Code)
	}
	again := e.ssoSession("ada@example.com", "gateway-owners")
	if rec := e.browser(again, "GET", "/admin/v1/orgs", ""); rec.Code != 401 {
		t.Fatalf("a deactivated user's new session = %d, want 401", rec.Code)
	}

	var audit struct {
		Data []struct{ Actor, Action string }
	}
	e.call("GET", "/admin/v1/audit", "", 200, &audit)
	if audit.Data[0].Actor != scimActor || audit.Data[0].Action != "scim.user.update" {
		t.Errorf("provisioning is audited: %+v", audit.Data[0])
	}
}

// TestSCIMEntraFlow follows what Entra ID sends: capitalized ops, string
// booleans, and member removal by path filter.
func TestSCIMEntraFlow(t *testing.T) {
	e := scimEnv(t)
	u := e.scim("POST", "/scim/v2/Users", `{"userName": "grace@example.com", "active": "True", "displayName": "Grace Hopper", "externalId": "e1"}`, 201)
	uid := u["id"].(string)
	g := e.scim("POST", "/scim/v2/Groups", `{"displayName": "gateway-owners", "members": [{"value": "`+uid+`"}]}`, 201)
	gid := g["id"].(string)
	if len(g["members"].([]any)) != 1 {
		t.Fatalf("group created with members: %v", g)
	}
	e.scim("POST", "/scim/v2/Groups", `{"displayName": "other", "members": [{"value": "usr_missing"}]}`, 400)

	cookie := e.ssoSession("grace@example.com")
	if rec := e.browser(cookie, "GET", "/admin/v1/orgs", ""); rec.Code != 200 {
		t.Fatalf("a provisioned owner with no groups claim = %d: %s", rec.Code, rec.Body)
	}
	e.scim("PATCH", "/scim/v2/Users/"+uid, `{"Operations": [{"op": "Replace", "path": "displayName", "value": "Rear Admiral Hopper"}, {"op": "Replace", "path": "emails[type eq \"work\"].value", "value": "Grace@Example.com"}]}`, 200)
	got := e.scim("GET", "/scim/v2/Users?filter=emails.value+eq+%22grace%40example.com%22", "", 200)
	if got["totalResults"].(float64) != 1 || got["Resources"].([]any)[0].(map[string]any)["displayName"] != "Rear Admiral Hopper" {
		t.Fatalf("patched user: %v", got)
	}

	e.scim("PATCH", "/scim/v2/Groups/"+gid, `{"Operations": [{"op": "Replace", "path": "displayName", "value": "Gateway Owners"}]}`, 200)
	e.scim("PATCH", "/scim/v2/Groups/"+gid, `{"Operations": [{"op": "Remove", "path": "members[value eq \"`+uid+`\"]"}]}`, 200)
	if got := e.scim("GET", "/scim/v2/Groups/"+gid, "", 200); got["displayName"] != "Gateway Owners" || len(got["members"].([]any)) != 0 {
		t.Fatalf("patched group: %v", got)
	}
	if rec := e.browser(cookie, "GET", "/admin/v1/orgs", ""); rec.Code != 401 {
		t.Fatalf("removed from the mapped group = %d, want 401", rec.Code)
	}
	list := e.scim("GET", `/scim/v2/Groups?filter=displayName+eq+%22gateway+owners%22&excludedAttributes=members`, "", 200)
	if list["totalResults"].(float64) != 1 {
		t.Fatalf("group lookup: %v", list)
	}
	e.scim("GET", `/scim/v2/Groups?filter=members+eq+%22x%22`, "", 400)

	var dir struct {
		Enabled bool
		Users   []struct {
			UserName string `json:"user_name"`
		}
	}
	e.call("GET", "/admin/v1/directory", "", 200, &dir)
	if !dir.Enabled || len(dir.Users) != 1 {
		t.Errorf("directory: %+v", dir)
	}

	e.scim("PUT", "/scim/v2/Groups/"+gid, `{"displayName": "gateway-owners", "members": [{"value": "`+uid+`"}]}`, 200)
	if rec := e.browser(cookie, "GET", "/admin/v1/orgs", ""); rec.Code != 200 {
		t.Fatalf("put back in the group = %d", rec.Code)
	}
	e.scim("DELETE", "/scim/v2/Users/"+uid, "", 204)
	if rec := e.browser(cookie, "GET", "/admin/v1/orgs", ""); rec.Code != 401 {
		t.Fatalf("a deleted user's session = %d, want 401", rec.Code)
	}
	e.scim("GET", "/scim/v2/Users/"+uid, "", 404)
	e.scim("DELETE", "/scim/v2/Groups/"+gid, "", 204)
}
