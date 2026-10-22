package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

const devKey = "test-key-0123456789"

// upstream is a fake OpenAI-compatible provider. It answers with status,
// echoes the model and path it received, and counts hits.
func upstream(t *testing.T, status int, hits *int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if r.Header.Get("Authorization") != "Bearer upstream-key" {
			t.Errorf("upstream got Authorization %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": body["model"], "path": r.URL.Path,
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

// gateway builds a server from YAML; %s placeholders take upstream URLs.
func gateway(t *testing.T, yaml string, logs *bytes.Buffer) http.Handler {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	var out io.Writer = io.Discard
	if logs != nil {
		out = logs
	}
	s, err := New(cfg, slog.New(slog.NewJSONHandler(out, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func do(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return v.Error.Code
}

func basicConfig(url string) string {
	return `
providers:
  up: {base_url: "` + url + `", api_key: upstream-key}
models:
  chat: {targets: [{provider: up, model: up-chat, price: {input_per_mtok: 1000000, output_per_mtok: 2000000}}]}
  embed: {targets: [{provider: up, model: up-embed}]}
keys:
  - {name: dev, key: ` + devKey + `}
  - {name: embed-only, key: embed-only-key-0123, allowed_models: [embed]}
`
}

func TestFallbackAndUsageLogging(t *testing.T) {
	var badHits, goodHits int
	bad := upstream(t, http.StatusServiceUnavailable, &badHits)
	good := upstream(t, http.StatusOK, &goodHits)
	var logs bytes.Buffer
	h := gateway(t, `
providers:
  bad: {base_url: "`+bad.URL+`", api_key: upstream-key}
  good: {base_url: "`+good.URL+`", api_key: upstream-key}
models:
  chat:
    targets:
      - {provider: bad, model: primary}
      - {provider: good, model: backup, price: {input_per_mtok: 1000000, output_per_mtok: 1000000}}
keys: [{name: dev, key: `+devKey+`}]
`, &logs)

	rec := do(h, "POST", "/v1/chat/completions", devKey, `{"model":"chat","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if badHits != 1 || goodHits != 1 {
		t.Fatalf("hits bad=%d good=%d", badHits, goodHits)
	}
	if got := rec.Header().Get("X-Gateway-Target"); got != "good/backup" {
		t.Fatalf("target header = %q", got)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing request id")
	}
	var line map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(l, &m) == nil && m["msg"] == "request" {
			line = m
		}
	}
	if line["fallbacks"] != 1.0 || line["input_tokens"] != 10.0 || line["output_tokens"] != 5.0 || line["cost_usd"] != 15.0 || line["key_prefix"] != devKey[:11] {
		t.Fatalf("request log = %v", line)
	}
	if strings.Contains(logs.String(), devKey) || strings.Contains(logs.String(), "upstream-key") {
		t.Fatal("secret leaked into logs")
	}
}

func TestAuthAndPolicy(t *testing.T) {
	var hits int
	h := gateway(t, basicConfig(upstream(t, 200, &hits).URL), nil)
	cases := []struct {
		name, path, key, body string
		status                int
		code                  string
	}{
		{"no key", "/v1/embeddings", "", `{"model":"embed"}`, 401, codeInvalidKey},
		{"wrong key", "/v1/embeddings", "nope-nope-nope-nope", `{"model":"embed"}`, 401, codeInvalidKey},
		{"allowed", "/v1/embeddings", "embed-only-key-0123", `{"model":"embed"}`, 200, ""},
		{"not allowed", "/v1/chat/completions", "embed-only-key-0123", `{"model":"chat"}`, 403, codeModelNotAllowed},
		{"unknown model", "/v1/chat/completions", devKey, `{"model":"nope"}`, 404, codeModelNotFound},
		{"bad json", "/v1/chat/completions", devKey, `[1,2]`, 400, codeInvalidRequest},
		{"missing model", "/v1/chat/completions", devKey, `{"messages":[]}`, 400, codeInvalidRequest},
		{"non-string model", "/v1/chat/completions", devKey, `{"model":5}`, 400, codeInvalidRequest},
		{"unsupported endpoint", "/v1/messages", devKey, `{"model":"chat"}`, 404, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(h, "POST", c.path, c.key, c.body)
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			if c.code != "" && errorCode(t, rec) != c.code {
				t.Fatalf("code = %q, want %q", errorCode(t, rec), c.code)
			}
		})
	}
}

func TestAnthropicStyleAuthAndErrors(t *testing.T) {
	var hits int
	h := gateway(t, basicConfig(upstream(t, 200, &hits).URL), nil)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"nope"}`))
	req.Header.Set("X-Api-Key", devKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var v struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 404 || v.Type != "error" || v.Error.Type != "not_found_error" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestRequestTooLarge(t *testing.T) {
	var hits int
	h := gateway(t, basicConfig(upstream(t, 200, &hits).URL)+"limits: {max_request_bytes: 64}\n", nil)
	rec := do(h, "POST", "/v1/chat/completions", devKey, `{"model":"chat","messages":"`+strings.Repeat("x", 100)+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge || hits != 0 {
		t.Fatalf("status = %d, hits = %d", rec.Code, hits)
	}
}

func TestUpstreamErrorPassesThrough(t *testing.T) {
	var hits int
	h := gateway(t, basicConfig(upstream(t, http.StatusBadRequest, &hits).URL), nil)
	rec := do(h, "POST", "/v1/chat/completions", devKey, `{"model":"chat"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"path":"/chat/completions"`) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestStreamingPassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{"data: {\"x\":1}\n\n", "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\n", "data: [DONE]\n\n"} {
			_, _ = io.WriteString(w, chunk)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(up.Close)
	h := gateway(t, basicConfig(up.URL), nil)
	rec := do(h, "POST", "/v1/chat/completions", devKey, `{"model":"chat","stream":true}`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "data: {\"x\":1}\n\n") || !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("body = %q", rec.Body)
	}
}

func TestListModelsFiltersByKey(t *testing.T) {
	var hits int
	h := gateway(t, basicConfig(upstream(t, 200, &hits).URL), nil)
	rec := do(h, "GET", "/v1/models", "embed-only-key-0123", "")
	if body := rec.Body.String(); rec.Code != 200 || !strings.Contains(body, `"embed"`) || strings.Contains(body, `"chat"`) {
		t.Fatalf("models = %d %s", rec.Code, body)
	}
	if rec := do(h, "GET", "/v1/models", "", ""); rec.Code != 401 {
		t.Fatalf("unauthenticated list = %d", rec.Code)
	}
}

func TestNoKeysRejectsEverything(t *testing.T) {
	var hits int
	h := gateway(t, `
providers: {up: {base_url: "`+upstream(t, 200, &hits).URL+`"}}
models: {chat: {targets: [{provider: up, model: m}]}}
`, nil)
	if rec := do(h, "POST", "/v1/chat/completions", "", `{"model":"chat"}`); rec.Code != 401 || hits != 0 {
		t.Fatalf("status = %d, hits = %d", rec.Code, hits)
	}
}

func TestRequestIDEcho(t *testing.T) {
	var hits int
	h := gateway(t, basicConfig(upstream(t, 200, &hits).URL), nil)
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("X-Request-Id", "client-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") != "client-123" {
		t.Fatal("safe id not echoed")
	}
	req.Header.Set("X-Request-Id", "bad id\nwith newline")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); !strings.HasPrefix(got, "req_") {
		t.Fatalf("unsafe id kept: %q", got)
	}
}

func TestRateLimitAndBudgetEnforced(t *testing.T) {
	var hits int
	h := gateway(t, `
providers: {up: {base_url: "`+upstream(t, 200, &hits).URL+`", api_key: upstream-key}}
models: {chat: {targets: [{provider: up, model: m, price: {input_per_mtok: 100000, output_per_mtok: 100000}}]}}
keys:
  - {name: slow, key: slow-key-0123456789, rpm: 1}
  - {name: cheap, key: cheap-key-0123456789, budget: {amount_usd: 1, period: daily}}
`, nil)
	first := do(h, "POST", "/v1/chat/completions", "slow-key-0123456789", `{"model":"chat"}`)
	if first.Code != 200 || first.Header().Get("X-Ratelimit-Limit-Requests") != "1" || first.Header().Get("X-Ratelimit-Remaining-Requests") != "0" {
		t.Fatalf("first = %d %v", first.Code, first.Header())
	}
	second := do(h, "POST", "/v1/chat/completions", "slow-key-0123456789", `{"model":"chat"}`)
	if second.Code != 429 || errorCode(t, second) != "rate_limit_exceeded" || second.Header().Get("Retry-After") == "" {
		t.Fatalf("second = %d %s %v", second.Code, second.Body, second.Header())
	}

	// Each call costs 15 tokens * $0.1 = $1.50, so the second is over budget.
	if rec := do(h, "POST", "/v1/chat/completions", "cheap-key-0123456789", `{"model":"chat"}`); rec.Code != 200 {
		t.Fatalf("budget first = %d", rec.Code)
	}
	rec := do(h, "POST", "/v1/chat/completions", "cheap-key-0123456789", `{"model":"chat"}`)
	if rec.Code != 429 || errorCode(t, rec) != "budget_exceeded" {
		t.Fatalf("budget second = %d %s", rec.Code, rec.Body)
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits)
	}
}

func TestAnthropicProviderEndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "upstream-key" {
			t.Errorf("upstream got %s key %q", r.URL.Path, r.Header.Get("X-Api-Key"))
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	t.Cleanup(up.Close)
	var logs bytes.Buffer
	h := gateway(t, `
providers: {ant: {type: anthropic, base_url: "`+up.URL+`", api_key: upstream-key}}
models: {claude: {targets: [{provider: ant, model: claude-x}]}}
keys: [{name: dev, key: `+devKey+`}]
`, &logs)
	chat := do(h, "POST", "/v1/chat/completions", devKey, `{"model":"claude","messages":[{"role":"user","content":"hello"}]}`)
	if chat.Code != 200 || !strings.Contains(chat.Body.String(), `"object":"chat.completion"`) || !strings.Contains(chat.Body.String(), `"content":"hi"`) {
		t.Fatalf("chat = %d %s", chat.Code, chat.Body)
	}
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("X-Api-Key", devKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"type":"text"`) {
		t.Fatalf("messages = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), `"input_tokens":4`) {
		t.Fatal("usage not logged")
	}
}
