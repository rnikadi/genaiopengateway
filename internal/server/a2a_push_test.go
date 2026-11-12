package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// Callers set push notifications through the gateway: the agent gets a
// URL at the gateway, and what it posts there reaches the caller's
// webhook with the caller's token (docs/a2a.md#push-notifications).
func TestA2APushNotifications(t *testing.T) {
	var mu sync.Mutex
	var agentSaw []map[string]any // push configurations as the agent got them
	var hooks []string            // "token|authorization|body" per delivery
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		hooks = append(hooks, r.Header.Get("X-A2A-Notification-Token")+"|"+r.Header.Get("Authorization")+"|"+string(raw))
		mu.Unlock()
		if strings.Contains(r.URL.Path, "broken") {
			http.Error(w, "no", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(hook.Close)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			_, _ = io.WriteString(w, `{"name":"P","supportedInterfaces":[{"url":"http://internal/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"capabilities":{"pushNotifications":true}}`)
			return
		}
		var req struct {
			ID     any
			Method string
			Params map[string]any
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		mu.Lock()
		defer mu.Unlock()
		switch req.Method {
		case "SendMessage":
			if conf, _ := req.Params["configuration"].(map[string]any); conf != nil {
				agentSaw = append(agentSaw, conf["taskPushNotificationConfig"].(map[string]any))
			}
			id := "t1"
			if len(agentSaw) > 0 {
				id = "t2"
			}
			reply(map[string]any{"task": map[string]any{"id": id, "contextId": "c-" + id, "status": map[string]any{"state": "TASK_STATE_WORKING"}}})
		case "CreateTaskPushNotificationConfig":
			agentSaw = append(agentSaw, req.Params)
			out := map[string]any{"id": "cfg1"}
			for k, v := range req.Params {
				out[k] = v
			}
			reply(out)
		case "GetTaskPushNotificationConfig", "ListTaskPushNotificationConfigs":
			reply(map[string]any{"configs": agentSaw})
		case "DeleteTaskPushNotificationConfig":
			reply(map[string]any{})
		}
	}))
	t.Cleanup(agent.Close)
	env := func(push string) *adminEnv {
		return newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
secrets: {encryption_key: "`+testEncryptionKey+`"}
a2a:
  public_url: https://gw.example.com
  push: `+push+`
  agents:
    planner: {url: "`+agent.URL+`/rpc", guardrails: [no-email]}
guardrails:
  no-email: {type: pii, detect: [email], action: redact, stages: [output]}
keys:
  - {name: a, key: `+devKey+`}
  - {name: b, key: `+guardKey+`}
`)
	}
	e := env(`{enabled: true, allowed_hosts: ["127.0.0.1"]}`)
	send := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"go"}]}}`
	if r := callA2A(t, e.h, "planner", devKey, rpc("SendMessage", send)); r.Error != nil {
		t.Fatalf("send = %+v", r)
	}
	notify := func(gatewayURL, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", strings.TrimPrefix(gatewayURL, "https://gw.example.com"), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/a2a+json")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	update := func(task string) string {
		return `{"statusUpdate":{"taskId":"` + task + `","contextId":"c","status":{"state":"TASK_STATE_COMPLETED","message":{"role":"ROLE_AGENT","parts":[{"text":"done, write to ann@example.com"}]}}}}`
	}

	// A configuration for a task: the agent gets the gateway's URL and
	// none of the caller's secrets; the caller gets its own URL back.
	create := `{"taskId":"t1","url":"` + hook.URL + `/hook","token":"tok-1","authentication":{"scheme":"Bearer","credentials":"cred-1"}}`
	r := callA2A(t, e.h, "planner", devKey, rpc("CreateTaskPushNotificationConfig", create))
	if r.Error != nil || r.Result["url"] != hook.URL+"/hook" || r.Result["id"] != "cfg1" {
		t.Fatalf("create = %+v", r)
	}
	mu.Lock()
	got := agentSaw[0]
	mu.Unlock()
	gwURL, _ := got["url"].(string)
	if !strings.HasPrefix(gwURL, "https://gw.example.com/a2a/planner/push/") || got["token"] != nil || got["authentication"] != nil {
		t.Fatalf("the agent got %v", got)
	}
	if rec := notify(gwURL, update("t1")); rec.Code != http.StatusNoContent {
		t.Fatalf("notify = %d %s", rec.Code, rec.Body)
	}
	mu.Lock()
	delivered := hooks[len(hooks)-1]
	mu.Unlock()
	if !strings.HasPrefix(delivered, "tok-1|Bearer cred-1|") || !strings.Contains(delivered, `"taskId":"t1"`) || strings.Contains(delivered, "ann@example.com") {
		t.Fatalf("the webhook got %s", delivered)
	}
	// The URL is for that task only, and nothing else is one.
	if rec := notify(gwURL, update("t2")); rec.Code != http.StatusForbidden {
		t.Errorf("a notification about another task = %d", rec.Code)
	}
	if rec := notify("https://gw.example.com/a2a/planner/push/guess", update("t1")); rec.Code != http.StatusNotFound {
		t.Errorf("a guessed URL = %d", rec.Code)
	}
	if rec := notify(gwURL, `[]`); rec.Code != http.StatusBadRequest {
		t.Errorf("not an object = %d", rec.Code)
	}
	if len(hooks) != 1 {
		t.Errorf("%d deliveries, want 1", len(hooks))
	}
	// Reading configurations shows the caller its own URL, and another
	// caller neither the task nor the URL.
	if r := callA2A(t, e.h, "planner", devKey, rpc("ListTaskPushNotificationConfigs", `{"taskId":"t1"}`)); r.Error != nil || !strings.Contains(mustJSON(r.Result), hook.URL+"/hook") || strings.Contains(mustJSON(r.Result), "/push/") {
		t.Errorf("list = %+v", r)
	}
	for _, body := range []string{rpc("CreateTaskPushNotificationConfig", create), rpc("ListTaskPushNotificationConfigs", `{"taskId":"t1"}`), rpc("DeleteTaskPushNotificationConfig", `{"taskId":"t1","id":"cfg1"}`)} {
		if r := callA2A(t, e.h, "planner", guardKey, body); r.Error == nil || r.Error.Code != a2aTaskNotFound {
			t.Errorf("another key: %s = %+v", body, r)
		}
	}
	// A webhook that fails tells the agent, which may try again.
	r = callA2A(t, e.h, "planner", devKey, rpc("CreateTaskPushNotificationConfig", `{"taskId":"t1","url":"`+hook.URL+`/broken"}`))
	if r.Error != nil {
		t.Fatalf("second configuration = %+v", r)
	}
	mu.Lock()
	broken, _ := agentSaw[1]["url"].(string)
	mu.Unlock()
	if rec := notify(broken, update("t1")); rec.Code != http.StatusBadGateway {
		t.Errorf("a failing webhook = %d", rec.Code)
	}
	// Deleting a configuration ends its URL.
	if r := callA2A(t, e.h, "planner", devKey, rpc("DeleteTaskPushNotificationConfig", `{"taskId":"t1","id":"cfg1"}`)); r.Error != nil {
		t.Fatalf("delete = %+v", r)
	}
	if rec := notify(gwURL, update("t1")); rec.Code != http.StatusNotFound {
		t.Errorf("after delete = %d", rec.Code)
	}

	// A configuration sent with a message is bound to the task the
	// agent first reports on.
	inline := strings.Replace(send, `}]}}`, `}]},"configuration":{"taskPushNotificationConfig":{"url":"`+hook.URL+`/inline"}}}`, 1)
	if r := callA2A(t, e.h, "planner", devKey, rpc("SendMessage", inline)); r.Error != nil {
		t.Fatalf("send with a configuration = %+v", r)
	}
	mu.Lock()
	inlineURL, _ := agentSaw[len(agentSaw)-1]["url"].(string)
	mu.Unlock()
	if rec := notify(inlineURL, `{"task":{"id":"t2","contextId":"c-t2","status":{"state":"TASK_STATE_COMPLETED"}}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("notify for a message's task = %d %s", rec.Code, rec.Body)
	}
	if rec := notify(inlineURL, update("t1")); rec.Code != http.StatusForbidden {
		t.Errorf("the URL was used for another task: %d", rec.Code)
	}

	// The card says push notifications work.
	if rec := do(e.h, "GET", "/a2a/planner/.well-known/agent-card.json", devKey, ""); !strings.Contains(rec.Body.String(), `"pushNotifications":true`) {
		t.Errorf("card: %s", rec.Body)
	}

	// Where webhooks may be: listed hosts only, when hosts are listed.
	for _, u := range []string{"https://hooks.example.org/x", "http://127.0.0.2/x", "https://user:pw@127.0.0.1/x", "ftp://127.0.0.1/x", ""} {
		r := callA2A(t, e.h, "planner", devKey, rpc("CreateTaskPushNotificationConfig", `{"taskId":"t1","url":"`+u+`"}`))
		if r.Error == nil || r.Error.Code != rpcInvalidParams {
			t.Errorf("url %q = %+v", u, r)
		}
	}
	// Off by default.
	off := env(`{}`)
	if r := callA2A(t, off.h, "planner", devKey, rpc("SendMessage", inline)); r.Error == nil || r.Error.Code != a2aPushNotSupported {
		t.Errorf("push when off = %+v", r)
	}
	if rec := do(off.h, "GET", "/a2a/planner/.well-known/agent-card.json", devKey, ""); !strings.Contains(rec.Body.String(), `"pushNotifications":false`) {
		t.Errorf("card when off: %s", rec.Body)
	}
}

// Without a list of hosts, webhooks must be public and use HTTPS.
func TestA2APushURLs(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://hooks.example.org/x":   true,
		"https://8.8.8.8/x":             true,
		"http://hooks.example.org/x":    false,
		"https://10.1.2.3/x":            false,
		"https://127.0.0.1/x":           false,
		"https://[::1]/x":               false,
		"https://169.254.169.254/x":     false,
		"https://100.64.0.1/x":          false,
		"https://[fd00::1]/x":           false,
		"https://localhost/x":           false,
		"https://db.internal/x":         false,
		"https://[::ffff:10.0.0.1]:8/x": false,
	} {
		if _, why := checkPushURL(config.A2APush{Enabled: true}, u); (why == "") != ok {
			t.Errorf("%s: %q", u, why)
		}
	}
	if trusted, why := checkPushURL(config.A2APush{AllowedHosts: []string{"*.corp.example"}}, "http://hooks.corp.example/x"); !trusted || why != "" {
		t.Errorf("a listed host: %v %q", trusted, why)
	}
	if _, why := checkPushURL(config.A2APush{AllowedHosts: []string{"*.corp.example"}}, "https://corp.example.evil.org/x"); why == "" {
		t.Error("a host that only looks listed was accepted")
	}
	// A public name that resolves to an internal address is refused when
	// it is connected to.
	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer internal.Close()
	if _, err := newPublicClient().Get(internal.URL); err == nil || !strings.Contains(err.Error(), "not public") {
		t.Errorf("connecting to a loopback address: %v", err)
	}
	if !publicIP(net.ParseIP("93.184.216.34")) || publicIP(net.ParseIP("192.168.1.1")) {
		t.Error("publicIP")
	}
}
