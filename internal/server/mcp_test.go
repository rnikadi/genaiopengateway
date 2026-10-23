package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

type echoIn struct {
	Text string `json:"text,omitempty"`
}

// mcpUpstream is a fake upstream MCP server. Its tools answer with the
// server name, so a test can tell which upstream served a call.
type mcpUpstream struct {
	*httptest.Server
	auth        atomic.Value // the Authorization header last received
	traceparent atomic.Value // _meta.traceparent of the last tool call
	expire      atomic.Bool  // answer the next request in a session with 404
	srv         *mcp.Server
}

func newMCPUpstream(t *testing.T, name string, stateless bool) *mcpUpstream {
	t.Helper()
	u := &mcpUpstream{}
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1"}, nil)
	u.srv = s
	echo := func(ctx context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		if tp, ok := req.Params.Meta["traceparent"].(string); ok {
			u.traceparent.Store(tp)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + ":" + in.Text}}}, nil, nil
	}
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "Echo text"}, echo)
	mcp.AddTool(s, &mcp.Tool{Name: "secret", Description: "Not for everyone"}, echo)
	mcp.AddTool(s, &mcp.Tool{Name: "fail"}, func(context.Context, *mcp.CallToolRequest, echoIn) (*mcp.CallToolResult, any, error) {
		return nil, nil, errors.New("tool broke")
	})
	mcp.AddTool(s, &mcp.Tool{Name: "slow"}, func(context.Context, *mcp.CallToolRequest, echoIn) (*mcp.CallToolResult, any, error) {
		time.Sleep(time.Second)
		return &mcp.CallToolResult{}, nil, nil
	})
	s.AddPrompt(&mcp.Prompt{Name: "greet", Arguments: []*mcp.PromptArgument{{Name: "who"}}}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "hello " + req.Params.Arguments["who"]}}}}, nil
	})
	read := func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: name + " read " + req.Params.URI}}}, nil
	}
	s.AddResource(&mcp.Resource{Name: "readme", URI: "file:///" + name + "/readme"}, read)
	s.AddResourceTemplate(&mcp.ResourceTemplate{Name: "docs", URITemplate: "file:///" + name + "/docs/{page}"}, read)

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: stateless})
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.auth.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Mcp-Session-Id") != "" && u.expire.CompareAndSwap(true, false) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

const (
	mcpLimitedKey = "limited-key-0123456789"
	mcpSoloKey    = "solo-key-0123456789"
)

// mcpGateway starts a gateway with two upstreams, alpha (stateful, with
// credentials) and beta (stateless), and one that is down. The upstreams
// are listed before it returns.
func mcpGateway(t *testing.T) (*httptest.Server, *mcpUpstream, *mcpUpstream) {
	t.Helper()
	alpha := newMCPUpstream(t, "alpha", false)
	beta := newMCPUpstream(t, "beta", true)
	cfg, err := config.Parse([]byte(`
keys:
  - {name: dev, key: ` + devKey + `}
  - {name: limited, key: ` + mcpLimitedKey + `, rpm: 1}
  - {name: solo, key: ` + mcpSoloKey + `, allowed_mcp_servers: [solo]}
mcp:
  servers:
    alpha: {url: "` + alpha.URL + `", headers: {Authorization: "Bearer alpha-token"}}
    beta: {url: "` + beta.URL + `", timeout: 300ms}
    down: {url: "http://127.0.0.1:1/mcp"}
  virtual_servers:
    eng:
      description: Engineering tools
      servers:
        - {server: alpha, tools: [echo, fail], prompts: false}
        - {server: beta, prefix: b, resources: false}
        - {server: down}
    solo:
      servers: [{server: alpha, prefix: ""}]
`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.mcp.Refresh(ctx)
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(func() { gw.Close(); s.Close(context.Background()) })
	return gw, alpha, beta
}

type bearer struct {
	key    string
	header http.Header
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	for k, v := range b.header {
		r.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(r)
}

func mcpClient(t *testing.T, url, key string, header http.Header) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: bearer{key, header}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func text(t *testing.T, content []mcp.Content) string {
	t.Helper()
	if len(content) != 1 {
		t.Fatalf("got %d content items", len(content))
	}
	tc, ok := content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T", content[0])
	}
	return tc.Text
}

func rpcCode(err error) int64 {
	var e *jsonrpc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func TestMCPVirtualServer(t *testing.T) {
	gw, alpha, _ := mcpGateway(t)
	ctx := context.Background()
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	cs := mcpClient(t, gw.URL+"/mcp/eng", devKey, http.Header{"Traceparent": {parent}})
	if init := cs.InitializeResult(); init.ServerInfo == nil || init.ServerInfo.Name != "genaiopengateway/eng" || init.Instructions != "Engineering tools" {
		t.Fatalf("initialize result %+v", init)
	}

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"alpha_echo", "alpha_fail", "b_echo", "b_fail", "b_secret", "b_slow"}; !slices.Equal(names, want) {
		t.Fatalf("tools %v, want %v", names, want)
	}

	for tool, want := range map[string]string{"alpha_echo": "alpha:hi", "b_echo": "beta:hi"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"text": "hi"}})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if got := text(t, res.Content); got != want || res.IsError {
			t.Fatalf("%s returned %q (isError %v), want %q", tool, got, res.IsError, want)
		}
	}
	if got := alpha.auth.Load(); got != "Bearer alpha-token" {
		t.Errorf("alpha got Authorization %q; the gateway must send only the configured credentials", got)
	}
	if tp, _ := alpha.traceparent.Load().(string); !strings.Contains(tp, "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Errorf("upstream _meta.traceparent %q does not continue the client's trace", tp)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "b_fail", Arguments: map[string]any{}})
	if err != nil || !res.IsError {
		t.Fatalf("a failing tool must return isError, got %v, %v", res, err)
	}
	start := time.Now()
	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "b_slow", Arguments: map[string]any{}})
	if rpcCode(err) != jsonrpc.CodeInternalError || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 2*time.Second {
		t.Fatalf("a slow upstream must time out with -32603, got %v after %v", err, time.Since(start))
	}

	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil || len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "b_greet" {
		t.Fatalf("prompts %+v, %v; alpha's prompts are excluded", prompts, err)
	}
	pr, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "b_greet", Arguments: map[string]string{"who": "ram"}})
	if err != nil || text(t, []mcp.Content{pr.Messages[0].Content}) != "hello ram" {
		t.Fatalf("get prompt: %+v, %v", pr, err)
	}

	resources, err := cs.ListResources(ctx, nil)
	if err != nil || len(resources.Resources) != 1 || resources.Resources[0].URI != "file:///alpha/readme" {
		t.Fatalf("resources %+v, %v; beta's resources are excluded", resources, err)
	}
	for uri, want := range map[string]string{"file:///alpha/readme": "alpha read file:///alpha/readme", "file:///alpha/docs/x": "alpha read file:///alpha/docs/x"} {
		rr, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil || rr.Contents[0].Text != want {
			t.Fatalf("read %s: %+v, %v", uri, rr, err)
		}
	}
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///beta/readme"}); err == nil {
		t.Fatal("a resource of an excluded upstream was readable")
	}

	solo := mcpClient(t, gw.URL+"/mcp/solo", mcpSoloKey, nil)
	tools, err = solo.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 4 || !slices.ContainsFunc(tools.Tools, func(t *mcp.Tool) bool { return t.Name == "secret" }) {
		t.Fatalf("solo tools %+v, %v: an empty prefix exposes upstream names", tools, err)
	}

	body := scrape(t, mustHandler(t, gw))
	for _, want := range []string{
		`gog_mcp_requests_total{method="tools/call",status="ok",virtual_server="eng"} 2`,
		`gog_mcp_requests_total{method="tools/call",status="tool_error",virtual_server="eng"} 1`,
		`gog_mcp_requests_total{method="tools/list",status="ok",virtual_server="eng"} 1`,
		`gog_mcp_tool_calls_total{server="beta",status="tool_error",tool="fail"} 1`,
		`gog_mcp_tool_calls_total{server="beta",status="error",tool="slow"} 1`,
		`gog_mcp_upstream_up{server="alpha"} 1`,
		`gog_mcp_upstream_up{server="down"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

// mustHandler returns a handler that proxies to a running gateway, so
// scrape can read its /metrics.
func mustHandler(t *testing.T, gw *httptest.Server) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(gw.URL + r.URL.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

func TestMCPSessionExpiry(t *testing.T) {
	gw, alpha, _ := mcpGateway(t)
	cs := mcpClient(t, gw.URL+"/mcp/solo", devKey, nil)
	alpha.expire.Store(true)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "again"}})
	if err != nil || text(t, res.Content) != "alpha:again" {
		t.Fatalf("a call on an expired upstream session must reconnect and succeed: %v, %v", res, err)
	}
	if alpha.expire.Load() {
		t.Fatal("the upstream session was never used")
	}
}

func TestMCPHTTPErrors(t *testing.T) {
	gw, _, _ := mcpGateway(t)
	post := func(path string, header http.Header) *http.Response {
		req, _ := http.NewRequest("POST", gw.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for name, tc := range map[string]struct {
		path   string
		header http.Header
		status int
	}{
		"no credentials":       {"/mcp/eng", nil, http.StatusUnauthorized},
		"wrong key":            {"/mcp/eng", http.Header{"Authorization": {"Bearer nope-0123456789"}}, http.StatusUnauthorized},
		"key not as bearer":    {"/mcp/eng", http.Header{"X-Api-Key": {devKey}}, http.StatusUnauthorized},
		"unknown server":       {"/mcp/nope", http.Header{"Authorization": {"Bearer " + devKey}}, http.StatusNotFound},
		"server not allowed":   {"/mcp/eng", http.Header{"Authorization": {"Bearer " + mcpSoloKey}}, http.StatusForbidden},
		"server allowed":       {"/mcp/solo", http.Header{"Authorization": {"Bearer " + mcpSoloKey}}, http.StatusOK},
		"no sessions to fetch": {"/mcp/solo", http.Header{"Authorization": {"Bearer " + mcpSoloKey}}, 0},
	} {
		if tc.status == 0 {
			req, _ := http.NewRequest("GET", gw.URL+tc.path, nil)
			req.Header = tc.header
			req.Header.Set("Accept", "text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s: GET status %d, want 405", name, resp.StatusCode)
			}
			continue
		}
		resp := post(tc.path, tc.header)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, tc.status)
		}
		if tc.status == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: 401 without WWW-Authenticate", name)
		}
	}
}

func TestMCPRateLimit(t *testing.T) {
	gw, _, _ := mcpGateway(t)
	ctx := context.Background()
	cs := mcpClient(t, gw.URL+"/mcp/solo", mcpLimitedKey, nil)
	for range 3 {
		if _, err := cs.ListTools(ctx, nil); err != nil {
			t.Fatalf("listing is not rate limited: %v", err)
		}
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{}})
	var e *jsonrpc.Error
	if !errors.As(err, &e) || e.Code != -32005 {
		t.Fatalf("second call: %v, want JSON-RPC error -32005", err)
	}
	var data struct {
		RetryAfterSeconds int64 `json:"retryAfterSeconds"`
	}
	if json.Unmarshal(e.Data, &data) != nil || data.RetryAfterSeconds < 1 || data.RetryAfterSeconds > 60 {
		t.Fatalf("error data %s lacks retryAfterSeconds", e.Data)
	}
}

// TestMCPLegacyClient speaks the 2025-06-18 protocol by hand, as older
// clients do, with an initialize handshake.
func TestMCPLegacyClient(t *testing.T) {
	gw, _, _ := mcpGateway(t)
	rpc := func(body string, version string) map[string]any {
		req, _ := http.NewRequest("POST", gw.URL+"/mcp/solo", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+devKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if version != "" {
			req.Header.Set("Mcp-Protocol-Version", version)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Mcp-Session-Id") != "" {
			t.Error("the gateway is stateless and must not issue sessions")
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("status %d: %v", resp.StatusCode, err)
		}
		return out
	}
	init := rpc(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"old","version":"1"}}}`, "")
	if v := init["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Fatalf("negotiated %v", v)
	}
	list := rpc(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "2025-06-18")
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("tools/list: %v", list)
	}
	call := rpc(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"old"}}}`, "2025-06-18")
	if !strings.Contains(mustJSON(call), "alpha:old") {
		t.Fatalf("tools/call: %v", call)
	}
}

func TestMCPToolOutsideAllowList(t *testing.T) {
	gw, _, _ := mcpGateway(t)
	req, _ := http.NewRequest("POST", gw.URL+"/mcp/eng", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"alpha_secret","arguments":{}}}`))
	req.Header.Set("Authorization", "Bearer "+devKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Error *jsonrpc.Error `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Error == nil || out.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("a tool outside the allow list must be unknown (-32602), got %+v, %v", out.Error, err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestMCPCatalogChangeAndReload(t *testing.T) {
	up := newMCPUpstream(t, "alpha", true)
	yaml := func(vs string) *config.Config {
		cfg, err := config.Parse([]byte(`
keys: [{name: dev, key: ` + devKey + `}]
mcp:
  servers: {alpha: {url: "` + up.URL + `"}}
  virtual_servers: {` + vs + `: {servers: [{server: alpha}]}}
`))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	s, err := New(yaml("one"), slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.mcp.Refresh(ctx)
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(func() { gw.Close(); s.Close(ctx) })
	hasTool := func(vs, name string) bool {
		tools, err := mcpClient(t, gw.URL+"/mcp/"+vs, devKey, nil).ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(tools.Tools, func(t *mcp.Tool) bool { return t.Name == name })
	}

	mcp.AddTool(up.srv, &mcp.Tool{Name: "added"}, func(context.Context, *mcp.CallToolRequest, echoIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	if hasTool("one", "alpha_added") {
		t.Fatal("a new upstream tool appeared before the catalog was refreshed")
	}
	s.mcp.Refresh(ctx)
	if !hasTool("one", "alpha_added") {
		t.Fatal("a new upstream tool did not appear after a refresh")
	}

	if err := s.Reload(yaml("two")); err != nil {
		t.Fatal(err)
	}
	if rec := do(s.Handler(), "POST", "/mcp/one", devKey, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("a removed virtual server answered %d", rec.Code)
	}
	if !hasTool("two", "alpha_added") {
		t.Fatal("an unchanged upstream lost its catalog on reload")
	}
}
