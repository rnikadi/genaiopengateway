package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

func TestCedarPolicies(t *testing.T) {
	e := newAdminEnv(t, "")
	var org, other, interns, eng, internKey, engKey idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs", `{"name":"globex"}`, 201, &other)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"interns"}`, 201, &interns)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"eng"}`, 201, &eng)
	e.call("POST", "/admin/v1/teams/"+interns.ID+"/keys", `{"name":"i"}`, 201, &internKey)
	e.call("POST", "/admin/v1/teams/"+eng.ID+"/keys", `{"name":"e"}`, 201, &engKey)
	cedarPath := "/admin/v1/orgs/" + org.ID + "/cedar"

	// Without policies, everything the key policies allow is allowed.
	var view struct {
		Version  int64  `json:"version"`
		Text     string `json:"text"`
		Policies int    `json:"policies"`
	}
	e.call("GET", cedarPath, "", 200, &view)
	if view.Version != 0 || view.Text != "" {
		t.Fatalf("empty view = %+v", view)
	}
	if rec := do(e.h, "POST", "/v1/chat/completions", internKey.Key, `{"model":"chat"}`); rec.Code != 200 {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body)
	}

	// Bad policies are refused; good ones apply at once.
	e.call("PUT", cedarPath, `{"text":"permit (principal, action, resource)"}`, 400, nil)
	e.call("PUT", cedarPath, `{"text":"  "}`, 400, nil)
	policies := `@id("base")
permit (principal, action, resource);
@id("interns-no-chat")
forbid (principal in Team::"` + interns.ID + `", action == Action::"chat.completions", resource == Model::"chat");
@id("no-streaming-for-bots")
forbid (principal, action in Action::"model", resource) when { context.stream && principal.name == "e" };
@id("no-deletes")
forbid (principal, action == Action::"call_tool", resource in MCPServer::"github") when { resource.name like "delete_*" };`
	raw, _ := json.Marshal(map[string]string{"text": policies})
	e.call("PUT", cedarPath, string(raw), 200, &view)
	if view.Version != 1 || view.Policies != 4 {
		t.Fatalf("saved = %+v", view)
	}

	rec := do(e.h, "POST", "/v1/chat/completions", internKey.Key, `{"model":"chat"}`)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "policy_denied") || !strings.Contains(rec.Body.String(), "interns-no-chat") {
		t.Fatalf("intern chat = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "POST", "/v1/embeddings", internKey.Key, `{"model":"embed"}`); rec.Code != 200 {
		t.Fatalf("intern embeddings = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "POST", "/v1/chat/completions", engKey.Key, `{"model":"chat"}`); rec.Code != 200 {
		t.Fatalf("eng chat = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "POST", "/v1/chat/completions", engKey.Key, `{"model":"chat","stream":true}`); rec.Code != 403 {
		t.Fatalf("eng stream = %d %s", rec.Code, rec.Body)
	}

	// Tool calls are decided too.
	p := &auth.Principal{OrgID: org.ID, TeamID: eng.ID, KeyID: engKey.ID, KeyName: "e"}
	if d := e.srv.authorizeTool(context.Background(), &mcpgw.Caller{Principal: p}, "github", "delete_repo"); !strings.Contains(d, "no-deletes") {
		t.Fatalf("delete_repo = %q", d)
	}
	if d := e.srv.authorizeTool(context.Background(), &mcpgw.Caller{Principal: p}, "github", "create_issue"); d != "" {
		t.Fatalf("create_issue = %q", d)
	}
	// Another organization has no policies.
	if d := e.srv.authorizeTool(context.Background(), &mcpgw.Caller{Principal: &auth.Principal{OrgID: other.ID}}, "github", "delete_repo"); d != "" {
		t.Fatalf("other org = %q", d)
	}

	// The check endpoint answers for a key, or for a draft.
	var check struct {
		Allowed  bool     `json:"allowed"`
		Policies []string `json:"policies"`
		Reason   string   `json:"reason"`
	}
	e.call("POST", cedarPath+"/check", `{"key_id":"`+internKey.ID+`","model":"chat"}`, 200, &check)
	if check.Allowed || len(check.Policies) != 1 || check.Policies[0] != "interns-no-chat" {
		t.Fatalf("check = %+v", check)
	}
	e.call("POST", cedarPath+"/check", `{"text":"forbid (principal, action, resource);","team_id":"eng","server":"github","tool":"x"}`, 200, &check)
	if check.Allowed || check.Reason == "" {
		t.Fatalf("draft check = %+v", check)
	}
	e.call("POST", cedarPath+"/check", `{"model":"chat","tool":"x"}`, 400, nil)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/cedar/check", `{"key_id":"`+internKey.ID+`","model":"chat","text":"permit (principal, action, resource);"}`, 200, &check)
	if !check.Allowed {
		t.Fatalf("permissive draft = %+v", check)
	}
	e.call("POST", "/admin/v1/orgs/"+other.ID+"/cedar/check", `{"key_id":"`+internKey.ID+`","model":"chat"}`, 404, nil)

	// Only owners write; readers of the organization read.
	viewer := e.session("v@example.com", store.Grant{Role: config.RoleViewer, OrgID: org.ID})
	admin := e.session("a@example.com", store.Grant{Role: config.RoleAdmin, OrgID: org.ID})
	for _, tc := range []struct {
		cookie, method, path, body string
		want                       int
	}{
		{viewer, "GET", cedarPath, "", 200},
		{viewer, "GET", cedarPath + "/versions", "", 200},
		{viewer, "POST", cedarPath + "/check", `{"model":"chat"}`, 200},
		{viewer, "GET", "/admin/v1/orgs/" + other.ID + "/cedar", "", 403},
		{admin, "PUT", cedarPath, string(raw), 403},
		{admin, "DELETE", cedarPath, "", 403},
	} {
		if rec := e.browser(tc.cookie, tc.method, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d: %s", tc.method, tc.path, rec.Code, tc.want, rec.Body)
		}
	}

	// Removing them is a new version; the history and audit log keep both.
	e.call("DELETE", cedarPath, "", 200, &view)
	if view.Version != 2 {
		t.Fatalf("deleted = %+v", view)
	}
	if rec := do(e.h, "POST", "/v1/chat/completions", internKey.Key, `{"model":"chat"}`); rec.Code != 200 {
		t.Fatalf("chat after delete = %d %s", rec.Code, rec.Body)
	}
	var versions struct {
		Data []store.CedarPolicies `json:"data"`
	}
	e.call("GET", cedarPath+"/versions", "", 200, &versions)
	if len(versions.Data) != 2 || versions.Data[0].Version != 2 || versions.Data[1].CreatedBy == "" {
		t.Fatalf("versions = %+v", versions.Data)
	}
	e.call("GET", cedarPath+"?version=1", "", 200, &view)
	if view.Text != policies || view.Policies != 4 {
		t.Fatalf("version 1 = %+v", view)
	}
	e.call("GET", cedarPath+"?version=9", "", 404, nil)
	var audit struct {
		Data []store.AuditEntry `json:"data"`
	}
	e.call("GET", "/admin/v1/audit?org_id="+org.ID, "", 200, &audit)
	actions := map[string]bool{}
	for _, a := range audit.Data {
		actions[a.Action] = true
	}
	if !actions["cedar.update"] || !actions["cedar.delete"] {
		t.Fatalf("audit = %v", actions)
	}
}

func TestCedarStoredDocumentThatNoLongerParses(t *testing.T) {
	e := newAdminEnv(t, "")
	var org, team, key idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"t"}`, 201, &team)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"k"}`, 201, &key)
	if _, err := e.srv.store.SaveCedarPolicies(context.Background(), org.ID, "not cedar", "test"); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := do(e.h, "POST", "/v1/chat/completions", key.Key, `{"model":"chat"}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "cannot be read") {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body)
	}
}
