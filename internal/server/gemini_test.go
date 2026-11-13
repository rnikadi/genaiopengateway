package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/bodylog"
)

// geminiUpstream is a fake Gemini API that answers with reply, as server-
// sent events when streaming, and records the last request.
func geminiUpstream(t *testing.T, reply string, path *string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*path = r.URL.RequestURI()
		if r.Header.Get("X-Goog-Api-Key") != "upstream-key" {
			t.Errorf("upstream got x-goog-api-key %q", r.Header.Get("X-Goog-Api-Key"))
		}
		chunk := func(text, finish string) string {
			c := map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}}}
			if finish != "" {
				c["finishReason"] = finish
			}
			b, _ := json.Marshal(map[string]any{"candidates": []any{c}, "usageMetadata": map[string]any{"promptTokenCount": 7, "candidatesTokenCount": 3}})
			return string(b)
		}
		if strings.Contains(r.URL.RawQuery, "alt=sse") {
			w.Header().Set("Content-Type", "text/event-stream")
			half := len(reply) / 2
			_, _ = io.WriteString(w, "data: "+chunk(reply[:half], "")+"\r\n\r\ndata: "+chunk(reply[half:], "STOP")+"\r\n\r\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chunk(reply, "STOP"))
	}))
	t.Cleanup(s.Close)
	return s
}

func geminiConfig(url string) string {
	return `
providers:
  g: {type: gemini, base_url: "` + url + `", api_key: upstream-key}
models:
  chat: {targets: [{provider: g, model: gemini-2.5-flash}]}
guardrails:
  no-pii: {type: pii, detect: [email], action: redact, stages: [input, output]}
keys:
  - {name: dev, key: ` + devKey + `}
  - {name: guarded, key: ` + guardKey + `, guardrails: [no-pii]}
`
}

func googleCall(h http.Handler, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-Goog-Api-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGenerateContent(t *testing.T) {
	var path string
	h := gateway(t, geminiConfig(geminiUpstream(t, "Hello there", &path).URL), nil)
	const req = `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`

	rec := googleCall(h, "/v1beta/models/chat:generateContent", devKey, req)
	if rec.Code != 200 || path != "/v1beta/models/gemini-2.5-flash:generateContent" || !strings.Contains(rec.Body.String(), "Hello there") {
		t.Fatalf("generateContent = %d %s: %s", rec.Code, path, rec.Body)
	}
	if rec.Header().Get("X-Gateway-Target") != "g/gemini-2.5-flash" {
		t.Errorf("target header %q", rec.Header().Get("X-Gateway-Target"))
	}

	rec = googleCall(h, "/v1beta/models/chat:streamGenerateContent?alt=sse", devKey, req)
	if rec.Code != 200 || path != "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse" || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %s: %s", rec.Code, path, rec.Body)
	}

	// Errors come in Google's shape.
	for _, tc := range []struct {
		path, key string
		status    int
		gStatus   string
	}{
		{"/v1beta/models/chat:generateContent", "wrong", 401, "UNAUTHENTICATED"},
		{"/v1beta/models/nope:generateContent", devKey, 404, "NOT_FOUND"},
		{"/v1beta/models/chat:countTokens", devKey, 404, "NOT_FOUND"},
	} {
		rec := googleCall(h, tc.path, tc.key, req)
		var e struct {
			Error struct {
				Code    int    `json:"code"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != tc.status || e.Error.Code != tc.status || e.Error.Status != tc.gStatus || e.Error.Message == "" {
			t.Errorf("%s: %d %s", tc.path, rec.Code, rec.Body)
		}
	}
}

// Guardrails apply to Google's API shape too, and a redacted Gemini
// stream is rebuilt as one event.
func TestGenerateContentGuardrails(t *testing.T) {
	var path string
	h := gateway(t, geminiConfig(geminiUpstream(t, "mail carol@example.com now", &path).URL), nil)
	rec := googleCall(h, "/v1beta/models/chat:streamGenerateContent?alt=sse", guardKey, `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`)
	body := rec.Body.String()
	if rec.Code != 200 || strings.Contains(body, "carol@") || strings.Count(body, "data: ") != 1 {
		t.Fatalf("stream = %d: %s", rec.Code, body)
	}
	var got struct {
		Candidates []struct {
			Content struct {
				Parts []struct{ Text string } `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(bodylog.Assemble("text/event-stream", rec.Body.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Content.Parts[0].Text != "mail [REDACTED:email] now" || got.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("assembled = %+v", got)
	}
}

// A stream the gateway cannot read is blocked when output guardrails
// apply, never passed on unchecked.
func TestUnreadableStreamBlocked(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: alice@example.com in no known format\n\n")
	}))
	t.Cleanup(up.Close)
	h := gateway(t, guardConfig(up.URL, "no-pii"), nil)
	rec := do(h, "POST", "/v1/chat/completions", guardKey, `{"model":"chat","stream":true}`)
	if rec.Code != 400 || errorCode(t, rec) != codeGuardrailBlocked || strings.Contains(rec.Body.String(), "alice@") {
		t.Fatalf("unreadable stream = %d: %s", rec.Code, rec.Body)
	}
}
