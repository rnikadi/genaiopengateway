package server

//lint:file-ignore SA1019 Sampling is deprecated by MCP 2026-07-28 but servers may use it for at least a year, and passing it on is opt-in.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// askingUpstream is an MCP server whose tools need something from the
// client: "ask" a name from the user, "think" a completion from the
// client's model. A stateless one asks with input_required results (MCP
// 2026-07-28); the other asks with requests of its own during the call.
func askingUpstream(t *testing.T, stateless bool) *httptest.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "asking", Version: "1"}, nil)
	form := &mcp.ElicitParams{Message: "What is your name?", RequestedSchema: map[string]any{
		"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}},
	}}
	sampling := &mcp.CreateMessageParams{MaxTokens: 10, Messages: []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "2+2?"}}}}
	hello := func(r *mcp.ElicitResult) *mcp.CallToolResult {
		if r.Action != "accept" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "no name given"}}}
		}
		name, _ := r.Content["name"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello " + name}}}
	}
	if stateless {
		s.AddTool(&mcp.Tool{Name: "ask", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if req.Params.RequestState != "asked" {
				return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"name": form}, RequestState: "asked"}, nil
			}
			r, _ := req.Params.InputResponses["name"].(*mcp.ElicitResult)
			if r == nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "no answer"}}}, nil
			}
			return hello(r), nil
		})
		s.AddTool(&mcp.Tool{Name: "think", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if req.Params.RequestState != "asked" {
				return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"sum": sampling}, RequestState: "asked"}, nil
			}
			r, _ := req.Params.InputResponses["sum"].(*mcp.CreateMessageWithToolsResult)
			if r == nil || len(r.Content) == 0 {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "no answer"}}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "model said " + r.Content[0].(*mcp.TextContent).Text}}}, nil
		})
	} else {
		s.AddTool(&mcp.Tool{Name: "ask", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			r, err := req.Session.Elicit(ctx, form)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "cannot ask: " + err.Error()}}}, nil
			}
			return hello(r), nil
		})
		s.AddTool(&mcp.Tool{Name: "think", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			r, err := req.Session.CreateMessage(ctx, sampling)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "cannot think: " + err.Error()}}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "model said " + r.Content.(*mcp.TextContent).Text}}}, nil
		})
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: stateless})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// A server's requests for input reach the client as input_required
// results and its answers reach the server, whichever way the server
// asks (docs/mcp.md#requests-for-input).
func TestMCPRequestsForInput(t *testing.T) {
	modern, classic := askingUpstream(t, true), askingUpstream(t, false)
	cfg, err := config.Parse([]byte(`
keys:
  - {name: dev, key: ` + devKey + `}
  - {name: other, key: ` + mcpSoloKey + `}
mcp:
  servers:
    modern: {url: "` + modern.URL + `", client_requests: [elicitation, sampling]}
    classic: {url: "` + classic.URL + `", client_requests: [elicitation, sampling]}
    modernshut: {url: "` + modern.URL + `"}
    classicshut: {url: "` + classic.URL + `"}
    onlyask: {url: "` + modern.URL + `", client_requests: [elicitation]}
  virtual_servers:
    all:
      servers: [{server: modern}, {server: classic}, {server: modernshut}, {server: classicshut}, {server: onlyask}]
`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s.mcp.Refresh(ctx)
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(func() { gw.Close(); s.Close(context.Background()) })

	var asked, sampled atomic.Int32
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			asked.Add(1)
			if req.Params.Message != "What is your name?" {
				t.Errorf("elicitation message %q", req.Params.Message)
			}
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"name": "Ada"}}, nil
		},
		CreateMessageHandler: func(_ context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			sampled.Add(1)
			return &mcp.CreateMessageResult{Role: "assistant", Model: "m", Content: &mcp.TextContent{Text: "4"}}, nil
		},
	})
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: gw.URL + "/mcp/all", HTTPClient: &http.Client{Transport: bearer{devKey, nil}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	call := func(name string) (string, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return text(t, res.Content), res.IsError
	}
	for _, tc := range []struct{ tool, want string }{
		{"modern_ask", "hello Ada"},
		{"modern_think", "model said 4"},
		{"classic_ask", "hello Ada"},
		{"classic_think", "model said 4"},
		{"onlyask_ask", "hello Ada"},
	} {
		if got, isErr := call(tc.tool); got != tc.want || isErr {
			t.Errorf("%s = %q (error %v), want %q", tc.tool, got, isErr, tc.want)
		}
	}
	if asked.Load() != 3 || sampled.Load() != 2 {
		t.Errorf("the client was asked %d times and sampled %d times", asked.Load(), sampled.Load())
	}

	// Servers that are not set to ask get nothing through.
	for _, tool := range []string{"modernshut_ask", "onlyask_think"} {
		if got, isErr := call(tool); !isErr || !strings.Contains(got, "not set to pass on") {
			t.Errorf("%s = %q (error %v)", tool, got, isErr)
		}
	}
	if got, isErr := call("classicshut_ask"); !isErr || !strings.Contains(got, "cannot ask") {
		t.Errorf("classicshut_ask = %q (error %v)", got, isErr)
	}
	if asked.Load() != 3 || sampled.Load() != 2 {
		t.Errorf("a request was passed on that should not be: asked %d, sampled %d", asked.Load(), sampled.Load())
	}

	// A client before 2026-07-28 gets an error it can show.
	rpc := func(key, version, body string) string {
		req, _ := http.NewRequest("POST", gw.URL+"/mcp/all", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", version)
		if version >= "2026-07-28" {
			req.Header.Set("Mcp-Method", "tools/call")
			req.Header.Set("Mcp-Name", gjsonName(body))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return string(raw)
	}
	for _, tool := range []string{"classic_ask", "modern_ask"} {
		out := rpc(devKey, "2025-06-18", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`)
		if !strings.Contains(out, `"isError":true`) || !strings.Contains(out, "2026-07-28 or later") {
			t.Errorf("%s for an old client: %s", tool, out)
		}
	}

	// A held call is answered only by the caller that started it.
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"t","version":"1"},"io.modelcontextprotocol/clientCapabilities":{"elicitation":{"form":{}}}}`
	first := rpc(devKey, "2026-07-28", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"classic_ask","arguments":{},`+meta+`}}`)
	i := strings.Index(first, "gog-held:")
	if i < 0 || !strings.Contains(first, "input_required") {
		t.Fatalf("no held call: %s", first)
	}
	state := first[i : i+strings.IndexByte(first[i:], '"')]
	answer := func(key string) string {
		return rpc(key, "2026-07-28", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"classic_ask","arguments":{},"requestState":"`+state+`","inputResponses":{"input":{"action":"accept","content":{"name":"Eve"}}},`+meta+`}}`)
	}
	if out := answer(mcpSoloKey); !strings.Contains(out, "no longer waiting") {
		t.Errorf("another caller answered a held call: %s", out)
	}
	if out := answer(devKey); !strings.Contains(out, "hello Eve") {
		t.Errorf("the caller's answer: %s", out)
	}
	if out := answer(devKey); !strings.Contains(out, "no longer waiting") {
		t.Errorf("a held call was answered twice: %s", out)
	}
}

// gjsonName reads params.name from a tools/call body.
func gjsonName(body string) string {
	const k = `"name":"`
	i := strings.Index(body, k)
	rest := body[i+len(k):]
	return rest[:strings.IndexByte(rest, '"')]
}

// A client that listens is told when its virtual server's tools change
// (docs/mcp.md#change-notifications).
func TestMCPListChanged(t *testing.T) {
	up := newMCPUpstream(t, "alpha", true)
	cfg, err := config.Parse([]byte(`
keys: [{name: dev, key: ` + devKey + `}]
mcp:
  servers: {alpha: {url: "` + up.URL + `"}}
  virtual_servers: {one: {servers: [{server: alpha}]}}
`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s.mcp.Refresh(ctx)
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(func() { gw.Close(); s.Close(context.Background()) })

	tools, prompts := make(chan struct{}, 8), make(chan struct{}, 8)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler:   func(context.Context, *mcp.ToolListChangedRequest) { tools <- struct{}{} },
		PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) { prompts <- struct{}{} },
	})
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL + "/mcp/one", HTTPClient: &http.Client{Transport: bearer{devKey, nil}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	told := func(ch chan struct{}, within time.Duration) bool {
		select {
		case <-ch:
			return true
		case <-time.After(within):
			return false
		}
	}
	// The gateway tells its listeners twice in a row: the stream stays
	// with the server it was opened on, which has been replaced by then.
	for _, name := range []string{"added", "more"} {
		s.mcp.Refresh(ctx)
		if told(tools, 300*time.Millisecond) {
			t.Fatal("told of a change when nothing changed")
		}
		mcp.AddTool(up.srv, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, echoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
		s.mcp.Refresh(ctx)
		if !told(tools, 5*time.Second) {
			t.Fatalf("not told that tool %s was added", name)
		}
		list, err := cs.ListTools(ctx, nil)
		if err != nil || !strings.Contains(mustJSON(list.Tools), "alpha_"+name) {
			t.Fatalf("listing after the notification: %v %v", list, err)
		}
	}
	if told(prompts, 100*time.Millisecond) {
		t.Fatal("told that prompts changed when only tools did")
	}
}
