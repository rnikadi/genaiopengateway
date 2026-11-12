package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/sse"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Error codes for A2A requests refused before JSON-RPC (docs/api.md).
const (
	codeAgentNotFound   = "agent_not_found"
	codeAgentNotAllowed = "agent_not_allowed"
)

// JSON-RPC and A2A error codes.
const (
	rpcParseError       = -32700
	rpcInvalidRequest   = -32600
	rpcMethodNotFound   = -32601
	rpcInvalidParams    = -32602
	rpcInternalError    = -32603
	a2aTaskNotFound     = -32001
	a2aPushNotSupported = -32003
	a2aUnsupported      = -32004
	a2aInvalidResponse  = -32006
)

const (
	// a2aCardTTL is how long an agent's card is reused.
	a2aCardTTL = 5 * time.Minute
	// maxA2AResponse bounds a response that does not stream.
	maxA2AResponse = 64 << 20
)

// a2aOp is one A2A operation the gateway relays (ADR 0033).
type a2aOp struct {
	// label names the operation in metrics and logs, whatever the
	// binding and protocol version of the request.
	label string
	// message is true when the request carries a message; otherwise it
	// names a task.
	message bool
	streams bool
	// push is set, get, list or delete for an operation on a task's
	// push notification configurations.
	push string
}

var (
	opSend      = a2aOp{label: "message/send", message: true}
	opStream    = a2aOp{label: "message/stream", message: true, streams: true}
	opGet       = a2aOp{label: "tasks/get"}
	opCancel    = a2aOp{label: "tasks/cancel"}
	opSubscribe = a2aOp{label: "tasks/resubscribe", streams: true}

	opPushSet    = a2aOp{label: "push/set", push: "set"}
	opPushGet    = a2aOp{label: "push/get", push: "get"}
	opPushList   = a2aOp{label: "push/list", push: "list"}
	opPushDelete = a2aOp{label: "push/delete", push: "delete"}
)

// a2aMethods are the JSON-RPC methods relayed to agents: the names of
// A2A 0.3, and those of A2A 1.0.
var a2aMethods = map[string]a2aOp{
	"message/send":      opSend,
	"message/stream":    opStream,
	"tasks/get":         opGet,
	"tasks/cancel":      opCancel,
	"tasks/resubscribe": opSubscribe,

	"SendMessage":          opSend,
	"SendStreamingMessage": opStream,
	"GetTask":              opGet,
	"CancelTask":           opCancel,
	"SubscribeToTask":      opSubscribe,

	"tasks/pushNotificationConfig/set":    opPushSet,
	"tasks/pushNotificationConfig/get":    opPushGet,
	"tasks/pushNotificationConfig/list":   opPushList,
	"tasks/pushNotificationConfig/delete": opPushDelete,
	"CreateTaskPushNotificationConfig":    opPushSet,
	"GetTaskPushNotificationConfig":       opPushGet,
	"ListTaskPushNotificationConfigs":     opPushList,
	"DeleteTaskPushNotificationConfig":    opPushDelete,
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// a2aCall is one A2A request on its way through the gateway.
type a2aCall struct {
	s      *Server
	rt     *runtime
	name   string
	agent  config.A2AAgent
	p      *auth.Principal
	rid    string
	id     json.RawMessage
	method string
	// status is what metrics and the log record: ok, or why not.
	status string
	// seen are the ids already recorded in this call.
	seen map[string]bool
	// rest is true for a request over the HTTP+JSON binding, whose
	// errors are HTTP errors and whose results have no envelope.
	rest bool
	// op is the operation, and for one on push notification
	// configurations pushTask and pushConfig are the task and the
	// configuration it names.
	op                   a2aOp
	pushTask, pushConfig string
}

// restErrors maps a JSON-RPC or A2A error code to the HTTP status, the
// google.rpc code name and the A2A reason of the HTTP+JSON binding.
var restErrors = map[int]struct {
	status       int
	name, reason string
}{
	rpcParseError:       {http.StatusBadRequest, "INVALID_ARGUMENT", ""},
	rpcInvalidRequest:   {http.StatusBadRequest, "INVALID_ARGUMENT", ""},
	rpcInvalidParams:    {http.StatusBadRequest, "INVALID_ARGUMENT", ""},
	rpcMethodNotFound:   {http.StatusNotFound, "NOT_FOUND", ""},
	rpcInternalError:    {http.StatusInternalServerError, "INTERNAL", ""},
	a2aTaskNotFound:     {http.StatusNotFound, "NOT_FOUND", "TASK_NOT_FOUND"},
	a2aPushNotSupported: {http.StatusBadRequest, "FAILED_PRECONDITION", "PUSH_NOTIFICATION_NOT_SUPPORTED"},
	a2aUnsupported:      {http.StatusBadRequest, "FAILED_PRECONDITION", "UNSUPPORTED_OPERATION"},
	a2aInvalidResponse:  {http.StatusInternalServerError, "INTERNAL", "INVALID_AGENT_RESPONSE"},
}

// rpcError writes an error in the request's binding. JSON-RPC errors
// travel with HTTP 200, as A2A clients expect; the HTTP+JSON binding
// answers with an HTTP status and a google.rpc.Status body.
func (c *a2aCall) rpcError(w http.ResponseWriter, code int, status, msg string) {
	c.status = status
	if c.rest {
		e := restErrors[code]
		if e.status == 0 {
			e = restErrors[rpcInternalError]
		}
		body := map[string]any{"code": e.status, "status": e.name, "message": msg}
		if e.reason != "" {
			body["details"] = []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": e.reason, "domain": "a2a-protocol.org"}}
		}
		w.Header().Set("Content-Type", "application/a2a+json")
		w.WriteHeader(e.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
		return
	}
	id := c.id
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

// begin authenticates the caller, finds the agent and applies policy
// and rate limits. It returns the call, the request with the caller in
// its context and a function to defer, or false after writing an error
// in the gateway's own shape.
func (s *Server) beginA2A(w http.ResponseWriter, r *http.Request, rest bool) (*a2aCall, *http.Request, func(), bool) {
	start := time.Now()
	rt := s.rt.Load()
	c := &a2aCall{s: s, rt: rt, name: r.PathValue("name"), rid: r.Header.Get("X-Request-Id"), method: "other", status: "ok", seen: map[string]bool{}, rest: rest}
	p, ok := s.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="genaiopengateway"`)
		writeError(w, provider.ChatCompletions, http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
		return nil, nil, nil, false
	}
	agent, ok := rt.cfg.A2A.Agents[c.name]
	if !ok {
		writeError(w, provider.ChatCompletions, http.StatusNotFound, codeAgentNotFound, "agent "+c.name+" is not configured")
		return nil, nil, nil, false
	}
	c.p, c.agent = p, agent
	done := func() {
		d := time.Since(start)
		s.metrics.ObserveA2ARequest(c.name, c.method, c.status, d)
		s.log.Info("a2a request", "request_id", c.rid, "agent", c.name, "method", c.method, "status", c.status,
			"org_id", p.OrgID, "team_id", p.TeamID, "key_id", p.KeyID, "duration_ms", d.Milliseconds())
	}
	if !p.AllowsAgent(c.name) {
		c.status = "denied"
		writeError(w, provider.ChatCompletions, http.StatusForbidden, codeAgentNotAllowed, "you may not use agent "+c.name)
		done()
		return nil, nil, nil, false
	}
	r = r.WithContext(s.callerContext(r.Context(), p))
	if status, code, msg, ok := s.admit(w, r, p, c.rid); !ok {
		c.status = "rate_limited"
		writeError(w, provider.ChatCompletions, status, code, msg)
		done()
		return nil, nil, nil, false
	}
	return c, r, done, true
}

// serveA2A relays a JSON-RPC request to an A2A agent: the caller must
// be allowed the agent, a task or context it names must be its own,
// and guardrails check the message (docs/a2a.md).
func (s *Server) serveA2A(w http.ResponseWriter, r *http.Request) {
	c, r, done, ok := s.beginA2A(w, r, false)
	if !ok {
		return
	}
	defer done()
	rt, agent := c.rt, c.agent
	if agent.URL == "" {
		c.rpcError(w, a2aUnsupported, "invalid", "this agent is not offered over JSON-RPC; use the HTTP+JSON binding")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rt.cfg.Limits.MaxRequestBytes))
	if err != nil {
		c.rpcError(w, rpcInvalidRequest, "invalid", "the request is too large or could not be read")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			c.rpcError(w, rpcInvalidRequest, "invalid", "batch requests are not supported")
			return
		}
		c.rpcError(w, rpcParseError, "invalid", "the request is not JSON")
		return
	}
	c.id = req.ID
	op, known := a2aMethods[req.Method]
	switch {
	case req.JSONRPC != "2.0" || req.Method == "":
		c.rpcError(w, rpcInvalidRequest, "invalid", "not a JSON-RPC 2.0 request")
		return
	case req.Method == "tasks/list" || req.Method == "ListTasks":
		c.rpcError(w, a2aUnsupported, "invalid", listTasksRefused)
		return
	case !known:
		c.rpcError(w, rpcMethodNotFound, "invalid", "method "+req.Method+" is not available through the gateway")
		return
	}
	c.method = op.label
	if op.streams && rt.guards.Applies(config.StageOutput, agent.Guardrails) {
		c.rpcError(w, a2aUnsupported, "invalid", "streaming is not available for this agent, since its output guardrails check whole results")
		return
	}
	var params map[string]any
	dec := json.NewDecoder(bytes.NewReader(req.Params))
	dec.UseNumber()
	if len(req.Params) == 0 || dec.Decode(&params) != nil || params == nil {
		c.rpcError(w, rpcInvalidParams, "invalid", "params must be an object")
		return
	}
	taskID, _ := params["id"].(string)
	if op.push != "" {
		// A2A 1.0 names the task taskId and the configuration id; 0.3
		// names the task id, except when setting a configuration.
		c.pushConfig, _ = params["pushNotificationConfigId"].(string)
		if t, _ := params["taskId"].(string); t != "" {
			c.pushConfig, taskID = taskID, t
		}
	}
	changed, ok := c.check(w, r.Context(), op, taskID, params)
	if !ok {
		return
	}
	if changed {
		raw, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "method": req.Method, "params": params})
	}
	out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, agent.URL, bytes.NewReader(raw))
	if err != nil {
		c.rpcError(w, rpcInternalError, "error", "the agent's URL is not valid")
		return
	}
	out.Header.Set("Content-Type", "application/json")
	c.forward(w, r, out, op.streams)
}

// listTasksRefused says why tasks cannot be listed through the gateway.
const listTasksRefused = "listing tasks is not available through the gateway: the agent knows every caller as the gateway, so its list would show other teams' tasks"

// serveA2AREST relays a request of the HTTP+JSON binding, with the same
// checks as serveA2A. The path after the agent's name is the binding's:
// that of A2A 1.0, or with /v1 in front that of A2A 0.3.
func (s *Server) serveA2AREST(w http.ResponseWriter, r *http.Request) {
	c, r, done, ok := s.beginA2A(w, r, true)
	if !ok {
		return
	}
	defer done()
	if c.agent.RESTURL == "" {
		c.rpcError(w, a2aUnsupported, "invalid", "this agent is not offered over HTTP+JSON; use the JSON-RPC binding")
		return
	}
	path := r.PathValue("path")
	rest := strings.TrimPrefix(path, "v1/")
	var op a2aOp
	var taskID string
	known := false
	switch {
	case rest == "message:send" && r.Method == http.MethodPost:
		op, known = opSend, true
	case rest == "message:stream" && r.Method == http.MethodPost:
		op, known = opStream, true
	case rest == "tasks" && r.Method == http.MethodGet:
		c.rpcError(w, a2aUnsupported, "invalid", listTasksRefused)
		return
	case strings.Contains(rest, "/pushNotificationConfigs"):
		// tasks/{id}/pushNotificationConfigs[/{config}]; A2A 0.3 spells
		// the first segment task when creating.
		seg := strings.Split(rest, "/")
		if len(seg) < 3 || len(seg) > 4 || (seg[0] != "tasks" && seg[0] != "task") || seg[2] != "pushNotificationConfigs" {
			break
		}
		taskID = seg[1]
		switch {
		case len(seg) == 3 && r.Method == http.MethodPost:
			op, known = opPushSet, true
		case len(seg) == 3 && r.Method == http.MethodGet:
			op, known = opPushList, true
		case len(seg) == 4 && r.Method == http.MethodGet:
			op, known, c.pushConfig = opPushGet, true, seg[3]
		case len(seg) == 4 && r.Method == http.MethodDelete:
			op, known, c.pushConfig = opPushDelete, true, seg[3]
		}
	case strings.HasPrefix(rest, "tasks/") && !strings.Contains(rest[len("tasks/"):], "/"):
		id, verb, _ := strings.Cut(rest[len("tasks/"):], ":")
		taskID = id
		switch {
		case verb == "" && r.Method == http.MethodGet:
			op, known = opGet, true
		case verb == "cancel" && r.Method == http.MethodPost:
			op, known = opCancel, true
		case verb == "subscribe" && (r.Method == http.MethodPost || r.Method == http.MethodGet):
			op, known = opSubscribe, true
		}
	}
	if !known {
		c.rpcError(w, rpcMethodNotFound, "invalid", r.Method+" /"+path+" is not available through the gateway")
		return
	}
	c.method = op.label
	if op.streams && c.rt.guards.Applies(config.StageOutput, c.agent.Guardrails) {
		c.rpcError(w, a2aUnsupported, "invalid", "streaming is not available for this agent, since its output guardrails check whole results")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, c.rt.cfg.Limits.MaxRequestBytes))
	if err != nil {
		c.rpcError(w, rpcInvalidRequest, "invalid", "the request is too large or could not be read")
		return
	}
	var params map[string]any
	if op.message || op.push == "set" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&params) != nil || params == nil {
			c.rpcError(w, rpcParseError, "invalid", "the request body must be a JSON object")
			return
		}
	}
	changed, ok := c.check(w, r.Context(), op, taskID, params)
	if !ok {
		return
	}
	if changed {
		raw, _ = json.Marshal(params)
	}
	target := c.agent.RESTURL + "/" + (&url.URL{Path: path}).EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(raw))
	if err != nil {
		c.rpcError(w, rpcInternalError, "error", "the agent's URL is not valid")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && len(raw) > 0 {
		out.Header.Set("Content-Type", ct)
	}
	c.forward(w, r, out, op.streams)
}

// check checks the task and context a request names, and runs input
// guardrails on a message, which may change params. It returns false
// after writing an error.
func (c *a2aCall) check(w http.ResponseWriter, ctx context.Context, op a2aOp, taskID string, params map[string]any) (changed, ok bool) {
	c.op = op
	if op.push != "" && (!c.rt.cfg.A2A.Push.Enabled || c.s.store == nil) {
		c.rpcError(w, a2aPushNotSupported, "invalid", "push notifications are not available through the gateway")
		return false, false
	}
	if !op.message {
		if taskID == "" {
			c.rpcError(w, rpcInvalidParams, "invalid", "the request must name a task")
			return false, false
		}
		if !c.owns(w, ctx, store.A2ATask, taskID, true) {
			return false, false
		}
		if op.push == "" {
			return false, true
		}
		c.pushTask = taskID
		if op.push != "set" {
			return false, true
		}
		conf := pushConfigIn(params)
		if conf == nil {
			c.rpcError(w, rpcInvalidParams, "invalid", "the request must carry a push notification configuration with a url")
			return false, false
		}
		return true, c.setPush(w, ctx, conf, taskID)
	}
	msg, _ := params["message"].(map[string]any)
	if msg == nil {
		c.rpcError(w, rpcInvalidParams, "invalid", "params.message is required")
		return false, false
	}
	// A task must have come from this caller. A context may be new,
	// since clients can choose one, but not someone else's.
	if id, _ := msg["taskId"].(string); id != "" && !c.owns(w, ctx, store.A2ATask, id, true) {
		return false, false
	}
	if id, _ := msg["contextId"].(string); id != "" && !c.owns(w, ctx, store.A2AContext, id, false) {
		return false, false
	}
	// A push configuration sent with the message gets the gateway's
	// URL. Its field is named differently in A2A 0.3 over JSON-RPC, 0.3
	// over HTTP+JSON, and 1.0.
	if conf, _ := params["configuration"].(map[string]any); conf != nil {
		if push := pushConfigIn(conf); push != nil {
			taskID, _ := msg["taskId"].(string)
			if !c.setPush(w, ctx, push, taskID) {
				return false, false
			}
			changed = true
		}
	}
	names := c.agent.Guardrails
	if !c.rt.guards.Applies(config.StageInput, names) {
		return changed, true
	}
	parts := textParts(msg)
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p["text"].(string)
	}
	meta := guardMeta(c.rid, c.p)
	res := c.rt.guards.Run(ctx, config.StageInput, names, texts, meta)
	c.s.recordGuardrails(meta, res.Decisions)
	if res.Blocked != nil {
		c.rpcError(w, rpcInvalidParams, "guardrail_blocked", blockedMessage(res.Blocked))
		return false, false
	}
	if !res.Changed {
		return changed, true
	}
	for i, p := range parts {
		p["text"] = res.Texts[i]
	}
	params["message"] = msg
	return true, true
}

// owns checks that a task or context is the caller's. An id the gateway
// never saw passes only when mustExist is false. It writes the error when
// the answer is no.
func (c *a2aCall) owns(w http.ResponseWriter, ctx context.Context, kind, id string, mustExist bool) bool {
	if c.s.store == nil {
		c.rpcError(w, a2aUnsupported, "invalid", "naming a task or context needs a database at the gateway")
		return false
	}
	o, err := c.s.store.GetA2AObject(ctx, c.name, kind, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if !mustExist {
			return true
		}
	case err != nil:
		c.s.log.Error("reading an A2A object failed", "request_id", c.rid, "err", err)
		c.rpcError(w, rpcInternalError, "error", "the gateway could not check the task")
		return false
	case o.OrgID == c.p.OrgID && o.TeamID == c.p.TeamID && (c.p.TeamID != auth.StaticTeamID || o.KeyID == c.p.KeyID):
		return true
	}
	c.rpcError(w, a2aTaskNotFound, "not_found", "task or context "+id+" not found")
	return false
}

// forward sends the request to the agent and relays its answer.
func (c *a2aCall) forward(w http.ResponseWriter, r *http.Request, req *http.Request, streams bool) {
	timeout := c.agent.Timeout.Std()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// A stream may run for as long as the agent works; the timeout only
	// bounds the wait for it to start.
	timer := time.AfterFunc(timeout, cancel)
	defer timer.Stop()
	req = req.WithContext(ctx)
	if streams {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	// The protocol version and extensions the caller asked for are the
	// agent's to honor.
	for _, h := range []string{"A2A-Version", "A2A-Extensions"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	for k, v := range c.agent.Headers {
		req.Header.Set(k, v.Value())
	}
	resp, err := c.s.client.Do(req)
	if err != nil {
		c.s.log.Warn("a2a agent failed", "request_id", c.rid, "agent", c.name, "err", err)
		c.rpcError(w, rpcInternalError, "upstream_error", "the agent could not be reached")
		return
	}
	defer resp.Body.Close()
	if streams {
		timer.Stop()
	}
	if resp.StatusCode != http.StatusOK {
		c.s.log.Warn("a2a agent refused", "request_id", c.rid, "agent", c.name, "status", resp.StatusCode)
		// Over HTTP+JSON an error is an HTTP status with a body that says
		// why. One about the gateway's own credentials is not the caller's.
		if c.rest && resp.StatusCode >= 400 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			c.status = "agent_error"
			if ct := resp.Header.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(raw)
			return
		}
		c.rpcError(w, rpcInternalError, "upstream_error", fmt.Sprintf("the agent answered HTTP %d", resp.StatusCode))
		return
	}
	if streams && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		c.relayStream(w, ctx, resp.Body)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxA2AResponse+1))
	if err != nil || len(raw) > maxA2AResponse {
		c.rpcError(w, a2aInvalidResponse, "upstream_error", "the agent's answer could not be read")
		return
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&out) != nil || out == nil {
		c.rpcError(w, a2aInvalidResponse, "upstream_error", "the agent's answer is not a JSON object")
		return
	}
	// Over JSON-RPC the result is in an envelope; over HTTP+JSON it is
	// the body.
	result := out
	if !c.rest {
		result, _ = out["result"].(map[string]any)
	}
	if c.op.push != "" && result != nil {
		c.restorePush(ctx, result, c.pushTask)
		raw, _ = json.Marshal(out)
		if c.op.push == "delete" {
			if err := c.s.store.DeleteA2APush(ctx, c.name, c.pushTask, c.pushConfig); err != nil {
				c.s.log.Error("deleting an A2A push configuration failed", "request_id", c.rid, "err", err)
			}
		}
	}
	if result != nil {
		c.record(ctx, result)
		names := c.agent.Guardrails
		if c.rt.guards.Applies(config.StageOutput, names) {
			parts := textParts(result)
			texts := make([]string, len(parts))
			for i, p := range parts {
				texts[i] = p["text"].(string)
			}
			meta := guardMeta(c.rid, c.p)
			res := c.rt.guards.Run(ctx, config.StageOutput, names, texts, meta)
			c.s.recordGuardrails(meta, res.Decisions)
			if res.Blocked != nil {
				c.rpcError(w, a2aInvalidResponse, "guardrail_blocked", blockedMessage(res.Blocked))
				return
			}
			if res.Changed {
				for i, p := range parts {
					p["text"] = res.Texts[i]
				}
				raw, _ = json.Marshal(out)
			}
		}
	} else if out["error"] != nil {
		c.status = "agent_error"
	}
	ct := "application/json"
	if c.rest {
		ct = "application/a2a+json"
	}
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(raw)
}

// relayStream passes an agent's event stream through, flushing each
// read, and records the ids its events carry.
func (c *a2aCall) relayStream(w http.ResponseWriter, ctx context.Context, body io.ReadCloser) {
	tap := sse.Tap(body, func(data []byte) {
		var ev map[string]any
		if json.Unmarshal(data, &ev) != nil || ev == nil {
			return
		}
		if !c.rest {
			ev, _ = ev["result"].(map[string]any)
		}
		if ev != nil {
			c.record(ctx, ev)
		}
	})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := tap.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				c.status = "upstream_error"
			}
			return
		}
	}
}

// record keeps the owner of each task and context a result names: a
// task, a message, or a task's status or artifact update.
func (c *a2aCall) record(ctx context.Context, result map[string]any) {
	if c.s.store == nil {
		return
	}
	add := func(kind, id string) {
		if id == "" || c.seen[kind+"/"+id] {
			return
		}
		c.seen[kind+"/"+id] = true
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := c.s.store.RecordA2AObject(wctx, &store.A2AObject{Agent: c.name, Kind: kind, ID: id, OrgID: c.p.OrgID, TeamID: c.p.TeamID, KeyID: c.p.KeyID})
		if err != nil {
			c.s.log.Error("recording an A2A object failed", "request_id", c.rid, "agent", c.name, "err", err)
		}
	}
	// A2A 0.3 over JSON-RPC marks what a result is with kind. The other
	// forms wrap it: {"task": {...}}, {"statusUpdate": {...}}.
	if kind, _ := result["kind"].(string); kind == "task" {
		id, _ := result["id"].(string)
		add(store.A2ATask, id)
	}
	if task, _ := result["task"].(map[string]any); task != nil {
		id, _ := task["id"].(string)
		add(store.A2ATask, id)
	}
	for _, v := range []any{result, result["task"], result["message"], result["statusUpdate"], result["artifactUpdate"]} {
		m, _ := v.(map[string]any)
		if m == nil {
			continue
		}
		id, _ := m["taskId"].(string)
		add(store.A2ATask, id)
		id, _ = m["contextId"].(string)
		add(store.A2AContext, id)
	}
}

// textParts finds the text parts in an A2A object, at any depth: every
// element with text of a parts list, or of a message's content list as
// A2A 0.3 names it over HTTP+JSON. Where parts say their kind (A2A 0.3
// over JSON-RPC), it must be text.
func textParts(v any) []map[string]any {
	var out []map[string]any
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for _, key := range []string{"parts", "content"} {
				parts, _ := v[key].([]any)
				for _, p := range parts {
					m, _ := p.(map[string]any)
					if _, isText := m["text"].(string); !isText {
						continue
					}
					kind := m["kind"]
					if kind == nil {
						kind = m["type"]
					}
					if kind == nil || kind == "text" {
						out = append(out, m)
					}
				}
			}
			for k, x := range v {
				if _, list := x.([]any); list && (k == "parts" || k == "content") {
					continue
				}
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(v)
	return out
}

// a2aCard serves an agent's card as seen through the gateway: the
// gateway's URL, JSON-RPC only, and a gateway key as the only way to
// authenticate (ADR 0033).
func (s *Server) a2aCard(w http.ResponseWriter, r *http.Request) {
	rt := s.rt.Load()
	name := r.PathValue("name")
	p, ok := s.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="genaiopengateway"`)
		writeError(w, provider.ChatCompletions, http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
		return
	}
	agent, ok := rt.cfg.A2A.Agents[name]
	if !ok {
		writeError(w, provider.ChatCompletions, http.StatusNotFound, codeAgentNotFound, "agent "+name+" is not configured")
		return
	}
	if !p.AllowsAgent(name) {
		writeError(w, provider.ChatCompletions, http.StatusForbidden, codeAgentNotAllowed, "you may not use agent "+name)
		return
	}
	raw, err := s.agentCard(r.Context(), name, agent)
	if err != nil {
		s.log.Warn("a2a agent card failed", "agent", name, "err", err)
		writeError(w, provider.ChatCompletions, http.StatusBadGateway, codeAllTargetsFailed, "the agent's card could not be read")
		return
	}
	var card map[string]any
	if json.Unmarshal(raw, &card) != nil || card == nil {
		writeError(w, provider.ChatCompletions, http.StatusBadGateway, codeAllTargetsFailed, "the agent's card is not a JSON object")
		return
	}
	base := rt.cfg.A2A.PublicURL
	if base == "" {
		base = rt.cfg.MCP.PublicURL
	}
	if base == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	self := base + "/a2a/" + name
	// A signature covers the card as the agent wrote it.
	delete(card, "signatures")
	caps, _ := card["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
	}
	// Push notifications go through the gateway, when it is set to
	// deliver them and the agent sends them.
	caps["pushNotifications"] = caps["pushNotifications"] == true && rt.cfg.A2A.Push.Enabled && s.store != nil
	if rt.guards.Applies(config.StageOutput, agent.Guardrails) {
		caps["streaming"] = false
	}
	card["capabilities"] = caps
	if theirs, ok := card["supportedInterfaces"].([]any); ok {
		// A card of A2A 1.0: the gateway offers the bindings it relays,
		// each at the protocol version the agent offers it.
		version := func(binding string) string {
			for _, v := range theirs {
				m, _ := v.(map[string]any)
				if pv, _ := m["protocolVersion"].(string); m["protocolBinding"] == binding && pv != "" {
					return pv
				}
			}
			return "1.0"
		}
		var ours []any
		if agent.URL != "" {
			ours = append(ours, map[string]any{"url": self, "protocolBinding": "JSONRPC", "protocolVersion": version("JSONRPC")})
		}
		if agent.RESTURL != "" {
			ours = append(ours, map[string]any{"url": self, "protocolBinding": "HTTP+JSON", "protocolVersion": version("HTTP+JSON")})
		}
		card["supportedInterfaces"] = ours
		for _, k := range []string{"url", "preferredTransport", "additionalInterfaces", "security", "supportsAuthenticatedExtendedCard"} {
			delete(card, k)
		}
		card["securitySchemes"] = map[string]any{"gateway": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer", "description": "A gateway API key"}}}
		card["securityRequirements"] = []any{map[string]any{"schemes": map[string]any{"gateway": map[string]any{"list": []any{}}}}}
		caps["extendedAgentCard"] = false
	} else {
		// A card of A2A 0.3.
		card["url"] = self
		delete(card, "additionalInterfaces")
		if agent.URL != "" {
			card["preferredTransport"] = "JSONRPC"
			if agent.RESTURL != "" {
				card["additionalInterfaces"] = []any{map[string]any{"url": self, "transport": "HTTP+JSON"}}
			}
		} else {
			card["preferredTransport"] = "HTTP+JSON"
		}
		card["securitySchemes"] = map[string]any{"gateway": map[string]any{"type": "http", "scheme": "bearer", "description": "A gateway API key"}}
		card["security"] = []any{map[string]any{"gateway": []any{}}}
		card["supportsAuthenticatedExtendedCard"] = false
	}
	if agent.Description != "" {
		card["description"] = agent.Description
	}
	writeJSON(w, http.StatusOK, card)
}

// cachedCard is an agent card as last fetched.
type cachedCard struct {
	url string
	raw []byte
	at  time.Time
}

// agentCard fetches an agent's card, or reuses one fetched recently.
func (s *Server) agentCard(ctx context.Context, name string, agent config.A2AAgent) ([]byte, error) {
	cardURL := agent.CardURL
	if cardURL == "" {
		at := agent.URL
		if at == "" {
			at = agent.RESTURL
		}
		u, err := url.Parse(at)
		if err != nil {
			return nil, err
		}
		cardURL = u.Scheme + "://" + u.Host + "/.well-known/agent-card.json"
	}
	if v, ok := s.a2aCards.Load(name); ok {
		if c := v.(cachedCard); c.url == cardURL && time.Since(c.at) < a2aCardTTL {
			return c.raw, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range agent.Headers {
		req.Header.Set(k, v.Value())
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	s.a2aCards.Store(name, cachedCard{url: cardURL, raw: raw, at: time.Now()})
	return raw, nil
}
