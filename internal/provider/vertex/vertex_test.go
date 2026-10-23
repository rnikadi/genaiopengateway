package vertex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
)

func TestHost(t *testing.T) {
	for loc, want := range map[string]string{
		"global":       "aiplatform.googleapis.com",
		"us":           "aiplatform.us.rep.googleapis.com",
		"eu":           "aiplatform.eu.rep.googleapis.com",
		"europe-west4": "europe-west4-aiplatform.googleapis.com",
	} {
		if got := Host(loc); got != want {
			t.Errorf("%s: %s", loc, got)
		}
	}
}

// fakeTokens counts token fetches, to check that a token is reused
// until it expires.
type fakeTokens struct{ n atomic.Int32 }

func (f *fakeTokens) Token() (*oauth2.Token, error) {
	n := f.n.Add(1)
	return &oauth2.Token{AccessToken: "tok" + string(rune('0'+n)), TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}, nil
}

type recorded struct {
	path   string
	header http.Header
	body   map[string]any
}

func start(t *testing.T, ts oauth2.TokenSource, respond func(w http.ResponseWriter, r *http.Request)) (*Provider, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path, rec.header, rec.body = r.URL.RequestURI(), r.Header.Clone(), map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&rec.body)
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := config.Provider{Type: config.ProviderVertex, BaseURL: srv.URL, Project: "acme-ai", Location: "europe-west4", Timeout: config.Duration(5 * time.Second)}
	tk := &tokens{}
	if ts != nil {
		tk.ts = oauth2.ReuseTokenSource(nil, ts)
	}
	return newWithTokens(cfg, srv.Client(), tk), rec
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

func TestGeminiOnVertex(t *testing.T) {
	ts := &fakeTokens{}
	p, rec := start(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)
	})
	_, body := send(t, p, provider.ChatCompletions, "gemini-2.5-pro", false, `{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if rec.path != "/v1/projects/acme-ai/locations/europe-west4/publishers/google/models/gemini-2.5-pro:generateContent" {
		t.Errorf("path %s", rec.path)
	}
	if rec.header.Get("Authorization") != "Bearer tok1" || !strings.Contains(body, `"content":"Hi"`) {
		t.Errorf("auth %q, body %s", rec.header.Get("Authorization"), body)
	}
	// A publisher prefix is accepted and the token is reused.
	send(t, p, provider.GenerateContent, "google/gemini-2.5-flash", true, `{"model":"google/gemini-2.5-flash","stream":true,"contents":[]}`, nil)
	if rec.path != "/v1/projects/acme-ai/locations/europe-west4/publishers/google/models/gemini-2.5-flash:streamGenerateContent?alt=sse" {
		t.Errorf("path %s", rec.path)
	}
	if ts.n.Load() != 1 {
		t.Errorf("tokens fetched %d times", ts.n.Load())
	}
}

func TestClaudeOnVertex(t *testing.T) {
	p, rec := start(t, &fakeTokens{}, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":streamRawPredict") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n"+
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"id":"m","type":"message","model":"claude","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)
	})
	hdr := http.Header{"Anthropic-Version": {"2023-06-01"}, "Anthropic-Beta": {"tools-2025"}}
	resp, _ := send(t, p, provider.Messages, "anthropic/claude-sonnet-4-6", false,
		`{"model":"anthropic/claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"Hi"}]}`, hdr)
	if rec.path != "/v1/projects/acme-ai/locations/europe-west4/publishers/anthropic/models/claude-sonnet-4-6:rawPredict" {
		t.Errorf("path %s", rec.path)
	}
	if _, ok := rec.body["model"]; ok || rec.body["anthropic_version"] != claudeVersion {
		t.Errorf("body %v", rec.body)
	}
	if rec.header.Get("Anthropic-Version") != "" || rec.header.Get("Anthropic-Beta") != "tools-2025" || rec.header.Get("Authorization") != "Bearer tok1" {
		t.Errorf("headers %v", rec.header)
	}
	if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 2 {
		t.Errorf("usage %+v", *resp.Usage)
	}

	// Chat completions use the Anthropic translation, streamed.
	resp, body := send(t, p, provider.ChatCompletions, "anthropic/claude-sonnet-4-6", true,
		`{"model":"anthropic/claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if !strings.HasSuffix(rec.path, "/claude-sonnet-4-6:streamRawPredict") || !strings.Contains(body, `"finish_reason":"stop"`) || resp.Usage.OutputTokens != 2 {
		t.Errorf("chat stream %s %s %+v", rec.path, body, *resp.Usage)
	}
}

func TestSupportsModel(t *testing.T) {
	p, _ := start(t, &fakeTokens{}, func(http.ResponseWriter, *http.Request) {})
	for _, tc := range []struct {
		ep    provider.Endpoint
		model string
		want  bool
	}{
		{provider.ChatCompletions, "gemini-2.5-pro", true},
		{provider.GenerateContent, "gemini-2.5-pro", true},
		{provider.Messages, "gemini-2.5-pro", false},
		{provider.ChatCompletions, "anthropic/claude-x", true},
		{provider.Messages, "anthropic/claude-x", true},
		{provider.GenerateContent, "anthropic/claude-x", false},
		{provider.ChatCompletions, "meta/llama", false},
		{provider.Embeddings, "gemini-embedding-001", false},
	} {
		if got := provider.SupportsModel(p, tc.ep, tc.model); got != tc.want {
			t.Errorf("%s %s: %v", tc.ep, tc.model, got)
		}
	}
}

// A credential failure is a 502 the router can fall back from, and the
// lookup is retried on the next request.
func TestCredentialFailure(t *testing.T) {
	p, _ := start(t, nil, func(w http.ResponseWriter, _ *http.Request) {})
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent/key.json")
	resp, body := send(t, p, provider.ChatCompletions, "gemini-x", false, `{"model":"gemini-x","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if resp.StatusCode != http.StatusBadGateway || !provider.Retryable(resp.StatusCode) || !strings.Contains(body, "upstream credentials") {
		t.Errorf("got %d %s", resp.StatusCode, body)
	}

	_, err := fromFile(context.Background(), "/nonexistent/key.json")
	if err == nil || !strings.Contains(err.Error(), "credentials_file") {
		t.Errorf("missing file: %v", err)
	}
	dir := t.TempDir()
	path := dir + "/user.json"
	if err := os.WriteFile(path, []byte(`{"type":"authorized_user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fromFile(context.Background(), path); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("user credentials: %v", err)
	}
}
