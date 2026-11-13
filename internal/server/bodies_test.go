package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

const testEncryptionKey = "XU4XYQMExW1ZlJ4E3eXfwgVLht06HQRhJRewoweY2YI="

const bodyLoggingConfig = `
secrets: {encryption_key: "` + testEncryptionKey + `"}
body_logging: {enabled: true, max_body_bytes: 1024}
`

// chat sends a chat request with a chosen request id.
func (e *adminEnv) chat(key, requestID, body string) {
	e.t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Request-Id", requestID)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		e.t.Fatalf("chat = %d: %s", rec.Code, rec.Body)
	}
}

// flush writes queued usage and bodies, as shutdown does.
func (e *adminEnv) flush() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.srv.Close(ctx)
}

func TestBodyLogging(t *testing.T) {
	e := newAdminEnv(t, bodyLoggingConfig)
	var org, other, team, logged, optedOut, plain idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs", `{"name":"globex"}`, 201, &other)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"support","log_bodies":true}`, 201, &team)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"bot"}`, 201, &logged)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"private","log_bodies":false}`, 201, &optedOut)
	var plainTeam idResp
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"search"}`, 201, &plainTeam)
	e.call("POST", "/admin/v1/teams/"+plainTeam.ID+"/keys", `{"name":"ci"}`, 201, &plain)

	secret := `{"model":"chat","messages":[{"role":"user","content":"I am jane@example.com, card 4111 1111 1111 1111"}],"api_key":"sk-hidden"}`
	e.chat(logged.Key, "req-logged", secret)
	e.chat(optedOut.Key, "req-opted-out", secret)
	e.chat(plain.Key, "req-plain", secret)
	e.flush()

	// Stored encrypted: the raw columns hold no plaintext.
	rows, err := e.srv.store.RequestBodies(context.Background(), "req-logged")
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored bodies = %v, %v", rows, err)
	}
	if strings.Contains(rows[0].Request, "messages") || strings.Contains(rows[0].Response, "chat") {
		t.Fatal("a body is stored in plaintext")
	}
	for _, id := range []string{"req-opted-out", "req-plain"} {
		if rows, _ := e.srv.store.RequestBodies(context.Background(), id); len(rows) != 0 {
			t.Errorf("%s: body logged without a policy asking for it", id)
		}
	}

	var got struct {
		Kind       string         `json:"kind"`
		Request    map[string]any `json:"request"`
		Response   map[string]any `json:"response"`
		Redactions map[string]int `json:"redactions"`
	}
	e.call("GET", "/admin/v1/requests/req-logged", "", 200, &got)
	content := got.Request["messages"].([]any)[0].(map[string]any)["content"]
	if content != "I am [REDACTED:email], card [REDACTED:credit_card]" || got.Request["api_key"] != "[REDACTED:secret]" {
		t.Fatalf("request = %v", got.Request)
	}
	if got.Kind != "model" || got.Response["path"] != "/chat/completions" {
		t.Fatalf("record = %+v", got)
	}
	if got.Redactions["email"] != 1 || got.Redactions["credit_card"] != 1 || got.Redactions["secret"] != 1 {
		t.Fatalf("redactions = %v", got.Redactions)
	}

	// Every read is audited.
	audit, _, _ := e.srv.store.ListAudit(context.Background(), org.ID, store.Page{})
	if len(audit) == 0 || audit[0].Action != "request_body.read" || audit[0].ResourceID != "req-logged" {
		t.Fatalf("no audit entry for the read: %+v", audit)
	}

	// The list joins usage to bodies.
	var list struct {
		Data []store.RequestRow `json:"data"`
	}
	e.call("GET", "/admin/v1/requests?org_id="+org.ID, "", 200, &list)
	if len(list.Data) != 3 {
		t.Fatalf("listed %d requests", len(list.Data))
	}
	for _, r := range list.Data {
		if r.HasBody != (r.RequestID == "req-logged") {
			t.Errorf("%s: has_body = %v", r.RequestID, r.HasBody)
		}
	}
	var page2 struct {
		Data    []store.RequestRow `json:"data"`
		HasMore bool               `json:"has_more"`
	}
	e.call("GET", "/admin/v1/requests?org_id="+org.ID+"&limit=2", "", 200, &page2)
	last := page2.Data[1].RequestID
	e.call("GET", "/admin/v1/requests?org_id="+org.ID+"&limit=2&after="+last, "", 200, &page2)
	if len(page2.Data) != 1 || page2.HasMore || page2.Data[0].RequestID == last {
		t.Fatalf("second page = %+v", page2)
	}

	// Roles: viewers see the list but not bodies; other organizations
	// do not learn the request exists.
	viewer := e.session("analyst@example.com", store.Grant{Role: config.RoleViewer, OrgID: org.ID})
	admin := e.session("lead@example.com", store.Grant{Role: config.RoleAdmin, OrgID: org.ID})
	outsider := e.session("rival@example.com", store.Grant{Role: config.RoleAdmin, OrgID: other.ID})
	for _, tc := range []struct {
		cookie, method, path string
		want                 int
	}{
		{viewer, "GET", "/admin/v1/requests?org_id=" + org.ID, 200},
		{viewer, "GET", "/admin/v1/requests/req-logged", 403},
		{admin, "GET", "/admin/v1/requests/req-logged", 200},
		{outsider, "GET", "/admin/v1/requests/req-logged", 404},
		{outsider, "GET", "/admin/v1/requests?org_id=" + org.ID, 403},
		{admin, "DELETE", "/admin/v1/requests?org_id=" + org.ID + "&before=2100-01-01T00:00:00Z", 403},
	} {
		if rec := e.browser(tc.cookie, tc.method, tc.path, ""); rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d: %s", tc.method, tc.path, rec.Code, tc.want, rec.Body)
		}
	}

	// Owners delete early, for erasure requests.
	var del struct {
		Deleted int `json:"deleted"`
	}
	e.call("DELETE", "/admin/v1/requests?org_id="+org.ID+"&before=2100-01-01T00:00:00Z", "", 200, &del)
	if del.Deleted != 1 {
		t.Fatalf("deleted %d", del.Deleted)
	}
	e.call("GET", "/admin/v1/requests/req-logged", "", 404, nil)
}

func TestBodyLoggingMasterSwitch(t *testing.T) {
	e := newAdminEnv(t, `secrets: {encryption_key: "`+testEncryptionKey+`"}`)
	var org, team, key idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","log_bodies":true}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"support"}`, 201, &team)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"bot"}`, 201, &key)
	e.chat(key.Key, "req-1", `{"model":"chat"}`)
	e.flush()
	if rows, _ := e.srv.store.RequestBodies(context.Background(), "req-1"); len(rows) != 0 {
		t.Fatal("a body was logged with body_logging off")
	}
}

func TestBodyLoggingTruncatesStreams(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{"Hello", " jane@example.com", strings.Repeat(" x", 3000)} {
			b, _ := json.Marshal(map[string]any{"object": "chat.completion.chunk", "model": "m",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": d}}}})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()
	e := newAdminEnv(t, bodyLoggingConfig+`
keys: [{name: static, key: sk-static-key-0000000000000000, log_bodies: true}]
`)
	prov := e.srv.rt.Load().cfg.Providers["up"]
	prov.BaseURL = up.URL
	e.srv.rt.Load().cfg.Providers["up"] = prov
	if err := e.srv.Reload(e.srv.rt.Load().cfg); err != nil {
		t.Fatal(err)
	}
	e.chat("sk-static-key-0000000000000000", "req-stream", `{"model":"chat","stream":true}`)
	e.flush()

	box, _ := secretBox(e.srv.rt.Load().cfg.Secrets)
	rows, _ := e.srv.store.RequestBodies(context.Background(), "req-stream")
	if len(rows) != 1 {
		t.Fatalf("stored %d bodies", len(rows))
	}
	resp, err := box.Open(rows[0].Response, bodyAD("req-stream", "response"))
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0].Truncated || len(resp) > 1024 {
		t.Fatalf("truncated %v, %d bytes", rows[0].Truncated, len(resp))
	}
	// The capture is cut inside the long chunk, so the stream assembles
	// from the first two events.
	if !strings.Contains(string(resp), `"content":"Hello [REDACTED:email]`) {
		t.Fatalf("response = %s", resp)
	}
}

func TestLogMCPBody(t *testing.T) {
	e := newAdminEnv(t, bodyLoggingConfig)
	on, off := true, false
	caller := func(v *bool) *mcpgw.Caller {
		return &mcpgw.Caller{RequestID: "mcp-1", Principal: &auth.Principal{OrgID: "org_1", KeyID: "key_1", Policies: [3]policy.Policy{{LogBodies: v}}}}
	}
	params := map[string]any{"name": "search", "arguments": map[string]any{"q": "mail bob@example.com"}}
	e.srv.logMCPBody(caller(&off), "tools", "tools/call", params, nil, nil)
	e.srv.logMCPBody(caller(&on), "tools", "tools/call", params, &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil)
	e.srv.logMCPBody(&mcpgw.Caller{RequestID: "mcp-2", Principal: caller(&on).Principal}, "tools", "tools/call", params, nil, errors.New("upstream down"))
	e.flush()

	box, _ := secretBox(e.srv.rt.Load().cfg.Secrets)
	rows, _ := e.srv.store.RequestBodies(context.Background(), "mcp-1")
	if len(rows) != 1 || rows[0].Kind != "mcp" || rows[0].Endpoint != "/mcp/tools" {
		t.Fatalf("rows = %+v", rows)
	}
	req, _ := box.Open(rows[0].Request, bodyAD("mcp-1", "request"))
	if !strings.Contains(string(req), `"q":"mail [REDACTED:email]"`) || !strings.Contains(string(req), `"method":"tools/call"`) {
		t.Fatalf("request = %s", req)
	}
	rows, _ = e.srv.store.RequestBodies(context.Background(), "mcp-2")
	resp, _ := box.Open(rows[0].Response, bodyAD("mcp-2", "response"))
	if string(resp) != `{"error":"upstream down"}` {
		t.Fatalf("error response = %s", resp)
	}
}
