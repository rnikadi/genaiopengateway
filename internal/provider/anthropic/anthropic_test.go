package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// fakeUpstream records the last request and answers with status and body.
type fakeUpstream struct {
	status  int
	body    string
	headers http.Header
	got     map[string]any
	header  http.Header
	path    string
}

func (f *fakeUpstream) start(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.path, f.header = r.URL.Path, r.Header.Clone()
		f.got = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&f.got)
		for k, v := range f.headers {
			w.Header()[k] = v
		}
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	return New(config.Provider{BaseURL: srv.URL, APIKey: "ant-key", Timeout: config.Duration(5 * time.Second)}, srv.Client())
}

func send(t *testing.T, p *Provider, ep provider.Endpoint, stream bool, body string, hdr http.Header) (*provider.Response, string) {
	t.Helper()
	resp, err := p.Send(context.Background(), &provider.Request{Endpoint: ep, Stream: stream, Body: []byte(body), Header: hdr})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(out)
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const textResponse = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
 "content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn",
 "usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}`

func TestChatNonStreaming(t *testing.T) {
	f := &fakeUpstream{status: 200, body: textResponse}
	p := f.start(t)
	resp, body := send(t, p, provider.ChatCompletions, false, `{"model":"claude-x","messages":[
		{"role":"system","content":"Be brief."},
		{"role":"developer","content":[{"type":"text","text":"Use English."}]},
		{"role":"user","content":"Hi"}],
		"max_completion_tokens":100,"temperature":0.2,"stop":"END","user":"u1"}`, nil)

	if f.path != "/v1/messages" || f.header.Get("X-Api-Key") != "ant-key" || f.header.Get("Anthropic-Version") != DefaultVersion {
		t.Fatalf("upstream request %s %v", f.path, f.header)
	}
	want := `{"max_tokens":100,"messages":[{"content":[{"text":"Hi","type":"text"}],"role":"user"}],"metadata":{"user_id":"u1"},"model":"claude-x","stop_sequences":["END"],"system":"Be brief.\n\nUse English.","temperature":0.2}`
	if got := asJSON(t, f.got); got != want {
		t.Fatalf("upstream body\n got %s\nwant %s", got, want)
	}

	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string  `json:"role"`
				Content *string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	c := out.Choices[0]
	if out.Object != "chat.completion" || *c.Message.Content != "Hello" || c.FinishReason != "stop" ||
		out.Usage.PromptTokens != 16 || out.Usage.CompletionTokens != 3 || out.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Fatalf("response = %s", body)
	}
	if *resp.Usage != (usage.Tokens{InputTokens: 16, OutputTokens: 3, CachedInputTokens: 4, Reported: true}) {
		t.Fatalf("metered usage = %+v", *resp.Usage)
	}
}

func TestChatDefaultsMaxTokens(t *testing.T) {
	f := &fakeUpstream{status: 200, body: textResponse}
	send(t, f.start(t), provider.ChatCompletions, false, `{"model":"m","messages":[{"role":"user","content":"x"}]}`, nil)
	if f.got["max_tokens"] != float64(defaultMaxTokens) {
		t.Fatalf("max_tokens = %v", f.got["max_tokens"])
	}
}

func TestChatToolRoundTrip(t *testing.T) {
	f := &fakeUpstream{status: 200, body: `{"id":"msg_2","model":"claude-x","stop_reason":"tool_use",
		"content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_2","name":"weather","input":{"city":"Paris"}}],
		"usage":{"input_tokens":5,"output_tokens":7}}`}
	p := f.start(t)
	_, body := send(t, p, provider.ChatCompletions, false, `{"model":"m",
		"tools":[{"type":"function","function":{"name":"weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"tool_choice":"required","parallel_tool_calls":false,
		"messages":[
		  {"role":"user","content":"Weather in Paris and Rome?"},
		  {"role":"assistant","content":null,"tool_calls":[
		     {"id":"toolu_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Rome\"}"}}]},
		  {"role":"tool","tool_call_id":"toolu_1","content":"sunny"},
		  {"role":"user","content":"And Paris?"}]}`, nil)

	wantMsgs := `[{"content":[{"text":"Weather in Paris and Rome?","type":"text"}],"role":"user"},` +
		`{"content":[{"id":"toolu_1","input":{"city":"Rome"},"name":"weather","type":"tool_use"}],"role":"assistant"},` +
		`{"content":[{"content":"sunny","tool_use_id":"toolu_1","type":"tool_result"},{"text":"And Paris?","type":"text"}],"role":"user"}]`
	if got := asJSON(t, f.got["messages"]); got != wantMsgs {
		t.Fatalf("messages\n got %s\nwant %s", got, wantMsgs)
	}
	if got := asJSON(t, f.got["tool_choice"]); got != `{"disable_parallel_tool_use":true,"type":"any"}` {
		t.Fatalf("tool_choice = %s", got)
	}
	if got := asJSON(t, f.got["tools"]); !strings.Contains(got, `"input_schema":{"properties":{"city":{"type":"string"}},"type":"object"}`) {
		t.Fatalf("tools = %s", got)
	}
	want := `"tool_calls":[{"function":{"arguments":"{\"city\":\"Paris\"}","name":"weather"},"id":"toolu_2","type":"function"}]`
	if !strings.Contains(body, want) || !strings.Contains(body, `"finish_reason":"tool_calls"`) || !strings.Contains(body, `"content":"Checking."`) {
		t.Fatalf("response = %s", body)
	}
}

func TestChatImages(t *testing.T) {
	f := &fakeUpstream{status: 200, body: textResponse}
	send(t, f.start(t), provider.ChatCompletions, false, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"What is this?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}},
		{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg"}}]}]}`, nil)
	want := `[{"content":[{"text":"What is this?","type":"text"},` +
		`{"source":{"data":"iVBORw0KGgo=","media_type":"image/png","type":"base64"},"type":"image"},` +
		`{"source":{"type":"url","url":"https://example.com/cat.jpg"},"type":"image"}],"role":"user"}]`
	if got := asJSON(t, f.got["messages"]); got != want {
		t.Fatalf("messages\n got %s\nwant %s", got, want)
	}
}

func TestChatRejectsUnsupported(t *testing.T) {
	cases := map[string]string{
		"n":           `{"model":"m","n":2,"messages":[]}`,
		"logprobs":    `{"model":"m","logprobs":true,"messages":[]}`,
		"json_schema": `{"model":"m","response_format":{"type":"json_schema"},"messages":[]}`,
		"bad args":    `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{oops"}}]}]}`,
		"bad image":   `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"file:///etc/passwd"}}]}]}`,
		"bad role":    `{"model":"m","messages":[{"role":"wizard","content":"x"}]}`,
		"bad choice":  `{"model":"m","tool_choice":"sometimes","messages":[]}`,
	}
	for name, body := range cases {
		f := &fakeUpstream{status: 200, body: textResponse}
		resp, out := send(t, f.start(t), provider.ChatCompletions, false, body, nil)
		if resp.StatusCode != 400 || !strings.Contains(out, `"invalid_request_error"`) || f.path != "" {
			t.Errorf("%s: status %d body %s upstream %q", name, resp.StatusCode, out, f.path)
		}
	}
}

func TestChatErrorMapping(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500, 529} {
		f := &fakeUpstream{status: status, headers: http.Header{"Retry-After": {"7"}},
			body: `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`}
		resp, body := send(t, f.start(t), provider.ChatCompletions, false, `{"model":"m","messages":[{"role":"user","content":"x"}]}`, nil)
		if resp.StatusCode != status || body != `{"error":{"code":"overloaded_error","message":"busy","type":"overloaded_error"}}` || resp.Header.Get("Retry-After") != "7" {
			t.Errorf("status %d: got %d %s %v", status, resp.StatusCode, body, resp.Header)
		}
	}
}

const sseStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_3","model":"claude-x","usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":2}}}` + "\n\n" +
	"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: ping\n" + `data: {"type":"ping"}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}` + "\n\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"f","input":{}}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}` + "\n\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}` + "\n\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestChatStreaming(t *testing.T) {
	f := &fakeUpstream{status: 200, body: sseStream, headers: http.Header{"Content-Type": {"text/event-stream"}}}
	resp, body := send(t, f.start(t), provider.ChatCompletions, true,
		`{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x"}]}`, nil)
	if f.got["stream"] != true || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream not requested or wrong content type: %v %v", f.got["stream"], resp.Header)
	}
	var deltas []map[string]any
	var usageChunk map[string]any
	lines := strings.Split(strings.TrimSpace(body), "\n\n")
	if lines[len(lines)-1] != "data: [DONE]" {
		t.Fatalf("stream does not end with [DONE]: %q", body)
	}
	for _, l := range lines[:len(lines)-1] {
		var c map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &c); err != nil {
			t.Fatalf("bad chunk %q", l)
		}
		if c["object"] != "chat.completion.chunk" || c["id"] != "msg_3" {
			t.Fatalf("chunk = %v", c)
		}
		if u, ok := c["usage"]; ok {
			usageChunk = u.(map[string]any)
			continue
		}
		deltas = append(deltas, c["choices"].([]any)[0].(map[string]any))
	}
	text, args := "", ""
	for _, d := range deltas {
		delta := d["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			text += s
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			fn := tcs[0].(map[string]any)["function"].(map[string]any)
			args += fn["arguments"].(string)
		}
	}
	last := deltas[len(deltas)-1]
	if text != "Hello" || args != `{"a":1}` || last["finish_reason"] != "tool_calls" {
		t.Fatalf("text %q args %q finish %v", text, args, last["finish_reason"])
	}
	if usageChunk["prompt_tokens"] != 14.0 || usageChunk["completion_tokens"] != 9.0 {
		t.Fatalf("usage chunk = %v", usageChunk)
	}
	if *resp.Usage != (usage.Tokens{InputTokens: 14, OutputTokens: 9, CachedInputTokens: 2, Reported: true}) {
		t.Fatalf("metered = %+v", *resp.Usage)
	}
}

func TestChatStreamingWithoutUsageChunk(t *testing.T) {
	f := &fakeUpstream{status: 200, body: sseStream}
	resp, body := send(t, f.start(t), provider.ChatCompletions, true, `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`, nil)
	if strings.Contains(body, `"usage"`) {
		t.Fatal("usage chunk sent without include_usage")
	}
	if resp.Usage.OutputTokens != 9 {
		t.Fatal("usage not metered")
	}
}

func TestMessagesPassthrough(t *testing.T) {
	f := &fakeUpstream{status: 200, body: textResponse}
	in := `{"model":"claude-x","max_tokens":5,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"enabled","budget_tokens":1024}}`
	hdr := http.Header{"Anthropic-Version": {"2024-01-01"}, "Anthropic-Beta": {"b1", "b2"}}
	resp, body := send(t, f.start(t), provider.Messages, false, in, hdr)
	if body != textResponse {
		t.Fatalf("body changed: %s", body)
	}
	if f.got["thinking"] == nil || f.header.Get("Anthropic-Version") != "2024-01-01" || len(f.header.Values("Anthropic-Beta")) != 2 {
		t.Fatalf("passthrough lost data: %v %v", f.got, f.header)
	}
	if resp.Usage.InputTokens != 16 || resp.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", *resp.Usage)
	}
}

func TestMessagesStreamingUsage(t *testing.T) {
	f := &fakeUpstream{status: 200, body: sseStream}
	resp, body := send(t, f.start(t), provider.Messages, true, `{"model":"m","stream":true}`, nil)
	if body != sseStream {
		t.Fatal("stream changed")
	}
	if *resp.Usage != (usage.Tokens{InputTokens: 14, OutputTokens: 9, CachedInputTokens: 2, Reported: true}) {
		t.Fatalf("usage = %+v", *resp.Usage)
	}
}

func TestMessagesErrorPassesThrough(t *testing.T) {
	errBody := `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`
	f := &fakeUpstream{status: 429, body: errBody}
	resp, body := send(t, f.start(t), provider.Messages, false, `{"model":"m"}`, nil)
	if resp.StatusCode != 429 || body != errBody {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

func TestClientClosingStreamStopsTranslator(t *testing.T) {
	f := &fakeUpstream{status: 200, body: sseStream}
	p := f.start(t)
	resp, err := p.Send(context.Background(), &provider.Request{Endpoint: provider.ChatCompletions, Stream: true,
		Body: []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	_, _ = resp.Body.Read(buf)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
}
