package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// a2aAgent10 is a fake agent that speaks A2A 1.0 over JSON-RPC at /rpc
// and over HTTP+JSON at /rest. It records what it was sent.
type a2aAgent10 struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string // "METHOD path version | body"
}

func newA2AAgent10(t *testing.T) *a2aAgent10 {
	a := &a2aAgent10{}
	task := func(id, text string) map[string]any {
		return map[string]any{"id": id, "contextId": "ctx-" + id, "status": map[string]any{"state": "TASK_STATE_COMPLETED"},
			"artifacts": []any{map[string]any{"artifactId": "a", "parts": []any{map[string]any{"text": "echo: " + text}}}}}
	}
	events := []string{
		`{"task":{"id":"stream-task","contextId":"stream-ctx","status":{"state":"TASK_STATE_WORKING"}}}`,
		`{"statusUpdate":{"taskId":"stream-task","contextId":"stream-ctx","status":{"state":"TASK_STATE_COMPLETED"}}}`,
	}
	textOf := func(msg map[string]any) string {
		parts, _ := msg["parts"].([]any)
		if len(parts) == 0 {
			return ""
		}
		s, _ := parts[0].(map[string]any)["text"].(string)
		return s
	}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.seen = append(a.seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("A2A-Version")+" "+r.Header.Get("X-Internal")+r.Header.Get("Authorization")+" | "+string(raw))
		a.mu.Unlock()
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		switch {
		case r.URL.Path == "/.well-known/agent-card.json":
			_, _ = io.WriteString(w, `{"name":"Planner","description":"Plans trips","version":"1",`+
				`"supportedInterfaces":[{"url":"http://internal/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"},{"url":"http://internal/rest","protocolBinding":"HTTP+JSON","protocolVersion":"1.0"},{"url":"http://internal/grpc","protocolBinding":"GRPC","protocolVersion":"1.0"}],`+
				`"capabilities":{"streaming":true,"pushNotifications":true,"extendedAgentCard":true},"securitySchemes":{"internal":{"apiKeySecurityScheme":{"location":"header","name":"X-Internal"}}},`+
				`"securityRequirements":[{"schemes":{"internal":{"list":[]}}}],"signatures":[{"protected":"x","signature":"y"}],"skills":[],"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"]}`)
		case r.URL.Path == "/rpc":
			params, _ := body["params"].(map[string]any)
			reply := func(result any) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": body["id"], "result": result})
			}
			switch body["method"] {
			case "SendMessage":
				msg, _ := params["message"].(map[string]any)
				reply(map[string]any{"task": task("rpc-task", textOf(msg))})
			case "SendStreamingMessage":
				w.Header().Set("Content-Type", "text/event-stream")
				for _, ev := range events {
					_, _ = io.WriteString(w, `data: {"jsonrpc":"2.0","id":1,"result":`+ev+"}\n\n")
				}
			case "GetTask", "CancelTask":
				id, _ := params["id"].(string)
				reply(task(id, "got"))
			}
		case r.URL.Path == "/rest/message:send":
			msg, _ := body["message"].(map[string]any)
			w.Header().Set("Content-Type", "application/a2a+json")
			_ = json.NewEncoder(w).Encode(map[string]any{"task": task("rest-task", textOf(msg))})
		case r.URL.Path == "/rest/message:stream" || strings.HasSuffix(r.URL.Path, ":subscribe"):
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range events {
				_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			}
		case strings.HasSuffix(r.URL.Path, ":cancel"):
			w.Header().Set("Content-Type", "application/a2a+json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":400,"status":"FAILED_PRECONDITION","message":"done already","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TASK_NOT_CANCELABLE","domain":"a2a-protocol.org"}]}}`)
		case strings.HasPrefix(r.URL.Path, "/rest/tasks/"), strings.HasPrefix(r.URL.Path, "/rest/v1/tasks/"):
			w.Header().Set("Content-Type", "application/a2a+json")
			_ = json.NewEncoder(w).Encode(task(r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:], "got"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *a2aAgent10) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[len(a.seen)-1]
}

// A2A 1.0 over JSON-RPC and the HTTP+JSON binding get the checks that
// A2A 0.3 over JSON-RPC gets (docs/a2a.md).
func TestA2ABindings(t *testing.T) {
	agent := newA2AAgent10(t)
	e := newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
a2a:
  public_url: https://gw.example.com
  agents:
    planner: {url: "`+agent.URL+`/rpc", rest_url: "`+agent.URL+`/rest/", headers: {X-Internal: agent-secret}, guardrails: [no-email]}
    restonly: {rest_url: "`+agent.URL+`/rest"}
    rpconly: {url: "`+agent.URL+`/rpc"}
    checked: {rest_url: "`+agent.URL+`/rest", guardrails: [no-echo]}
guardrails:
  no-email: {type: pii, detect: [email], action: redact, stages: [input]}
  no-echo: {type: regex, patterns: ['echo'], action: block, stages: [output]}
keys:
  - {name: a, key: `+devKey+`}
  - {name: b, key: `+guardKey+`}
`)
	rest := func(method, agentName, path, key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/a2a/"+agentName+"/"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/a2a+json")
		req.Header.Set("A2A-Version", "1.0")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	send := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"mail ann@example.com"}]}}`

	// JSON-RPC with the method names of A2A 1.0.
	r := callA2A(t, e.h, "planner", devKey, rpc("SendMessage", send))
	if r.Error != nil || r.Result["task"].(map[string]any)["id"] != "rpc-task" {
		t.Fatalf("SendMessage = %+v", r)
	}
	if got := agent.last(); strings.Contains(got, "ann@example.com") || !strings.Contains(got, "agent-secret") || strings.Contains(got, devKey) {
		t.Fatalf("the agent got %s", got)
	}
	if r := callA2A(t, e.h, "planner", devKey, rpc("GetTask", `{"id":"rpc-task"}`)); r.Error != nil || r.Result["id"] != "rpc-task" {
		t.Fatalf("GetTask = %+v", r)
	}
	for _, body := range []string{rpc("GetTask", `{"id":"rpc-task"}`), rpc("CancelTask", `{"id":"rpc-task"}`), rpc("SubscribeToTask", `{"id":"rpc-task"}`)} {
		if r := callA2A(t, e.h, "planner", guardKey, body); r.Error == nil || r.Error.Code != a2aTaskNotFound {
			t.Errorf("another key: %s = %+v", body, r)
		}
	}
	for body, code := range map[string]int{
		rpc("ListTasks", `{}`): a2aUnsupported,
		rpc("CreateTaskPushNotificationConfig", `{"taskId":"rpc-task","url":"http://x"}`):                                              a2aPushNotSupported,
		rpc("SendMessage", strings.Replace(send, `}]}}`, `}]},"configuration":{"taskPushNotificationConfig":{"url":"http://x"}}}`, 1)): a2aPushNotSupported,
		rpc("GetExtendedAgentCard", `{}`): rpcMethodNotFound,
	} {
		if r := callA2A(t, e.h, "planner", devKey, body); r.Error == nil || r.Error.Code != code {
			t.Errorf("%s = %+v, want %d", body, r, code)
		}
	}
	if rec := do(e.h, "POST", "/a2a/planner", devKey, rpc("SendStreamingMessage", send)); !strings.Contains(rec.Body.String(), "stream-task") {
		t.Fatalf("stream: %s", rec.Body)
	}
	if r := callA2A(t, e.h, "planner", devKey, rpc("GetTask", `{"id":"stream-task"}`)); r.Error != nil {
		t.Fatalf("a task from a stream is not the caller's: %+v", r)
	}

	// The HTTP+JSON binding.
	rec := rest("POST", "planner", "message:send", devKey, send)
	var sent struct{ Task struct{ ID string } }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &sent) != nil || sent.Task.ID != "rest-task" || !strings.Contains(rec.Header().Get("Content-Type"), "a2a+json") {
		t.Fatalf("message:send = %d %s", rec.Code, rec.Body)
	}
	got := agent.last()
	if !strings.HasPrefix(got, "POST /rest/message:send 1.0 agent-secret |") || strings.Contains(got, "ann@example.com") || !strings.Contains(got, `"ROLE_USER"`) {
		t.Fatalf("the agent got %s", got)
	}
	if rec := rest("GET", "planner", "tasks/rest-task?historyLength=2", devKey, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"rest-task"`) || !strings.HasPrefix(agent.last(), "GET /rest/tasks/rest-task?historyLength=2 ") {
		t.Fatalf("get task = %d %s (%s)", rec.Code, rec.Body, agent.last())
	}
	// The paths of A2A 0.3 have /v1 in front.
	if rec := rest("GET", "planner", "v1/tasks/rest-task", devKey, ""); rec.Code != 200 || !strings.HasPrefix(agent.last(), "GET /rest/v1/tasks/rest-task ") {
		t.Fatalf("0.3 path = %d (%s)", rec.Code, agent.last())
	}
	// The agent's own errors pass through.
	if rec := rest("POST", "planner", "tasks/rest-task:cancel", devKey, `{}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "TASK_NOT_CANCELABLE") {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
	}
	for _, method := range []string{"POST", "GET"} {
		if rec := rest(method, "planner", "tasks/rest-task:subscribe", devKey, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "statusUpdate") || rec.Header().Get("Content-Type") != "text/event-stream" {
			t.Fatalf("%s subscribe = %d %s", method, rec.Code, rec.Body)
		}
	}
	if rec := rest("POST", "planner", "message:stream", devKey, send); rec.Code != 200 || !strings.Contains(rec.Body.String(), "stream-ctx") {
		t.Fatalf("message:stream = %d %s", rec.Code, rec.Body)
	}

	// Another key learns nothing of the task, in the binding's error shape.
	calls := len(agent.seen)
	for _, c := range [][3]string{
		{"GET", "tasks/rest-task", ""}, {"POST", "tasks/rest-task:cancel", "{}"}, {"POST", "tasks/rest-task:subscribe", ""},
		{"POST", "message:send", strings.Replace(send, `"role"`, `"taskId":"rest-task","role"`, 1)},
		{"POST", "message:send", strings.Replace(send, `"role"`, `"contextId":"ctx-rest-task","role"`, 1)},
	} {
		rec := rest(c[0], "planner", c[1], guardKey, c[2])
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"reason":"TASK_NOT_FOUND"`) || !strings.Contains(rec.Body.String(), `"status":"NOT_FOUND"`) {
			t.Errorf("another key: %s %s = %d %s", c[0], c[1], rec.Code, rec.Body)
		}
	}
	// What the gateway refuses.
	for _, c := range []struct {
		method, path, body string
		status             int
		reason             string
	}{
		{"GET", "tasks", "", 400, "UNSUPPORTED_OPERATION"},
		{"POST", "tasks/rest-task/pushNotificationConfigs", `{"url":"http://x"}`, 400, "PUSH_NOTIFICATION_NOT_SUPPORTED"},
		{"GET", "tasks/rest-task/pushNotificationConfigs", "", 400, "PUSH_NOTIFICATION_NOT_SUPPORTED"},
		{"POST", "message:send", strings.Replace(send, `}]}}`, `}]},"configuration":{"taskPushNotificationConfig":{"url":"http://x"}}}`, 1), 400, "PUSH_NOTIFICATION_NOT_SUPPORTED"},
		{"POST", "v1/message:send", strings.Replace(send, `}]}}`, `}]},"configuration":{"pushNotification":{"url":"http://x"}}}`, 1), 400, "PUSH_NOTIFICATION_NOT_SUPPORTED"},
		{"GET", "extendedAgentCard", "", 404, ""},
		{"DELETE", "tasks/rest-task", "", 404, ""},
		{"POST", "message:send", "not json", 400, ""},
	} {
		rec := rest(c.method, "planner", c.path, devKey, c.body)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.reason) || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s %s = %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if len(agent.seen) != calls {
		t.Errorf("%d refused requests reached the agent", len(agent.seen)-calls)
	}
	// An agent is offered only over the bindings it has.
	if rec := rest("POST", "rpconly", "message:send", devKey, send); rec.Code != 400 || !strings.Contains(rec.Body.String(), "JSON-RPC") {
		t.Errorf("HTTP+JSON for a JSON-RPC agent = %d %s", rec.Code, rec.Body)
	}
	if r := callA2A(t, e.h, "restonly", devKey, rpc("SendMessage", send)); r.Error == nil || r.Error.Code != a2aUnsupported {
		t.Errorf("JSON-RPC for an HTTP+JSON agent = %+v", r)
	}
	if rec := rest("POST", "restonly", "message:send", "wrong-key", send); rec.Code != 401 {
		t.Errorf("a wrong key = %d", rec.Code)
	}
	// Output guardrails check an HTTP+JSON result, and refuse streams.
	if rec := rest("POST", "checked", "message:send", devKey, send); rec.Code != 500 || !strings.Contains(rec.Body.String(), "INVALID_AGENT_RESPONSE") {
		t.Errorf("a blocked result = %d %s", rec.Code, rec.Body)
	}
	if rec := rest("POST", "checked", "message:stream", devKey, send); rec.Code != 400 || !strings.Contains(rec.Body.String(), "UNSUPPORTED_OPERATION") {
		t.Errorf("a stream with output guardrails = %d %s", rec.Code, rec.Body)
	}

	// The card of A2A 1.0 names the gateway's interfaces and its key.
	card := func(name string) map[string]any {
		var c map[string]any
		rec := do(e.h, "GET", "/a2a/"+name+"/.well-known/agent-card.json", devKey, "")
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &c) != nil {
			t.Fatalf("card of %s = %d %s", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "internal") || c["signatures"] != nil {
			t.Fatalf("the card of %s shows the agent's own details: %s", name, rec.Body)
		}
		return c
	}
	c := card("planner")
	want := `[{"protocolBinding":"JSONRPC","protocolVersion":"1.0","url":"https://gw.example.com/a2a/planner"},{"protocolBinding":"HTTP+JSON","protocolVersion":"1.0","url":"https://gw.example.com/a2a/planner"}]`
	if got := mustJSON(c["supportedInterfaces"]); got != want {
		t.Errorf("interfaces: %s", got)
	}
	caps := c["capabilities"].(map[string]any)
	if caps["pushNotifications"] != false || caps["extendedAgentCard"] != false || caps["streaming"] != true {
		t.Errorf("capabilities: %v", caps)
	}
	if got := mustJSON(c["securitySchemes"]) + mustJSON(c["securityRequirements"]); !strings.Contains(got, `"httpAuthSecurityScheme"`) || !strings.Contains(got, `{"schemes":{"gateway":{"list":[]}}}`) {
		t.Errorf("security: %s", got)
	}
	if got := mustJSON(card("restonly")["supportedInterfaces"]); strings.Contains(got, "JSONRPC") || !strings.Contains(got, "HTTP+JSON") {
		t.Errorf("interfaces of an HTTP+JSON agent: %s", got)
	}
}
