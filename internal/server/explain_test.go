package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

type explainView struct {
	Allowed bool
	Summary string
	Denial  *store.Denial
	Checks  []struct{ ID, Status, Detail, Fix, Link string }
}

func (v explainView) check(id string) (status, detail string) {
	for _, c := range v.Checks {
		if c.ID == id {
			return c.Status, c.Detail
		}
	}
	return "", ""
}

func chatWithID(e *adminEnv, key, model, id string) int {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Request-Id", id)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code
}

func TestWhyDenied(t *testing.T) {
	e := newAdminEnv(t, `
mcp:
  servers:
    up: {url: "http://127.0.0.1:9/mcp", pin_tools: strict}
  virtual_servers:
    docs: {servers: [{server: up}]}
`)
	var org, team, key, other idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","allowed_models":["chat","embed"]}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"search","allowed_models":["chat"],"rpm":1,"allowed_mcp_servers":["docs"]}`, 201, &team)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"ci","owner":"ada@example.com"}`, 201, &key)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"other","owner":"bob@example.com"}`, 201, &other)

	explain := func(cookie, body string, want int) explainView {
		t.Helper()
		var rec *httptest.ResponseRecorder
		if cookie == "" {
			rec = do(e.h, "POST", "/admin/v1/explain", adminToken, body)
		} else {
			rec = e.browser(cookie, "POST", "/admin/v1/explain", body)
		}
		if rec.Code != want {
			t.Fatalf("explain %s = %d %s", body, rec.Code, rec.Body)
		}
		var v explainView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return v
	}

	// Before any request: chat is allowed, embed is not, and why.
	if v := explain("", `{"key_id":"`+key.ID+`","model":"chat"}`, 200); !v.Allowed {
		t.Fatalf("chat = %+v", v)
	}
	v := explain("", `{"key_id":"`+key.ID+`","model":"embed"}`, 200)
	if st, detail := v.check("allowed_models"); v.Allowed || st != checkFail || !strings.Contains(detail, "team's allowed models (chat)") {
		t.Fatalf("embed = %+v", v)
	}
	if v := explain("", `{"key_id":"`+key.ID+`","model":"nope"}`, 200); v.Allowed {
		t.Fatalf("an unknown model is allowed: %+v", v)
	}

	// Refusals are recorded, and explained by request id.
	if code := chatWithID(e, key.Key, "embed", "req-embed"); code != 403 {
		t.Fatalf("embed = %d", code)
	}
	if code := chatWithID(e, key.Key, "chat", "req-ok"); code != 200 {
		t.Fatalf("chat = %d", code)
	}
	if code := chatWithID(e, key.Key, "chat", "req-rpm"); code != 429 {
		t.Fatalf("second chat in the minute = %d", code)
	}
	v = explain("", `{"key_id":"`+key.ID+`","model":"chat"}`, 200)
	if st, detail := v.check("rate_limits"); v.Allowed || st != checkFail || !strings.Contains(detail, "team's limit of 1 requests a minute") {
		t.Fatalf("after the limit = %+v", v)
	}
	e.srv.denials.Close(context.Background())
	var list struct{ Data []store.Denial }
	e.call("GET", "/admin/v1/denials?org_id="+org.ID, "", 200, &list)
	if len(list.Data) != 2 || list.Data[0].RequestID != "req-rpm" || list.Data[0].Code != "rate_limit_exceeded" || list.Data[1].Code != codeModelNotAllowed {
		t.Fatalf("denials = %+v", list.Data)
	}
	v = explain("", `{"request_id":"req-embed"}`, 200)
	if v.Denial == nil || v.Denial.Reason != "this key may not use model embed" || v.Allowed {
		t.Fatalf("by request = %+v", v)
	}
	explain("", `{"request_id":"req-ok"}`, 404)

	// MCP: the server is allowed, its tools need approval in strict mode.
	v = explain("", `{"key_id":"`+key.ID+`","server":"docs","tool":"up_search"}`, 200)
	if st, _ := v.check("allowed_mcp_servers"); st != checkPass {
		t.Fatalf("MCP server = %+v", v)
	}
	if st, _ := v.check("tool"); st != checkWarn {
		t.Fatalf("an unseen tool = %+v", v)
	}
	if v := explain("", `{"key_id":"`+key.ID+`","server":"docs","tool":"other_x"}`, 200); v.Allowed {
		t.Fatalf("a tool the virtual server does not expose = %+v", v)
	}

	// Who may ask: readers of the key, and its owner through the portal.
	dev := e.session("ada@example.com", store.Grant{Role: config.RoleDeveloper, OrgID: org.ID, TeamID: team.ID})
	explain(dev, `{"key_id":"`+key.ID+`","model":"chat"}`, 200)
	explain(dev, `{"key_id":"`+other.ID+`","model":"chat"}`, 404)
	viewer := e.session("vic@example.com", store.Grant{Role: config.RoleViewer, OrgID: org.ID})
	explain(viewer, `{"key_id":"`+other.ID+`","model":"chat"}`, 200)
	outsider := e.session("eve@example.com", store.Grant{Role: config.RoleViewer, OrgID: "org_other"})
	explain(outsider, `{"key_id":"`+key.ID+`","model":"chat"}`, 404)
	if rec := e.browser(outsider, "GET", "/admin/v1/denials?org_id="+org.ID, ""); rec.Code != 403 {
		t.Errorf("denials of another organization = %d", rec.Code)
	}
	explain("", `{"key_id":"`+key.ID+`"}`, 400)
}
