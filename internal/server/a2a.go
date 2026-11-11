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

// a2aMethods are the methods relayed to agents (ADR 0033); whether each
// streams its answer.
var a2aMethods = map[string]bool{
	"message/send":      false,
	"message/stream":    true,
	"tasks/get":         false,
	"tasks/cancel":      false,
	"tasks/resubscribe": true,
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
}

// rpcError writes a JSON-RPC error. JSON-RPC errors travel with HTTP
// 200, as A2A clients expect.
func (c *a2aCall) rpcError(w http.ResponseWriter, code int, status, msg string) {
	c.status = status
	id := c.id
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

// serveA2A relays a JSON-RPC request to an A2A agent: the caller must
// be allowed the agent, a task or context it names must be its own,
// and guardrails check the message (docs/a2a.md).
func (s *Server) serveA2A(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rt := s.rt.Load()
	c := &a2aCall{s: s, rt: rt, name: r.PathValue("name"), rid: r.Header.Get("X-Request-Id"), method: "other", status: "ok", seen: map[string]bool{}}
	p, ok := s.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="genaiopengateway"`)
		writeError(w, provider.ChatCompletions, http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
		return
	}
	agent, ok := rt.cfg.A2A.Agents[c.name]
	if !ok {
		writeError(w, provider.ChatCompletions, http.StatusNotFound, codeAgentNotFound, "agent "+c.name+" is not configured")
		return
	}
	c.p, c.agent = p, agent
	defer func() {
		d := time.Since(start)
		s.metrics.ObserveA2ARequest(c.name, c.method, c.status, d)
		s.log.Info("a2a request", "request_id", c.rid, "agent", c.name, "method", c.method, "status", c.status,
			"org_id", p.OrgID, "team_id", p.TeamID, "key_id", p.KeyID, "duration_ms", d.Milliseconds())
	}()
	if !p.AllowsAgent(c.name) {
		c.status = "denied"
		writeError(w, provider.ChatCompletions, http.StatusForbidden, codeAgentNotAllowed, "you may not use agent "+c.name)
		return
	}
	r = r.WithContext(s.callerContext(r.Context(), p))
	if status, code, msg, ok := s.admit(w, r, p, c.rid); !ok {
		c.status = "rate_limited"
		writeError(w, provider.ChatCompletions, status, code, msg)
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
	streams, known := a2aMethods[req.Method]
	switch {
	case req.JSONRPC != "2.0" || req.Method == "":
		c.rpcError(w, rpcInvalidRequest, "invalid", "not a JSON-RPC 2.0 request")
		return
	case strings.HasPrefix(req.Method, "tasks/pushNotificationConfig/"):
		c.rpcError(w, a2aPushNotSupported, "invalid", "push notifications are not available through the gateway")
		return
	case !known:
		c.rpcError(w, rpcMethodNotFound, "invalid", "method "+req.Method+" is not available through the gateway")
		return
	}
	c.method = req.Method
	if streams && rt.guards.Applies(config.StageOutput, agent.Guardrails) {
		c.rpcError(w, a2aUnsupported, "invalid", "streaming is not available for this agent, since its output guardrails check whole results")
		return
	}
	body, ok := c.checkParams(w, r.Context(), raw, req)
	if !ok {
		return
	}
	c.forward(w, r, body, streams)
}

// checkParams checks the task and context a request names, and runs
// input guardrails on a message. It returns the request to send, or
// false after writing an error.
func (c *a2aCall) checkParams(w http.ResponseWriter, ctx context.Context, raw []byte, req rpcRequest) ([]byte, bool) {
	var params map[string]any
	dec := json.NewDecoder(bytes.NewReader(req.Params))
	dec.UseNumber()
	if len(req.Params) == 0 || dec.Decode(&params) != nil || params == nil {
		c.rpcError(w, rpcInvalidParams, "invalid", "params must be an object")
		return nil, false
	}
	if !strings.HasPrefix(req.Method, "message/") {
		id, _ := params["id"].(string)
		if id == "" {
			c.rpcError(w, rpcInvalidParams, "invalid", "params.id must name a task")
			return nil, false
		}
		return raw, c.owns(w, ctx, store.A2ATask, id, true)
	}
	msg, _ := params["message"].(map[string]any)
	if msg == nil {
		c.rpcError(w, rpcInvalidParams, "invalid", "params.message is required")
		return nil, false
	}
	if conf, _ := params["configuration"].(map[string]any); conf != nil && conf["pushNotificationConfig"] != nil {
		c.rpcError(w, a2aPushNotSupported, "invalid", "push notifications are not available through the gateway")
		return nil, false
	}
	// A task must have come from this caller. A context may be new,
	// since clients can choose one, but not someone else's.
	if id, _ := msg["taskId"].(string); id != "" && !c.owns(w, ctx, store.A2ATask, id, true) {
		return nil, false
	}
	if id, _ := msg["contextId"].(string); id != "" && !c.owns(w, ctx, store.A2AContext, id, false) {
		return nil, false
	}
	names := c.agent.Guardrails
	if !c.rt.guards.Applies(config.StageInput, names) {
		return raw, true
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
		return nil, false
	}
	if !res.Changed {
		return raw, true
	}
	for i, p := range parts {
		p["text"] = res.Texts[i]
	}
	params["message"] = msg
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "method": req.Method, "params": params})
	return out, true
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
func (c *a2aCall) forward(w http.ResponseWriter, r *http.Request, body []byte, streams bool) {
	timeout := c.agent.Timeout.Std()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// A stream may run for as long as the agent works; the timeout only
	// bounds the wait for it to start.
	timer := time.AfterFunc(timeout, cancel)
	defer timer.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.agent.URL, bytes.NewReader(body))
	if err != nil {
		c.rpcError(w, rpcInternalError, "error", "the agent's URL is not valid")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if streams {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
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
	if dec.Decode(&out) != nil {
		c.rpcError(w, a2aInvalidResponse, "upstream_error", "the agent's answer is not JSON-RPC")
		return
	}
	if result, ok := out["result"].(map[string]any); ok {
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
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// relayStream passes an agent's event stream through, flushing each
// read, and records the ids its events carry.
func (c *a2aCall) relayStream(w http.ResponseWriter, ctx context.Context, body io.ReadCloser) {
	tap := sse.Tap(body, func(data []byte) {
		var ev struct {
			Result map[string]any `json:"result"`
		}
		if json.Unmarshal(data, &ev) == nil && ev.Result != nil {
			c.record(ctx, ev.Result)
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
	if kind, _ := result["kind"].(string); kind == "task" {
		id, _ := result["id"].(string)
		add(store.A2ATask, id)
	}
	id, _ := result["taskId"].(string)
	add(store.A2ATask, id)
	id, _ = result["contextId"].(string)
	add(store.A2AContext, id)
}

// textParts finds the text parts in an A2A object: every element of a
// parts list whose kind is text, at any depth.
func textParts(v any) []map[string]any {
	var out []map[string]any
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if parts, ok := v["parts"].([]any); ok {
				for _, p := range parts {
					if m, ok := p.(map[string]any); ok {
						if _, isText := m["text"].(string); isText && (m["kind"] == "text" || m["type"] == "text") {
							out = append(out, m)
						}
					}
				}
			}
			for k, x := range v {
				if k != "parts" {
					walk(x)
				}
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
	card["url"] = base + "/a2a/" + name
	card["preferredTransport"] = "JSONRPC"
	delete(card, "additionalInterfaces")
	// A signature covers the card as the agent wrote it.
	delete(card, "signatures")
	card["securitySchemes"] = map[string]any{"gateway": map[string]any{"type": "http", "scheme": "bearer", "description": "A gateway API key"}}
	card["security"] = []any{map[string]any{"gateway": []any{}}}
	card["supportsAuthenticatedExtendedCard"] = false
	caps, _ := card["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
	}
	caps["pushNotifications"] = false
	if rt.guards.Applies(config.StageOutput, agent.Guardrails) {
		caps["streaming"] = false
	}
	card["capabilities"] = caps
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
		u, err := url.Parse(agent.URL)
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
