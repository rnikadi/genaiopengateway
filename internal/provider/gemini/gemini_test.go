package gemini

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
	status int
	body   string
	got    map[string]any
	raw    string
	header http.Header
	url    string
}

func (f *fakeUpstream) start(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.url, f.header = r.URL.RequestURI(), r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		f.raw, f.got = string(b), map[string]any{}
		_ = json.Unmarshal(b, &f.got)
		if strings.Contains(f.url, "alt=sse") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	return New(config.Provider{BaseURL: srv.URL, APIKey: "goog-key", Timeout: config.Duration(5 * time.Second)}, srv.Client())
}

func send(t *testing.T, p *Provider, ep provider.Endpoint, model string, stream bool, body string) (*provider.Response, string) {
	t.Helper()
	resp, err := p.Send(context.Background(), &provider.Request{Endpoint: ep, Model: model, Stream: stream, Body: []byte(body)})
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

const textResponse = `{"candidates":[{"content":{"role":"model","parts":[{"text":"thinking...","thought":true},{"text":"Hello"}]},"finishReason":"STOP"}],
 "usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"thoughtsTokenCount":5,"cachedContentTokenCount":4},
 "modelVersion":"gemini-2.5-pro","responseId":"r1"}`

func TestChatNonStreaming(t *testing.T) {
	f := &fakeUpstream{status: 200, body: textResponse}
	p := f.start(t)
	resp, body := send(t, p, provider.ChatCompletions, "gemini-2.5-pro", false, `{"model":"gemini-2.5-pro","messages":[
		{"role":"system","content":"Be brief."},
		{"role":"developer","content":[{"type":"text","text":"Use English."}]},
		{"role":"user","content":"Hi"}],
		"max_completion_tokens":100,"temperature":0.2,"stop":"END","seed":7,"reasoning_effort":"low",
		"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}}`)

	if f.url != "/v1beta/models/gemini-2.5-pro:generateContent" || f.header.Get("X-Goog-Api-Key") != "goog-key" {
		t.Fatalf("upstream request %s %v", f.url, f.header)
	}
	want := `{"contents":[{"parts":[{"text":"Hi"}],"role":"user"}],"generationConfig":{"maxOutputTokens":100,"responseJsonSchema":{"type":"object"},` +
		`"responseMimeType":"application/json","seed":7,"stopSequences":["END"],"temperature":0.2,"thinkingConfig":{"thinkingLevel":"low"}},` +
		`"systemInstruction":{"parts":[{"text":"Be brief."},{"text":"Use English."}]}}`
	if got := asJSON(t, f.got); got != want {
		t.Errorf("upstream body\n got %s\nwant %s", got, want)
	}
	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "chatcmpl-r1" || out.Object != "chat.completion" || out.Model != "gemini-2.5-pro" ||
		out.Choices[0].Message.Content != "Hello" || out.Choices[0].FinishReason != "stop" {
		t.Errorf("response %s", body)
	}
	if got := asJSON(t, out.Usage); got != `{"completion_tokens":8,"completion_tokens_details":{"reasoning_tokens":5},"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":4},"total_tokens":18}` {
		t.Errorf("usage %s", got)
	}
	if *resp.Usage != (usage.Tokens{InputTokens: 10, OutputTokens: 8, CachedInputTokens: 4, ReasoningTokens: 5, Reported: true}) {
		t.Errorf("metered %+v", *resp.Usage)
	}
}

func TestChatStreaming(t *testing.T) {
	f := &fakeUpstream{status: 200, body: "" +
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"He"}]}}],"usageMetadata":{"promptTokenCount":10},"modelVersion":"gemini-x","responseId":"r2"}` + "\n\n" +
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"llo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"thoughtsTokenCount":1}}` + "\n\n"}
	p := f.start(t)
	resp, body := send(t, p, provider.ChatCompletions, "gemini-x", true, `{"model":"gemini-x","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Hi"}]}`)
	if f.url != "/v1beta/models/gemini-x:streamGenerateContent?alt=sse" {
		t.Fatalf("url %s", f.url)
	}
	var deltas []string
	var finish string
	var lastUsage map[string]any
	for line := range strings.SplitSeq(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatal(err)
		}
		if c.ID != "chatcmpl-r2" {
			t.Errorf("chunk id %q", c.ID)
		}
		for _, ch := range c.Choices {
			deltas = append(deltas, ch.Delta.Content)
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
		if c.Usage != nil {
			lastUsage = c.Usage
		}
	}
	if strings.Join(deltas, "") != "Hello" || finish != "stop" || lastUsage["completion_tokens"] != 3.0 {
		t.Errorf("stream %v %q %v\n%s", deltas, finish, lastUsage, body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("no [DONE]")
	}
	if *resp.Usage != (usage.Tokens{InputTokens: 10, OutputTokens: 3, ReasoningTokens: 1, Reported: true}) {
		t.Errorf("metered %+v", *resp.Usage)
	}
}

// A tool call round trip: tools are declared, the model's call comes
// back with its thought signature, and the client's tool message is sent
// as a function response under the call's function name.
func TestChatTools(t *testing.T) {
	f := &fakeUpstream{status: 200, body: `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"weather","args":{"city":"Paris"}},"thoughtSignature":"sig1"}]},"finishReason":"STOP"}]}`}
	p := f.start(t)
	_, body := send(t, p, provider.ChatCompletions, "g", false, `{"model":"g","messages":[{"role":"user","content":"Weather?"}],
		"tools":[{"type":"function","function":{"name":"weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"weather"}}}`)
	if got := asJSON(t, f.got["tools"]); got != `[{"functionDeclarations":[{"description":"Get weather","name":"weather","parametersJsonSchema":{"properties":{"city":{"type":"string"}},"type":"object"}}]}]` {
		t.Errorf("tools %s", got)
	}
	if got := asJSON(t, f.got["toolConfig"]); got != `{"functionCallingConfig":{"allowedFunctionNames":["weather"],"mode":"ANY"}}` {
		t.Errorf("tool config %s", got)
	}
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []chatToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	calls := out.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "weather" || calls[0].Function.Arguments != `{"city":"Paris"}` ||
		calls[0].ExtraContent == nil || calls[0].ExtraContent.Google.ThoughtSignature != "sig1" || !strings.HasPrefix(calls[0].ID, "call_") ||
		out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("tool call %s", body)
	}

	// The client sends the call and its result back.
	call, _ := json.Marshal(calls[0])
	f.body = textResponse
	send(t, p, provider.ChatCompletions, "g", false, `{"model":"g","messages":[{"role":"user","content":"Weather?"},
		{"role":"assistant","content":null,"tool_calls":[`+string(call)+`]},
		{"role":"tool","tool_call_id":"`+calls[0].ID+`","content":"{\"temp\":21}"},
		{"role":"tool","tool_call_id":"`+calls[0].ID+`","content":"sunny"}]}`)
	want := `[{"parts":[{"text":"Weather?"}],"role":"user"},` +
		`{"parts":[{"functionCall":{"args":{"city":"Paris"},"name":"weather"},"thoughtSignature":"sig1"}],"role":"model"},` +
		`{"parts":[{"functionResponse":{"name":"weather","response":{"temp":21}}},{"functionResponse":{"name":"weather","response":{"result":"sunny"}}}],"role":"user"}]`
	if got := asJSON(t, f.got["contents"]); got != want {
		t.Errorf("contents\n got %s\nwant %s", got, want)
	}

	// A tool message must answer a call in the conversation.
	resp, _ := send(t, p, provider.ChatCompletions, "g", false, `{"model":"g","messages":[{"role":"tool","tool_call_id":"nope","content":"x"}]}`)
	if resp.StatusCode != 400 {
		t.Errorf("unknown tool_call_id: %d", resp.StatusCode)
	}
}

func TestChatStreamingTools(t *testing.T) {
	f := &fakeUpstream{status: 200, body: `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"fc1","name":"a","args":{"x":1}}},{"functionCall":{"name":"b"}}]},"finishReason":"STOP"}]}` + "\n\n"}
	p := f.start(t)
	_, body := send(t, p, provider.ChatCompletions, "g", true, `{"model":"g","stream":true,"messages":[{"role":"user","content":"Go"}]}`)
	for _, want := range []string{`"index":0`, `"id":"fc1"`, `"arguments":"{\"x\":1}"`, `"index":1`, `"arguments":"{}","name":"b"`, `"finish_reason":"tool_calls"`} {
		if !strings.Contains(body, want) {
			t.Errorf("stream lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"usage"`) {
		t.Errorf("usage chunk sent without include_usage")
	}
}

func TestChatImages(t *testing.T) {
	f := &fakeUpstream{status: 200, body: textResponse}
	p := f.start(t)
	send(t, p, provider.ChatCompletions, "g", false, `{"model":"g","messages":[{"role":"user","content":[
		{"type":"text","text":"What is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`)
	if got := asJSON(t, f.got["contents"]); got != `[{"parts":[{"text":"What is this?"},{"inlineData":{"data":"iVBORw0KGgo=","mimeType":"image/png"}}],"role":"user"}]` {
		t.Errorf("contents %s", got)
	}
	// The gateway does not fetch image URLs.
	resp, body := send(t, p, provider.ChatCompletions, "g", false, `{"model":"g","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "data URLs") {
		t.Errorf("image url: %d %s", resp.StatusCode, body)
	}
}

func TestChatRejectsUnsupported(t *testing.T) {
	f := &fakeUpstream{status: 200, body: textResponse}
	p := f.start(t)
	for _, body := range []string{
		`{"model":"g","n":2,"messages":[]}`,
		`{"model":"g","logprobs":true,"messages":[]}`,
		`{"model":"g","reasoning_effort":"max","messages":[]}`,
		`{"model":"g","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`,
	} {
		f.url = ""
		resp, out := send(t, p, provider.ChatCompletions, "g", false, body)
		if resp.StatusCode != 400 || f.url != "" || !strings.Contains(out, "invalid_request_error") {
			t.Errorf("%s: %d %s (upstream %q)", body, resp.StatusCode, out, f.url)
		}
	}
}

func TestChatErrors(t *testing.T) {
	for _, tc := range []struct {
		status   int
		body     string
		wantType string
	}{
		{400, `{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`, "invalid_argument"},
		{401, `{"error":{"code":401,"message":"no key","status":"UNAUTHENTICATED"}}`, "unauthenticated"},
		{429, `{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`, "resource_exhausted"},
		{500, `oops`, "api_error"},
	} {
		f := &fakeUpstream{status: tc.status, body: tc.body}
		p := f.start(t)
		resp, body := send(t, p, provider.ChatCompletions, "g", false, `{"model":"g","messages":[{"role":"user","content":"Hi"}]}`)
		var e struct {
			Error struct{ Type, Message string } `json:"error"`
		}
		_ = json.Unmarshal([]byte(body), &e)
		if resp.StatusCode != tc.status || e.Error.Type != tc.wantType || e.Error.Message == "" {
			t.Errorf("%d: got %d %s", tc.status, resp.StatusCode, body)
		}
	}
}

func TestChatPromptBlocked(t *testing.T) {
	f := &fakeUpstream{status: 200, body: `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":4}}`}
	p := f.start(t)
	resp, body := send(t, p, provider.ChatCompletions, "g", false, `{"model":"g","messages":[{"role":"user","content":"Hi"}]}`)
	if resp.StatusCode != 400 || !strings.Contains(body, `"content_filter"`) || resp.Usage.InputTokens != 4 {
		t.Errorf("blocked prompt: %d %s %+v", resp.StatusCode, body, *resp.Usage)
	}
	f.body = `{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`
	_, body = send(t, p, provider.ChatCompletions, "g", false, `{"model":"g","messages":[{"role":"user","content":"Hi"}]}`)
	if !strings.Contains(body, `"finish_reason":"content_filter"`) {
		t.Errorf("safety finish: %s", body)
	}
}

// generateContent is passed through with the model and stream moved to
// the URL, and usage read on the way.
func TestPassthrough(t *testing.T) {
	f := &fakeUpstream{status: 200, body: textResponse}
	p := f.start(t)
	resp, body := send(t, p, provider.GenerateContent, "gemini-2.5-pro", false, `{"model":"gemini-2.5-pro","stream":false,"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`)
	if f.url != "/v1beta/models/gemini-2.5-pro:generateContent" || f.raw != `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}` {
		t.Errorf("upstream %s %s", f.url, f.raw)
	}
	if body != textResponse || resp.Usage.OutputTokens != 8 {
		t.Errorf("response %s %+v", body, *resp.Usage)
	}

	f.body = `data: {"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1}}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":"b"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":2}}` + "\n\n"
	resp, body = send(t, p, provider.GenerateContent, "g", true, `{"model":"g","stream":true,"contents":[]}`)
	if f.url != "/v1beta/models/g:streamGenerateContent?alt=sse" || body != f.body {
		t.Errorf("stream %s %s", f.url, body)
	}
	if *resp.Usage != (usage.Tokens{InputTokens: 2, OutputTokens: 2, Reported: true}) {
		t.Errorf("metered %+v", *resp.Usage)
	}
}

func TestUpstreamHooks(t *testing.T) {
	f := &fakeUpstream{status: 200, body: textResponse}
	p := f.start(t)
	p.up = Upstream{
		URL: func(model, method string) string {
			return strings.TrimSuffix(p.cfg.BaseURL, "/") + "/custom/" + model + ":" + method
		},
		Authorize: func(_ context.Context, h http.Header) error {
			h.Set("Authorization", "Bearer tok")
			return nil
		},
	}
	send(t, p, provider.ChatCompletions, "m", false, `{"model":"m","messages":[{"role":"user","content":"Hi"}]}`)
	if f.url != "/custom/m:generateContent" || f.header.Get("Authorization") != "Bearer tok" || f.header.Get("X-Goog-Api-Key") != "" {
		t.Errorf("hooks: %s %v", f.url, f.header)
	}
	p.up.Authorize = func(context.Context, http.Header) error { return io.ErrUnexpectedEOF }
	resp, body := send(t, p, provider.ChatCompletions, "m", false, `{"model":"m","messages":[{"role":"user","content":"Hi"}]}`)
	if resp.StatusCode != 502 || !strings.Contains(body, "upstream credentials") {
		t.Errorf("auth failure: %d %s", resp.StatusCode, body)
	}
}
