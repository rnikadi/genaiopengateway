package assemblyai

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native/nativetest"
)

func init() { pollInterval = time.Millisecond }

func fake(final string) *nativetest.Upstream {
	polls := 0
	return &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, body string) {
		switch {
		case r.URL.Path == "/v2/upload":
			_, _ = w.Write([]byte(`{"upload_url":"https://cdn.assemblyai.com/upload/u1"}`))
		case r.URL.Path == "/v2/transcript":
			_, _ = w.Write([]byte(`{"id":"t1","status":"queued"}`))
		case r.Method == "GET":
			polls++
			if polls < 2 {
				_, _ = w.Write([]byte(`{"id":"t1","status":"processing"}`))
				return
			}
			_, _ = w.Write([]byte(final))
		case r.Method == "DELETE":
			_, _ = w.Write([]byte(`{"id":"t1","status":"completed","text":"Deleted by user."}`))
		}
	}}
}

func file() []provider.File {
	return []provider.File{{Field: "file", Filename: "a.wav", Data: []byte("RIFFaudio")}}
}

func TestTranscribe(t *testing.T) {
	up := fake(`{"id":"t1","status":"completed","text":"Hello world.","language_code":"en","audio_duration":42,"words":[{"text":"Hello","start":100,"end":400},{"text":"world.","start":500,"end":900}]}`)
	p := New(up.Start(t), http.DefaultClient)
	resp, body := nativetest.Send(t, p, &provider.Request{Endpoint: provider.Transcriptions, Model: "universal",
		Body: []byte(`{"response_format":"verbose_json","prompt":"names: Ada"}`), Files: file()})
	if resp.StatusCode != 200 || resp.Usage.AudioSeconds != 42 || !strings.Contains(body, `"text":"Hello world."`) || !strings.Contains(body, `{"word":"world.","start":0.5,"end":0.9}`) {
		t.Fatalf("%d %s %+v", resp.StatusCode, body, resp.Usage)
	}
	var paths []string
	for _, c := range up.Calls {
		paths = append(paths, c.Method+" "+c.Path)
		if c.Header.Get("Authorization") != "up-key" {
			t.Errorf("%s %s: authorization %q", c.Method, c.Path, c.Header.Get("Authorization"))
		}
	}
	if got := strings.Join(paths, ", "); got != "POST /v2/upload, POST /v2/transcript, GET /v2/transcript/t1, GET /v2/transcript/t1, DELETE /v2/transcript/t1" {
		t.Fatalf("calls %s", got)
	}
	if b := up.Calls[0].Body; b != "RIFFaudio" {
		t.Fatalf("upload %q", b)
	}
	if b := up.Calls[1].Body; !strings.Contains(b, `"audio_url":"https://cdn.assemblyai.com/upload/u1"`) || !strings.Contains(b, `"speech_model":"universal"`) || !strings.Contains(b, `"language_detection":true`) || !strings.Contains(b, `"prompt":"names: Ada"`) {
		t.Fatalf("start %s", b)
	}
}

func TestTranscriptError(t *testing.T) {
	up := fake(`{"id":"t1","status":"error","error":"File does not appear to contain audio."}`)
	resp, body := nativetest.Send(t, New(up.Start(t), http.DefaultClient), &provider.Request{Endpoint: provider.Transcriptions, Model: "universal", Body: []byte(`{}`), Files: file()})
	if resp.StatusCode != 400 || !strings.Contains(body, "does not appear to contain audio") || up.Calls[len(up.Calls)-1].Method != "DELETE" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestTimeout(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		switch r.URL.Path {
		case "/v2/upload":
			_, _ = w.Write([]byte(`{"upload_url":"u"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"t1","status":"processing"}`))
		}
	}}
	cfg := up.Start(t)
	cfg.Timeout = config.Duration(50 * time.Millisecond)
	resp, _ := nativetest.Send(t, New(cfg, http.DefaultClient), &provider.Request{Endpoint: provider.Transcriptions, Model: "universal", Body: []byte(`{}`), Files: file()})
	// A poll cancelled by the timeout can reach the upstream after the
	// DELETE, so look for the DELETE among all calls, not only the last.
	deleted := false
	for _, c := range up.Calls {
		deleted = deleted || c.Method == "DELETE"
	}
	if resp.StatusCode != http.StatusGatewayTimeout || !deleted {
		t.Fatalf("%d, calls %+v", resp.StatusCode, up.Calls)
	}
}
