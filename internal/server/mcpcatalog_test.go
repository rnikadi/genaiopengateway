package server

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

func TestMCPCatalogRequests(t *testing.T) {
	e := newAdminEnv(t, `
mcp:
  servers: {up: {url: "http://127.0.0.1:1/mcp"}}
  virtual_servers:
    docs: {description: Documentation search, servers: [{server: up}]}
    github: {servers: [{server: up}], guardrails: []}
    payroll: {servers: [{server: up}]}
`)
	var org, search, open idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","allowed_mcp_servers":["docs","github"]}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"search","allowed_mcp_servers":["docs"]}`, 201, &search)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"open"}`, 201, &open)

	lead := e.session("lead@example.com", store.Grant{Role: config.RoleAdmin, OrgID: org.ID, TeamID: search.ID})
	admin := e.session("admin@example.com", store.Grant{Role: config.RoleAdmin, OrgID: org.ID})

	type entry struct {
		Name, Access, Endpoint string
		RequestID              string `json:"request_id"`
	}
	catalog := func(cookie, teamID string) map[string]entry {
		t.Helper()
		var out struct{ Data []entry }
		rec := e.browser(cookie, "GET", "/admin/v1/mcp/catalog?team_id="+teamID, "")
		if rec.Code != 200 {
			t.Fatalf("catalog = %d %s", rec.Code, rec.Body)
		}
		decodeJSON(t, rec.Body.Bytes(), &out)
		m := map[string]entry{}
		for _, x := range out.Data {
			m[x.Name] = x
		}
		return m
	}
	c := catalog(lead, search.ID)
	if c["docs"].Access != "allowed" || c["github"].Access != "available" || c["payroll"].Access != "blocked_by_org" || c["docs"].Endpoint != "/mcp/docs" {
		t.Fatalf("catalog = %+v", c)
	}
	if c := catalog(admin, open.ID); c["github"].Access != "allowed" || c["payroll"].Access != "blocked_by_org" {
		t.Fatalf("open team catalog = %+v", c)
	}
	if rec := e.browser(lead, "GET", "/admin/v1/mcp/catalog?team_id="+open.ID, ""); rec.Code != 403 {
		t.Fatalf("other team's catalog = %d", rec.Code)
	}

	// The team lead asks; asking twice, or for what the team has, is refused.
	var req, payroll struct {
		ID, Status string
	}
	if rec := e.browser(lead, "POST", "/admin/v1/mcp/requests", `{"team_id":"`+search.ID+`","server":"github","reason":"triage issues"}`); rec.Code != 201 {
		t.Fatalf("request = %d %s", rec.Code, rec.Body)
	} else {
		decodeJSON(t, rec.Body.Bytes(), &req)
	}
	for body, want := range map[string]int{
		`{"team_id":"` + search.ID + `","server":"github"}`: 409,
		`{"team_id":"` + search.ID + `","server":"docs"}`:   409,
		`{"team_id":"` + search.ID + `","server":"nope"}`:   404,
		`{"team_id":"` + open.ID + `","server":"payroll"}`:  403,
	} {
		if rec := e.browser(lead, "POST", "/admin/v1/mcp/requests", body); rec.Code != want {
			t.Errorf("%s = %d, want %d: %s", body, rec.Code, want, rec.Body)
		}
	}
	if rec := e.browser(lead, "POST", "/admin/v1/mcp/requests", `{"team_id":"`+search.ID+`","server":"payroll"}`); rec.Code != 201 {
		t.Fatalf("payroll request = %d %s", rec.Code, rec.Body)
	} else {
		decodeJSON(t, rec.Body.Bytes(), &payroll)
	}
	if c := catalog(lead, search.ID); c["github"].Access != "pending" || c["github"].RequestID != req.ID {
		t.Fatalf("pending = %+v", c["github"])
	}

	// The lead sees the requests but cannot decide; an org admin can.
	var list struct {
		Data []struct {
			ID        string
			TeamName  string `json:"team_name"`
			CanDecide bool   `json:"can_decide"`
		}
	}
	decodeJSON(t, e.browser(lead, "GET", "/admin/v1/mcp/requests?status=pending", "").Body.Bytes(), &list)
	if len(list.Data) != 2 || list.Data[0].TeamName != "search" || list.Data[0].CanDecide {
		t.Fatalf("lead's list = %+v", list.Data)
	}
	if rec := e.browser(lead, "POST", "/admin/v1/mcp/requests/"+req.ID+"/approve", ""); rec.Code != 403 {
		t.Fatalf("lead approves = %d", rec.Code)
	}
	decodeJSON(t, e.browser(admin, "GET", "/admin/v1/mcp/requests", "").Body.Bytes(), &list)
	if len(list.Data) != 2 || !list.Data[0].CanDecide {
		t.Fatalf("admin's list = %+v", list.Data)
	}
	if rec := e.browser(admin, "POST", "/admin/v1/mcp/requests/"+req.ID+"/approve", `{"note":"ok"}`); rec.Code != 200 {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	var team store.Team
	e.call("GET", "/admin/v1/teams/"+search.ID, "", 200, &team)
	if !slices.Equal(team.Policy.AllowedMCPServers, []string{"docs", "github"}) {
		t.Fatalf("team servers = %v", team.Policy.AllowedMCPServers)
	}
	if c := catalog(lead, search.ID); c["github"].Access != "allowed" {
		t.Fatalf("after approval = %+v", c["github"])
	}
	if rec := e.browser(admin, "POST", "/admin/v1/mcp/requests/"+req.ID+"/deny", ""); rec.Code != 409 {
		t.Fatalf("deciding twice = %d", rec.Code)
	}

	// The organization leaves payroll out: approval waits for an owner.
	if rec := e.browser(admin, "POST", "/admin/v1/mcp/requests/"+payroll.ID+"/approve", ""); rec.Code != 409 {
		t.Fatalf("approve blocked by org = %d %s", rec.Code, rec.Body)
	}
	if rec := e.browser(admin, "POST", "/admin/v1/mcp/requests/"+payroll.ID+"/deny", `{"note":"not for search"}`); rec.Code != 200 {
		t.Fatalf("deny = %d %s", rec.Code, rec.Body)
	}
	var denied struct{ Data []store.MCPAccessRequest }
	e.call("GET", "/admin/v1/mcp/requests?org_id="+org.ID+"&status=denied", "", 200, &denied)
	if len(denied.Data) != 1 {
		t.Fatalf("denied = %+v", denied.Data)
	}
	got := denied.Data[0]
	if got.Status != "denied" || got.Note != "not for search" || got.DecidedBy != "admin@example.com" {
		t.Fatalf("denied = %+v", got)
	}

	var audit struct{ Data []store.AuditEntry }
	e.call("GET", "/admin/v1/audit?org_id="+org.ID, "", 200, &audit)
	actions := map[string]int{}
	for _, a := range audit.Data {
		actions[a.Action]++
	}
	if actions["mcp_request.create"] != 2 || actions["mcp_request.approve"] != 1 || actions["mcp_request.deny"] != 1 {
		t.Fatalf("audit = %v", actions)
	}
}

func decodeJSON(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}
