package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mediaUpstream answers the OpenAI media endpoints and records what it
// was sent.
type mediaUpstream struct {
	*httptest.Server
	model, file, contentType string
}

func newMediaUpstream(t *testing.T) *mediaUpstream {
	t.Helper()
	u := &mediaUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.contentType = r.Header.Get("Content-Type")
		var body map[string]any
		if strings.HasPrefix(u.contentType, "multipart/") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("upstream multipart: %v", err)
			}
			u.model = r.FormValue("model")
			body = map[string]any{"response_format": r.FormValue("response_format"), "stream": r.FormValue("stream")}
			if f, _, err := r.FormFile("file"); err == nil {
				b, _ := io.ReadAll(f)
				u.file = string(b)
			}
		} else {
			_ = json.NewDecoder(r.Body).Decode(&body)
			u.model, _ = body["model"].(string)
		}
		switch r.URL.Path {
		case "/images/generations":
			if body["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"cA==\"}\n\n")
				io.WriteString(w, "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"aQ==\",\"usage\":{\"input_tokens\":10,\"output_tokens\":100}}\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"created":1,"data":[{"b64_json":"aQ=="},{"b64_json":"aQ=="}],"usage":{"input_tokens":10,"output_tokens":100}}`)
		case "/audio/speech":
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write([]byte{0xff, 0xfb, 0x90, 0x00})
		case "/audio/transcriptions":
			if body["response_format"] == "text" {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				io.WriteString(w, "call jane@example.com\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"text":"call jane@example.com","usage":{"type":"duration","seconds":30}}`)
		case "/moderations", "/rerank":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"results":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func mediaConfig(url string) string {
	return `
providers:
  up: {base_url: "` + url + `", api_key: upstream-key}
  claude: {type: anthropic, base_url: "http://127.0.0.1:1", api_key: k}
models:
  image: {targets: [{provider: up, model: gpt-image-1, price: {per_image: 0.04, input_per_mtok: 1000, output_per_mtok: 1000}}]}
  speech: {targets: [{provider: up, model: tts-1, price: {per_mchars: 15000000}}]}
  transcribe: {targets: [{provider: up, model: whisper-1, price: {per_audio_minute: 0.006}}]}
  moderate: {targets: [{provider: up, model: omni-moderation-latest}]}
  rerank: {targets: [{provider: up, model: rerank-v3.5, price: {per_request: 0.002}}]}
  claude: {targets: [{provider: claude, model: claude-haiku-4-5}]}
guardrails:
  no-pii: {type: pii, detect: [email], action: redact, stages: [input, output]}
keys:
  - {name: dev, key: ` + devKey + `}
  - {name: guarded, key: ` + guardKey + `, guardrails: [no-pii]}
`
}

// lastRequestLog returns the last request log line.
func lastRequestLog(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	var line map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(l, &m) == nil && m["msg"] == "request" {
			line = m
		}
	}
	return line
}

func multipartBody(t *testing.T, fields map[string]string, file string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile("file", "a.mp3")
	fw.Write([]byte(file))
	w.Close()
	return buf.String(), w.FormDataContentType()
}

func doType(h http.Handler, path, key, body, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMediaEndpoints(t *testing.T) {
	up := newMediaUpstream(t)
	var logs bytes.Buffer
	h := gateway(t, mediaConfig(up.URL), &logs)
	near := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

	t.Run("images", func(t *testing.T) {
		rec := do(h, "POST", "/v1/images/generations", devKey, `{"model":"image","prompt":"a falcon"}`)
		if rec.Code != 200 || up.model != "gpt-image-1" {
			t.Fatalf("status %d, upstream model %q: %s", rec.Code, up.model, rec.Body)
		}
		// 2 images at 0.04, plus 110 tokens at 1000 per million.
		if l := lastRequestLog(t, &logs); l["images"] != 2.0 || !near(l["cost_usd"].(float64), 0.08+0.11) || l["endpoint"] != "images.generations" {
			t.Fatalf("log = %v", l)
		}
	})
	t.Run("image stream", func(t *testing.T) {
		rec := do(h, "POST", "/v1/images/generations", guardKey, `{"model":"image","prompt":"a falcon","stream":true}`)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "image_generation.completed") {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		if l := lastRequestLog(t, &logs); l["images"] != 1.0 || l["input_tokens"] != 10.0 {
			t.Fatalf("log = %v", l)
		}
	})
	t.Run("speech", func(t *testing.T) {
		rec := do(h, "POST", "/v1/audio/speech", guardKey, `{"model":"speech","input":"héllo","voice":"alloy"}`)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "audio/mpeg" || rec.Body.Len() != 4 {
			t.Fatalf("status %d type %q len %d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
		// 5 characters at 15e6 per million.
		if l := lastRequestLog(t, &logs); l["characters"] != 5.0 || !near(l["cost_usd"].(float64), 75) {
			t.Fatalf("log = %v", l)
		}
	})
	t.Run("transcription", func(t *testing.T) {
		body, ct := multipartBody(t, map[string]string{"model": "transcribe"}, "RIFFaudio")
		rec := doType(h, "/v1/audio/transcriptions", guardKey, body, ct)
		if rec.Code != 200 || up.model != "whisper-1" || up.file != "RIFFaudio" || !strings.HasPrefix(up.contentType, "multipart/form-data") {
			t.Fatalf("status %d, upstream model %q file %q: %s", rec.Code, up.model, up.file, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "jane@example.com") {
			t.Fatalf("output guardrail did not redact the transcript: %s", rec.Body)
		}
		if l := lastRequestLog(t, &logs); l["audio_seconds"] != 30.0 || !near(l["cost_usd"].(float64), 0.003) {
			t.Fatalf("log = %v", l)
		}
	})
	t.Run("transcription as text", func(t *testing.T) {
		body, ct := multipartBody(t, map[string]string{"model": "transcribe", "response_format": "text"}, "RIFF")
		rec := doType(h, "/v1/audio/transcriptions", guardKey, body, ct)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "jane@example.com") || !strings.Contains(rec.Body.String(), "call ") {
			t.Fatalf("status %d: %q", rec.Code, rec.Body)
		}
	})
	t.Run("streamed transcription with output guardrails", func(t *testing.T) {
		body, ct := multipartBody(t, map[string]string{"model": "transcribe", "stream": "true"}, "RIFF")
		if rec := doType(h, "/v1/audio/transcriptions", guardKey, body, ct); rec.Code != 400 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		// Without guardrails the stream flag goes upstream as a field.
		if rec := doType(h, "/v1/audio/transcriptions", devKey, body, ct); rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	})
	t.Run("rerank", func(t *testing.T) {
		rec := do(h, "POST", "/v1/rerank", devKey, `{"model":"rerank","query":"q","documents":["a","b"]}`)
		if rec.Code != 200 || up.model != "rerank-v3.5" {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		if l := lastRequestLog(t, &logs); !near(l["cost_usd"].(float64), 0.002) {
			t.Fatalf("log = %v", l)
		}
	})
	t.Run("moderation", func(t *testing.T) {
		if rec := do(h, "POST", "/v1/moderations", devKey, `{"model":"moderate","input":"hi"}`); rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	})
	t.Run("unsupported provider", func(t *testing.T) {
		rec := do(h, "POST", "/v1/audio/speech", devKey, `{"model":"claude","input":"hi","voice":"alloy"}`)
		if rec.Code != 404 || errorCode(t, rec) != codeModelNotFound {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	})
	t.Run("bad multipart", func(t *testing.T) {
		if rec := doType(h, "/v1/audio/transcriptions", devKey, "not multipart", "multipart/form-data; boundary=x"); rec.Code != 400 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	})
}

// Input guardrails see the text of media requests.
func TestMediaInputGuardrails(t *testing.T) {
	up := newMediaUpstream(t)
	h := gateway(t, mediaConfig(up.URL), nil)
	rec := do(h, "POST", "/v1/images/generations", guardKey, `{"model":"image","prompt":"draw jane@example.com"}`)
	if rec.Code != 200 || rec.Header().Get(guardrailsHeader) == "" {
		t.Fatalf("status %d, header %q", rec.Code, rec.Header().Get(guardrailsHeader))
	}
	rec = do(h, "POST", "/v1/rerank", guardKey, `{"model":"rerank","query":"mail jane@example.com","documents":["bob@example.com"]}`)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get(guardrailsHeader), "no-pii") {
		t.Fatalf("rerank status %d, header %q", rec.Code, rec.Header().Get(guardrailsHeader))
	}
}
