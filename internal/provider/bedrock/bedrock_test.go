package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

var staticCreds = credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "")

// fake is a Bedrock runtime that checks SigV4 signatures and answers
// each path with a handler.
type fake struct {
	t      *testing.T
	mu     sync.Mutex // Titan embeddings call in parallel
	paths  []string
	bodies []map[string]any
	header http.Header
	answer func(w http.ResponseWriter, r *http.Request)
}

func (f *fake) start(apiKey string) *Provider {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.URL.EscapedPath())
		f.header = r.Header.Clone()
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.bodies = append(f.bodies, body)
		if apiKey == "" {
			f.checkSignature(r, raw)
		}
		f.answer(w, r)
	}))
	f.t.Cleanup(srv.Close)
	cfg := config.Provider{Type: config.ProviderBedrock, BaseURL: srv.URL, Region: "us-east-1", APIKey: config.Secret(apiKey), Timeout: config.Duration(5 * time.Second)}
	return newWithCredentials(cfg, srv.Client(), staticCreds)
}

// checkSignature signs the received request again, at the time it says
// it was signed, and compares.
func (f *fake) checkSignature(r *http.Request, body []byte) {
	f.t.Helper()
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		f.t.Errorf("x-amz-date: %v", err)
		return
	}
	again, _ := http.NewRequest(r.Method, "http://"+r.Host+r.URL.RequestURI(), bytes.NewReader(body))
	for _, h := range []string{"Content-Type", "Accept"} {
		again.Header.Set(h, r.Header.Get(h))
	}
	sum := sha256.Sum256(body)
	c, _ := staticCreds.Retrieve(context.Background())
	if err := v4.NewSigner().SignHTTP(context.Background(), c, again, hex.EncodeToString(sum[:]), "bedrock", "us-east-1", at); err != nil {
		f.t.Fatal(err)
	}
	if got, want := r.Header.Get("Authorization"), again.Header.Get("Authorization"); got != want || !strings.HasPrefix(got, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
		f.t.Errorf("signature\n got %s\nwant %s", got, want)
	}
}

func send(t *testing.T, p *Provider, ep provider.Endpoint, model string, stream bool, body string, hdr http.Header) (*provider.Response, string) {
	t.Helper()
	resp, err := p.Send(context.Background(), &provider.Request{Endpoint: ep, Model: model, Stream: stream, Body: []byte(body), Header: hdr})
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

// frames encodes AWS event stream messages: each is an event type and a
// JSON payload, or an exception when the type starts with "!".
func frames(t *testing.T, events ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	for _, e := range events {
		m := eventstream.Message{Payload: []byte(e[1])}
		if typ, ok := strings.CutPrefix(e[0], "!"); ok {
			m.Headers.Set(":message-type", eventstream.StringValue("exception"))
			m.Headers.Set(":exception-type", eventstream.StringValue(typ))
		} else {
			m.Headers.Set(":message-type", eventstream.StringValue("event"))
			m.Headers.Set(":event-type", eventstream.StringValue(e[0]))
		}
		if err := enc.Encode(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

const converseResponseBody = `{"output":{"message":{"role":"assistant","content":[{"text":"Hello"}]}},"stopReason":"end_turn",
	"usage":{"inputTokens":10,"outputTokens":3,"totalTokens":13,"cacheReadInputTokens":4,"cacheWriteInputTokens":2}}`

func TestConverse(t *testing.T) {
	f := &fake{t: t, answer: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, converseResponseBody) }}
	p := f.start("")
	resp, body := send(t, p, provider.ChatCompletions, "us.meta.llama3-3-70b-instruct-v1:0", false, `{"model":"x","messages":[
		{"role":"system","content":"Be brief."},
		{"role":"user","content":[{"type":"text","text":"Hi"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/"}}]}],
		"max_tokens":50,"temperature":0.5,"stop":["END"]}`, nil)
	if f.paths[0] != "/model/us.meta.llama3-3-70b-instruct-v1%3A0/converse" || f.header.Get("Accept") != "application/json" {
		t.Errorf("path %s, accept %s", f.paths[0], f.header.Get("Accept"))
	}
	want := `{"inferenceConfig":{"maxTokens":50,"stopSequences":["END"],"temperature":0.5},"messages":[{"content":[{"text":"Hi"},` +
		`{"image":{"format":"jpeg","source":{"bytes":"/9j/"}}}],"role":"user"}],"system":[{"text":"Be brief."}]}`
	if got := asJSON(t, f.bodies[0]); got != want {
		t.Errorf("converse body\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, `"finish_reason":"stop"`) ||
		!strings.Contains(body, `"prompt_tokens":16`) || !strings.Contains(body, `"cached_tokens":4`) {
		t.Errorf("response %s", body)
	}
	if *resp.Usage != (usage.Tokens{InputTokens: 16, OutputTokens: 3, CachedInputTokens: 4, Reported: true}) {
		t.Errorf("metered %+v", *resp.Usage)
	}
}

func TestConverseTools(t *testing.T) {
	f := &fake{t: t, answer: func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"toolUse":{"toolUseId":"tu1","name":"weather","input":{"city":"Oslo"}}}]}},"stopReason":"tool_use","usage":{"inputTokens":1,"outputTokens":1}}`)
	}}
	p := f.start("")
	_, body := send(t, p, provider.ChatCompletions, "m", false, `{"model":"m","messages":[
		{"role":"user","content":"Weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"tu0","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Bergen\"}"}}]},
		{"role":"tool","tool_call_id":"tu0","content":"rain"},
		{"role":"user","content":"And Oslo?"}],
		"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],"tool_choice":"required"}`, nil)
	want := `{"messages":[{"content":[{"text":"Weather?"}],"role":"user"},` +
		`{"content":[{"toolUse":{"input":{"city":"Bergen"},"name":"weather","toolUseId":"tu0"}}],"role":"assistant"},` +
		`{"content":[{"toolResult":{"content":[{"text":"rain"}],"toolUseId":"tu0"}},{"text":"And Oslo?"}],"role":"user"}],` +
		`"toolConfig":{"toolChoice":{"any":{}},"tools":[{"toolSpec":{"inputSchema":{"json":{"type":"object"}},"name":"weather"}}]}}`
	if got := asJSON(t, f.bodies[0]); got != want {
		t.Errorf("converse body\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(body, `"tool_calls":[{"function":{"arguments":"{\"city\":\"Oslo\"}","name":"weather"},"id":"tu1","type":"function"}]`) ||
		!strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("response %s", body)
	}

	for _, bad := range []string{
		`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"none"}`,
		`{"model":"m","messages":[],"n":2}`,
		`{"model":"m","messages":[],"response_format":{"type":"json_schema"}}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/a.png"}}]}]}`,
	} {
		resp, out := send(t, p, provider.ChatCompletions, "m", false, bad, nil)
		if resp.StatusCode != 400 || !strings.Contains(out, "invalid_request_error") {
			t.Errorf("%s: %d %s", bad, resp.StatusCode, out)
		}
	}
	if len(f.paths) != 1 {
		t.Errorf("rejected requests reached the upstream: %v", f.paths)
	}
}

func TestConverseStream(t *testing.T) {
	stream := frames(t,
		[2]string{"messageStart", `{"role":"assistant"}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hel"},"p":"x"}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"lo"}}`},
		[2]string{"contentBlockStart", `{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tu1","name":"f"}}}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"a\":"}}}`},
		[2]string{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"1}"}}}`},
		[2]string{"messageStop", `{"stopReason":"tool_use"}`},
		[2]string{"metadata", `{"usage":{"inputTokens":5,"outputTokens":7}}`},
	)
	f := &fake{t: t, answer: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(stream)
	}}
	p := f.start("bedrock-api-key")
	resp, body := send(t, p, provider.ChatCompletions, "m", true, `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if f.paths[0] != "/model/m/converse-stream" || f.header.Get("Authorization") != "Bearer bedrock-api-key" || f.header.Get("Accept") != "application/vnd.amazon.eventstream" {
		t.Errorf("request %s %v", f.paths[0], f.header)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content type %q", resp.Header.Get("Content-Type"))
	}
	var text, args, finish string
	var gotUsage bool
	for line := range strings.SplitSeq(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Function struct{ Arguments string } `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatal(err)
		}
		for _, ch := range c.Choices {
			text += ch.Delta.Content
			for _, tc := range ch.Delta.ToolCalls {
				args += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
		gotUsage = gotUsage || c.Usage["completion_tokens"] == 7.0
	}
	if text != "Hello" || args != `{"a":1}` || finish != "tool_calls" || !gotUsage || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream %q %q %q %v:\n%s", text, args, finish, gotUsage, body)
	}
	if *resp.Usage != (usage.Tokens{InputTokens: 5, OutputTokens: 7, Reported: true}) {
		t.Errorf("metered %+v", *resp.Usage)
	}

	// An exception in the stream reaches the client as an error event.
	stream = frames(t, [2]string{"messageStart", `{"role":"assistant"}`}, [2]string{"!ThrottlingException", `{"message":"slow down"}`})
	_, body = send(t, p, provider.ChatCompletions, "m", true, `{"model":"m","stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if !strings.Contains(body, `"message":"slow down"`) || strings.Contains(body, "[DONE]") {
		t.Errorf("exception: %s", body)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		status    int
		errType   string
		want      int
		anthropic string
	}{
		{400, "ValidationException", 400, "invalid_request_error"},
		{403, "AccessDeniedException", 403, "permission_error"},
		{429, "ThrottlingException", 429, "rate_limit_error"},
		{400, "ServiceQuotaExceededException", 429, "rate_limit_error"},
		{429, "ModelNotReadyException", 503, "overloaded_error"},
		{500, "InternalServerException", 500, "api_error"},
	} {
		f := &fake{t: t, answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Amzn-Errortype", tc.errType+":http://internal.amazon.com/coral/com.amazon.bedrock/")
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"message":"it failed"}`)
		}}
		p := f.start("k")
		resp, body := send(t, p, provider.ChatCompletions, "m", false, `{"model":"m","messages":[{"role":"user","content":"Hi"}]}`, nil)
		if resp.StatusCode != tc.want || !strings.Contains(body, `"type":"`+tc.errType+`"`) || !strings.Contains(body, "it failed") {
			t.Errorf("chat %s: %d %s", tc.errType, resp.StatusCode, body)
		}
		resp, body = send(t, p, provider.Messages, "anthropic.claude-x", false, `{"model":"anthropic.claude-x","max_tokens":5,"messages":[]}`, nil)
		if resp.StatusCode != tc.want || !strings.Contains(body, `"type":"`+tc.anthropic+`"`) || !strings.Contains(body, `"type":"error"`) {
			t.Errorf("messages %s: %d %s", tc.errType, resp.StatusCode, body)
		}
	}
}

func TestClaudeMessages(t *testing.T) {
	chunk := func(ev string) string {
		return `{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(ev)) + `"}`
	}
	stream := frames(t,
		[2]string{"chunk", chunk(`{"type":"message_start","message":{"id":"m1","model":"claude","usage":{"input_tokens":9,"output_tokens":0}}}`)},
		[2]string{"chunk", chunk(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`)},
		[2]string{"chunk", chunk(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`)},
	)
	f := &fake{t: t, answer: func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "invoke-with-response-stream") {
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(stream)
			return
		}
		_, _ = io.WriteString(w, `{"id":"m1","type":"message","content":[{"type":"text","text":"Hi"}],"usage":{"input_tokens":9,"output_tokens":4}}`)
	}}
	p := f.start("")
	hdr := http.Header{"Anthropic-Version": {"2023-06-01"}, "Anthropic-Beta": {"a-1, b-2"}}
	const model = "eu.anthropic.claude-sonnet-4-6"
	resp, _ := send(t, p, provider.Messages, model, false, `{"model":"`+model+`","max_tokens":5,"messages":[{"role":"user","content":"Hi"}]}`, hdr)
	if f.paths[0] != "/model/eu.anthropic.claude-sonnet-4-6/invoke" || resp.Usage.InputTokens != 9 {
		t.Errorf("invoke %s %+v", f.paths[0], *resp.Usage)
	}
	if got := asJSON(t, f.bodies[0]); got != `{"anthropic_beta":["a-1","b-2"],"anthropic_version":"bedrock-2023-05-31","max_tokens":5,"messages":[{"content":"Hi","role":"user"}]}` {
		t.Errorf("body %s", got)
	}
	if f.header.Get("Anthropic-Version") != "" || f.header.Get("Anthropic-Beta") != "" {
		t.Errorf("anthropic headers sent: %v", f.header)
	}

	resp, body := send(t, p, provider.Messages, model, true, `{"model":"`+model+`","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, hdr)
	if f.paths[1] != "/model/eu.anthropic.claude-sonnet-4-6/invoke-with-response-stream" || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("stream %s %v", f.paths[1], resp.Header)
	}
	if !strings.Contains(body, "event: message_start\ndata: {\"type\":\"message_start\"") || !strings.Contains(body, "event: content_block_delta\n") {
		t.Errorf("stream body %s", body)
	}
	if *resp.Usage != (usage.Tokens{InputTokens: 9, OutputTokens: 4, Reported: true}) {
		t.Errorf("metered %+v", *resp.Usage)
	}
}

func TestEmbeddings(t *testing.T) {
	f := &fake{t: t, answer: func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(r.URL.Path, "cohere") {
			w.Header().Set("X-Amzn-Bedrock-Input-Token-Count", "6")
			_, _ = io.WriteString(w, `{"embeddings":[[0.1,0.2],[0.3,0.4]]}`)
			return
		}
		_, _ = io.WriteString(w, `{"embedding":[1,2],"inputTextTokenCount":3}`)
	}}
	p := f.start("k")
	resp, body := send(t, p, provider.Embeddings, "amazon.titan-embed-text-v2:0", false, `{"model":"e","input":["a","b","c"],"dimensions":256}`, nil)
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if len(out.Data) != 3 || out.Data[2].Index != 2 || out.Usage.PromptTokens != 9 || resp.Usage.InputTokens != 9 || len(f.paths) != 3 {
		t.Errorf("titan: %s (%d calls)", body, len(f.paths))
	}
	if f.bodies[0]["dimensions"] != 256.0 || f.paths[0] != "/model/amazon.titan-embed-text-v2%3A0/invoke" {
		t.Errorf("titan request %v %s", f.bodies[0], f.paths[0])
	}

	_, body = send(t, p, provider.Embeddings, "cohere.embed-english-v3", false, `{"model":"e","input":["a","b"]}`, nil)
	_ = json.Unmarshal([]byte(body), &out)
	if len(out.Data) != 2 || out.Data[1].Embedding[1] != 0.4 || out.Usage.PromptTokens != 6 {
		t.Errorf("cohere: %s", body)
	}
	if got := asJSON(t, f.bodies[3]); got != `{"input_type":"search_document","texts":["a","b"]}` {
		t.Errorf("cohere request %s", got)
	}

	resp, _ = send(t, p, provider.Embeddings, "cohere.embed-english-v3", false, `{"model":"e","input":"a","encoding_format":"base64"}`, nil)
	if resp.StatusCode != 400 {
		t.Errorf("base64 encoding: %d", resp.StatusCode)
	}
}

func TestSupportsModel(t *testing.T) {
	p := New(config.Provider{Region: "us-east-1"}, http.DefaultClient)
	for _, tc := range []struct {
		ep    provider.Endpoint
		model string
		want  bool
	}{
		{provider.ChatCompletions, "us.anthropic.claude-sonnet-4-6", true},
		{provider.Messages, "us.anthropic.claude-sonnet-4-6", true},
		{provider.ChatCompletions, "amazon.nova-pro-v1:0", true},
		{provider.Messages, "amazon.nova-pro-v1:0", false},
		{provider.Embeddings, "amazon.titan-embed-text-v2:0", true},
		{provider.ChatCompletions, "amazon.titan-embed-text-v2:0", false},
		{provider.Embeddings, "cohere.embed-multilingual-v3", true},
		{provider.Responses, "amazon.nova-pro-v1:0", false},
	} {
		if got := provider.SupportsModel(p, tc.ep, tc.model); got != tc.want {
			t.Errorf("%s %s: %v", tc.ep, tc.model, got)
		}
	}
	if got := p.url("arn:aws:bedrock:us-east-1:123:application-inference-profile/abc", "converse"); got != "https://bedrock-runtime.us-east-1.amazonaws.com/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A123%3Aapplication-inference-profile%2Fabc/converse" {
		t.Errorf("arn url %s", got)
	}
}
