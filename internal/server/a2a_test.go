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

// a2aAgent is a fake A2A agent: message/send answers with a new task
// that echoes the text, message/stream with two events, and tasks/get
// and tasks/cancel with the task named.
type a2aAgent struct {
	*httptest.Server
	mu    sync.Mutex
	calls []map[string]any
	auth  []string
	n     int
}

func newA2AAgent(t *testing.T) *a2aAgent {
	a := &a2aAgent{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/.well-known/agent-card.json" {
			_, _ = io.WriteString(w, `{"name":"Planner","description":"Plans trips","url":"http://internal/a2a","version":"1","protocolVersion":"0.3.0",`+
				`"capabilities":{"streaming":true,"pushNotifications":true},"securitySchemes":{"internal":{"type":"apiKey","in":"header","name":"X-Internal"}},`+
				`"security":[{"internal":[]}],"additionalInterfaces":[{"url":"http://internal/grpc","transport":"GRPC"}],"signatures":[{"protected":"x","signature":"y"}],`+
				`"skills":[{"id":"plan","name":"Plan","description":"Plans","tags":["travel"]}],"defaultInputModes":["text"],"defaultOutputModes":["text"]}`)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		a.mu.Lock()
		a.calls = append(a.calls, req)
		a.auth = append(a.auth, r.Header.Get("X-Internal")+"|"+r.Header.Get("Authorization"))
		a.n++
		n := a.n
		a.mu.Unlock()
		params, _ := req["params"].(map[string]any)
		id := req["id"]
		reply := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		}
		switch req["method"] {
		case "message/send":
			msg := params["message"].(map[string]any)
			text := msg["parts"].([]any)[0].(map[string]any)["text"].(string)
			taskID, _ := msg["taskId"].(string)
			if taskID == "" {
				taskID = "task-" + string(rune('0'+n))
			}
			ctxID, _ := msg["contextId"].(string)
			if ctxID == "" {
				ctxID = "ctx-" + string(rune('0'+n))
			}
			reply(map[string]any{"kind": "task", "id": taskID, "contextId": ctxID, "status": map[string]any{"state": "completed"},
				"artifacts": []any{map[string]any{"artifactId": "a", "parts": []any{map[string]any{"kind": "text", "text": "echo: " + text}}}}})
		case "message/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range []string{
				`{"jsonrpc":"2.0","id":1,"result":{"kind":"task","id":"stream-task","contextId":"stream-ctx","status":{"state":"working"}}}`,
				`{"jsonrpc":"2.0","id":1,"result":{"kind":"status-update","taskId":"stream-task","contextId":"stream-ctx","status":{"state":"completed"},"final":true}}`,
			} {
				_, _ = io.WriteString(w, "data: "+ev+"\n\n")
				w.(http.Flusher).Flush()
			}
		case "tasks/get", "tasks/cancel":
			reply(map[string]any{"kind": "task", "id": params["id"], "contextId": "ctx", "status": map[string]any{"state": "completed"}})
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "no"}})
		}
	}))
	t.Cleanup(a.Close)
	return a
}

func rpc(method, params string) string {
	return `{"jsonrpc":"2.0","id":7,"method":"` + method + `","params":` + params + `}`
}

func sendText(text, extra string) string {
	return rpc("message/send", `{"message":{"role":"user","messageId":"m1","parts":[{"kind":"text","text":"`+text+`"}]`+extra+`}}`)
}

type rpcReply struct {
	ID     any            `json:"id"`
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    int
		Message string
	} `json:"error"`
}

func callA2A(t *testing.T, h http.Handler, agent, key, body string) rpcReply {
	t.Helper()
	rec := do(h, "POST", "/a2a/"+agent, key, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s = %d %s", body, rec.Code, rec.Body)
	}
	var r rpcReply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return r
}

func a2aEnv(t *testing.T, agent *a2aAgent) *adminEnv {
	return newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
a2a:
  public_url: https://gw.example.com
  agents:
    planner: {url: "`+agent.URL+`/a2a", headers: {X-Internal: agent-secret}, guardrails: [no-email]}
    plain: {url: "`+agent.URL+`/a2a", description: Plain agent}
guardrails:
  no-email: {type: pii, detect: [email], action: redact, stages: [input]}
  no-ssn: {type: pii, detect: [us_ssn], action: block, stages: [input]}
keys:
  - {name: a, key: `+devKey+`}
  - {name: b, key: `+guardKey+`}
`)
}

func TestA2ARelayAndOwnership(t *testing.T) {
	agent := newA2AAgent(t)
	e := a2aEnv(t, agent)

	// A message starts a task; the agent's credentials are sent, the
	// caller's are not, and the input guardrail redacts the email.
	r := callA2A(t, e.h, "planner", devKey, sendText("mail ann@example.com", ""))
	if r.Error != nil || r.Result["id"] != "task-1" || r.ID != 7.0 {
		t.Fatalf("send = %+v", r)
	}
	agent.mu.Lock()
	sent := agent.calls[0]["params"].(map[string]any)["message"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
	auth := agent.auth[0]
	agent.mu.Unlock()
	if strings.Contains(sent, "ann@example.com") || auth != "agent-secret|" {
		t.Fatalf("agent got %q with %q", sent, auth)
	}

	// The owner may get, continue and cancel the task; another key
	// learns nothing about it, nor about its context.
	if r := callA2A(t, e.h, "planner", devKey, rpc("tasks/get", `{"id":"task-1"}`)); r.Error != nil || r.Result["id"] != "task-1" {
		t.Fatalf("get = %+v", r)
	}
	if r := callA2A(t, e.h, "planner", devKey, sendText("again", `,"taskId":"task-1","contextId":"ctx-1"`)); r.Error != nil {
		t.Fatalf("continue = %+v", r)
	}
	for _, body := range []string{
		rpc("tasks/get", `{"id":"task-1"}`),
		rpc("tasks/cancel", `{"id":"task-1"}`),
		sendText("hi", `,"taskId":"task-1"`),
		sendText("hi", `,"contextId":"ctx-1"`),
		rpc("tasks/get", `{"id":"never-seen"}`),
	} {
		if r := callA2A(t, e.h, "planner", guardKey, body); r.Error == nil || r.Error.Code != a2aTaskNotFound {
			t.Errorf("another key: %s = %+v", body, r)
		}
	}
	// The same id at another agent is a different task.
	if r := callA2A(t, e.h, "plain", devKey, rpc("tasks/get", `{"id":"task-1"}`)); r.Error == nil || r.Error.Code != a2aTaskNotFound {
		t.Errorf("other agent = %+v", r)
	}
	// A new context chosen by the client is fine.
	if r := callA2A(t, e.h, "plain", guardKey, sendText("hi", `,"contextId":"my-own"`)); r.Error != nil {
		t.Errorf("new context = %+v", r)
	}

	// Streams pass through and their task is recorded.
	rec := do(e.h, "POST", "/a2a/planner", devKey, rpc("message/stream", `{"message":{"role":"user","messageId":"m","parts":[{"kind":"text","text":"go"}]}}`))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" || strings.Count(rec.Body.String(), "data: ") != 2 {
		t.Fatalf("stream = %d %s", rec.Code, rec.Body)
	}
	if r := callA2A(t, e.h, "planner", devKey, rpc("tasks/get", `{"id":"stream-task"}`)); r.Error != nil {
		t.Fatalf("streamed task = %+v", r)
	}
	if r := callA2A(t, e.h, "planner", guardKey, rpc("tasks/get", `{"id":"stream-task"}`)); r.Error == nil {
		t.Fatal("another key read the streamed task")
	}
}

func TestA2ARefusals(t *testing.T) {
	agent := newA2AAgent(t)
	e := a2aEnv(t, agent)
	for _, c := range []struct {
		agent, key string
		status     int
	}{
		{"planner", "", http.StatusUnauthorized},
		{"planner", "wrong", http.StatusUnauthorized},
		{"missing", devKey, http.StatusNotFound},
	} {
		if rec := do(e.h, "POST", "/a2a/"+c.agent, c.key, sendText("hi", "")); rec.Code != c.status {
			t.Errorf("%s with %q = %d", c.agent, c.key, rec.Code)
		}
	}
	for body, code := range map[string]int{
		`not json`:                     rpcParseError,
		`[` + sendText("hi", "") + `]`: rpcInvalidRequest,
		`{"jsonrpc":"1.0","id":1,"method":"tasks/get"}`:                                                                   rpcInvalidRequest,
		rpc("agent/getAuthenticatedExtendedCard", `{}`):                                                                   rpcMethodNotFound,
		rpc("tasks/pushNotificationConfig/set", `{}`):                                                                     a2aPushNotSupported,
		rpc("message/send", `{"message":{"parts":[]},"configuration":{"pushNotificationConfig":{"url":"https://evil"}}}`): a2aPushNotSupported,
		rpc("tasks/get", `{}`):    rpcInvalidParams,
		rpc("message/send", `{}`): rpcInvalidParams,
	} {
		if r := callA2A(t, e.h, "planner", devKey, body); r.Error == nil || r.Error.Code != code {
			t.Errorf("%s = %+v, want %d", body, r, code)
		}
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.calls) != 0 {
		t.Errorf("refused requests reached the agent: %v", agent.calls)
	}
}

func TestA2APolicyAndGuardrailBlock(t *testing.T) {
	agent := newA2AAgent(t)
	e := newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
a2a:
  agents:
    planner: {url: "`+agent.URL+`/a2a", guardrails: [no-ssn]}
    plain: {url: "`+agent.URL+`/a2a"}
guardrails:
  no-ssn: {type: pii, detect: [us_ssn], action: block, stages: [input]}
`)
	var org, team, key idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"t","allowed_agents":["planner"]}`, 201, &team)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"k"}`, 201, &key)
	if rec := do(e.h, "POST", "/a2a/plain", key.Key, sendText("hi", "")); rec.Code != http.StatusForbidden || errorCode(t, rec) != codeAgentNotAllowed {
		t.Fatalf("not allowed = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "GET", "/a2a/plain/.well-known/agent-card.json", key.Key, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("card not allowed = %d", rec.Code)
	}
	if r := callA2A(t, e.h, "planner", key.Key, sendText("my ssn is 123-45-6789", "")); r.Error == nil || !strings.Contains(r.Error.Message, "no-ssn") {
		t.Fatalf("blocked = %+v", r)
	}
	if rec := do(e.h, "POST", "/admin/v1/orgs/"+org.ID+"/teams", adminToken, `{"name":"u","allowed_agents":["nope"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown agent in policy = %d", rec.Code)
	}
	if r := callA2A(t, e.h, "planner", key.Key, sendText("plan a trip", "")); r.Error != nil {
		t.Fatalf("allowed = %+v", r)
	}
}

func TestA2ACard(t *testing.T) {
	agent := newA2AAgent(t)
	e := a2aEnv(t, agent)
	if rec := do(e.h, "GET", "/a2a/planner/.well-known/agent-card.json", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key = %d", rec.Code)
	}
	for _, path := range []string{"/a2a/planner/.well-known/agent-card.json", "/a2a/plain/.well-known/agent.json"} {
		rec := do(e.h, "GET", path, devKey, "")
		var card map[string]any
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &card) != nil {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
		name := strings.Split(path, "/")[2]
		caps := card["capabilities"].(map[string]any)
		schemes, _ := json.Marshal(card["securitySchemes"])
		if card["url"] != "https://gw.example.com/a2a/"+name || card["preferredTransport"] != "JSONRPC" || card["additionalInterfaces"] != nil ||
			card["signatures"] != nil || caps["pushNotifications"] != false || caps["streaming"] != true ||
			string(schemes) != `{"gateway":{"description":"A gateway API key","scheme":"bearer","type":"http"}}` || card["skills"] == nil {
			t.Fatalf("%s card = %s", path, rec.Body)
		}
		if name == "plain" && card["description"] != "Plain agent" {
			t.Fatalf("description = %v", card["description"])
		}
	}
}

func TestA2AWithoutDatabase(t *testing.T) {
	agent := newA2AAgent(t)
	h := gateway(t, `
a2a: {agents: {plain: {url: "`+agent.URL+`/a2a"}}}
keys: [{name: a, key: `+devKey+`}]
`, nil)
	if r := callA2A(t, h, "plain", devKey, sendText("hi", "")); r.Error != nil {
		t.Fatalf("send = %+v", r)
	}
	if r := callA2A(t, h, "plain", devKey, rpc("tasks/get", `{"id":"task-1"}`)); r.Error == nil || r.Error.Code != a2aUnsupported {
		t.Fatalf("get = %+v", r)
	}
}
