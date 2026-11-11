package server

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

func TestCustomAndTeamRoles(t *testing.T) {
	e := newAdminEnv(t, `
sso:
  custom_roles:
    billing: {description: Finance, permissions: [org.read, usage.read]}
    keymaster: {permissions: [teams.read, keys.read, keys.write, playground.use]}
`)
	var acme, globex, search, ads, other, searchKey, adsKey idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &acme)
	e.call("POST", "/admin/v1/orgs", `{"name":"globex"}`, 201, &globex)
	e.call("POST", "/admin/v1/orgs/"+acme.ID+"/teams", `{"name":"search"}`, 201, &search)
	e.call("POST", "/admin/v1/orgs/"+acme.ID+"/teams", `{"name":"ads"}`, 201, &ads)
	e.call("POST", "/admin/v1/orgs/"+globex.ID+"/teams", `{"name":"other"}`, 201, &other)
	e.call("POST", "/admin/v1/teams/"+search.ID+"/keys", `{"name":"s"}`, 201, &searchKey)
	e.call("POST", "/admin/v1/teams/"+ads.ID+"/keys", `{"name":"a"}`, 201, &adsKey)

	billing := e.session("finance@example.com", store.Grant{Role: "billing", OrgID: acme.ID})
	teamAdmin := e.session("lead@example.com", store.Grant{Role: config.RoleAdmin, OrgID: acme.ID, TeamID: search.ID})
	keymaster := e.session("ops@example.com", store.Grant{Role: "keymaster", OrgID: acme.ID, TeamID: ads.ID})

	for _, tc := range []struct {
		who          string
		cookie       string
		method, path string
		body         string
		want         int
	}{
		{"billing", billing, "GET", "/admin/v1/orgs/" + acme.ID, "", 200},
		{"billing", billing, "GET", "/admin/v1/usage?org_id=" + acme.ID, "", 200},
		{"billing", billing, "GET", "/admin/v1/usage?team_id=" + search.ID, "", 200},
		{"billing", billing, "GET", "/admin/v1/usage?org_id=" + globex.ID, "", 403},
		{"billing", billing, "GET", "/admin/v1/teams/" + search.ID + "/keys", "", 403},
		{"billing", billing, "GET", "/admin/v1/audit?org_id=" + acme.ID, "", 403},
		{"billing", billing, "POST", "/admin/v1/orgs/" + acme.ID + "/teams", `{"name":"x"}`, 403},

		{"team admin", teamAdmin, "GET", "/admin/v1/orgs/" + acme.ID, "", 200},
		{"team admin", teamAdmin, "GET", "/admin/v1/teams/" + search.ID, "", 200},
		{"team admin", teamAdmin, "GET", "/admin/v1/teams/" + ads.ID, "", 403},
		{"team admin", teamAdmin, "POST", "/admin/v1/teams/" + search.ID + "/keys", `{"name":"new"}`, 201},
		{"team admin", teamAdmin, "POST", "/admin/v1/teams/" + ads.ID + "/keys", `{"name":"new"}`, 403},
		{"team admin", teamAdmin, "POST", "/admin/v1/keys/" + searchKey.ID + "/rotate", "", 200},
		{"team admin", teamAdmin, "POST", "/admin/v1/keys/" + adsKey.ID + "/revoke", "", 403},
		{"team admin", teamAdmin, "PATCH", "/admin/v1/teams/" + search.ID, `{"rpm":1000}`, 403},
		{"team admin", teamAdmin, "DELETE", "/admin/v1/teams/" + search.ID, "", 403},
		{"team admin", teamAdmin, "POST", "/admin/v1/orgs/" + acme.ID + "/teams", `{"name":"x"}`, 403},
		{"team admin", teamAdmin, "GET", "/admin/v1/usage?team_id=" + search.ID, "", 200},
		{"team admin", teamAdmin, "GET", "/admin/v1/usage?org_id=" + acme.ID, "", 403},
		{"team admin", teamAdmin, "GET", "/admin/v1/usage?key_id=" + adsKey.ID, "", 403},
		{"team admin", teamAdmin, "GET", "/admin/v1/orgs/" + globex.ID, "", 403},
		{"team admin", teamAdmin, "DELETE", "/admin/v1/cache?org_id=" + acme.ID + "&team_id=" + search.ID, "", 200},
		{"team admin", teamAdmin, "DELETE", "/admin/v1/cache?org_id=" + acme.ID, "", 403},

		{"keymaster", keymaster, "POST", "/admin/v1/teams/" + ads.ID + "/keys", `{"name":"k"}`, 201},
		{"keymaster", keymaster, "GET", "/admin/v1/usage?team_id=" + ads.ID, "", 403},
		{"keymaster", keymaster, "PUT", "/admin/v1/teams/" + ads.ID + "/credentials/up", `{"api_key":"sk-x"}`, 403},
	} {
		if rec := e.browser(tc.cookie, tc.method, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s: %s %s = %d, want %d: %s", tc.who, tc.method, tc.path, rec.Code, tc.want, rec.Body)
		}
	}

	ids := func(cookie, path string) []string {
		var out struct{ Data []struct{ ID string } }
		rec := e.browser(cookie, "GET", path, "")
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		var got []string
		for _, d := range out.Data {
			got = append(got, d.ID)
		}
		return got
	}
	if got := ids(teamAdmin, "/admin/v1/orgs"); !slices.Equal(got, []string{acme.ID}) {
		t.Errorf("a team admin lists the team's organization: %v", got)
	}
	if got := ids(teamAdmin, "/admin/v1/orgs/"+acme.ID+"/teams"); !slices.Equal(got, []string{search.ID}) {
		t.Errorf("a team admin lists only their team: %v", got)
	}
	keys := ids(teamAdmin, "/admin/v1/keys")
	if len(keys) != 2 || slices.Contains(keys, adsKey.ID) {
		t.Errorf("a team admin lists only their team's keys: %v", keys)
	}
	if got := ids(billing, "/admin/v1/keys"); len(got) != 0 {
		t.Errorf("a role without keys.read lists no keys: %v", got)
	}

	var me struct {
		Operator struct {
			Grants []struct {
				TeamName    string `json:"team_name"`
				Permissions []string
			}
		}
	}
	rec := e.browser(teamAdmin, "GET", "/admin/v1/me", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if g := me.Operator.Grants; len(g) != 1 || g[0].TeamName != "search" || !slices.Equal(g[0].Permissions, config.Permissions) {
		t.Errorf("me describes the team grant: %s", rec.Body)
	}
}

func TestTeamRoleMappings(t *testing.T) {
	e := newAdminEnv(t, "")
	var acme, search idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &acme)
	e.call("POST", "/admin/v1/orgs/"+acme.ID+"/teams", `{"name":"search"}`, 201, &search)
	if err := e.srv.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := e.srv.resolveGrants([]config.RoleMapping{
		{Group: "search-leads", Role: config.RoleAdmin, Org: "acme", Team: "search"},
		{Group: "search-leads", Role: config.RoleViewer, Org: acme.ID, Team: search.ID},
		{Group: "search-leads", Role: config.RoleAdmin, Org: "acme", Team: "missing"},
	}, "", []string{"search-leads"})
	want := []store.Grant{
		{Role: config.RoleAdmin, OrgID: acme.ID, TeamID: search.ID},
		{Role: config.RoleViewer, OrgID: acme.ID, TeamID: search.ID},
	}
	if !slices.Equal(got, want) {
		t.Errorf("grants %+v, want %+v", got, want)
	}
}

func TestCustomRolesInSettings(t *testing.T) {
	e := newAdminEnv(t, "")
	var acme idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &acme)
	e.call("PUT", "/admin/v1/config/settings/gateway", `{"sso": {"custom_roles": {"billing": {"permissions": ["org.read", "usage.read"]}}}}`, 201, nil)
	e.call("PUT", "/admin/v1/config/settings/gateway", `{"sso": {"custom_roles": {"billing": {"permissions": ["usage.write"]}}}}`, 400, nil)
	if err := e.srv.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	billing := e.session("finance@example.com", store.Grant{Role: "billing", OrgID: acme.ID})
	if rec := e.browser(billing, "GET", "/admin/v1/usage?org_id="+acme.ID, ""); rec.Code != 200 {
		t.Fatalf("a custom role saved in Settings applies: %d %s", rec.Code, rec.Body)
	}
}
