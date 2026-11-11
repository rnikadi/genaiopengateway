package deepgram

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native/nativetest"
)

func TestSpeech(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) { _, _ = w.Write([]byte("OggS")) }}
	p := New(up.Start(t), http.DefaultClient)
	resp, body := nativetest.Send(t, p, &provider.Request{Endpoint: provider.Speech, Model: "aura-2-thalia-en",
		Body: []byte(`{"input":"hello","voice":"ignored","response_format":"opus"}`)})
	c := up.Calls[0]
	q, _ := url.ParseQuery(c.Query)
	if c.Path != "/v1/speak" || q.Get("model") != "aura-2-thalia-en" || q.Get("encoding") != "opus" || q.Get("container") != "ogg" || c.Header.Get("Authorization") != "Token up-key" || c.Body != `{"text":"hello"}` {
		t.Fatalf("call %+v", c)
	}
	if resp.StatusCode != 200 || body != "OggS" || resp.Header.Get("Content-Type") != "audio/ogg" || resp.Usage.Characters != 5 {
		t.Fatalf("%d %q %+v", resp.StatusCode, body, resp.Usage)
	}
}

func TestTranscribe(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"metadata":{"duration":90.2},"results":{"channels":[{"detected_language":"en","alternatives":[{"transcript":"Hello world.","words":[{"word":"hello","punctuated_word":"Hello","start":0.1,"end":0.4},{"word":"world","punctuated_word":"world.","start":0.5,"end":0.9}]}]}]}}`))
	}}
	p := New(up.Start(t), http.DefaultClient)
	resp, body := nativetest.Send(t, p, &provider.Request{Endpoint: provider.Transcriptions, Model: "nova-3",
		Body:  []byte(`{"model":"x"}`),
		Files: []provider.File{{Field: "file", Filename: "a.mp3", ContentType: "audio/mpeg", Data: []byte("ID3")}}})
	c := up.Calls[0]
	q, _ := url.ParseQuery(c.Query)
	if c.Path != "/v1/listen" || q.Get("model") != "nova-3" || q.Get("detect_language") != "true" || c.Header.Get("Content-Type") != "audio/mpeg" || c.Body != "ID3" {
		t.Fatalf("call %+v", c)
	}
	if resp.StatusCode != 200 || body != `{"text":"Hello world.","usage":{"seconds":90.2,"type":"duration"}}` || resp.Usage.AudioSeconds != 90.2 {
		t.Fatalf("%d %s %+v", resp.StatusCode, body, resp.Usage)
	}
	_, body = nativetest.Send(t, p, &provider.Request{Endpoint: provider.Transcriptions, Model: "nova-3",
		Body:  []byte(`{"response_format":"srt"}`),
		Files: []provider.File{{Field: "file", Data: []byte("x")}}})
	if !strings.HasPrefix(body, "1\n00:00:00,100 --> 00:00:00,900\nHello world.\n") {
		t.Fatalf("srt %q", body)
	}
	if !p.SupportsModel(provider.Speech, "aura-2-thalia-en") || p.SupportsModel(provider.Speech, "nova-3") || !p.SupportsModel(provider.Transcriptions, "nova-3") {
		t.Error("SupportsModel")
	}
}

func TestError(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"err_code":"Bad Request","err_msg":"Bad Request: failed to process audio: corrupt or unsupported data","request_id":"r"}`))
	}}
	resp, body := nativetest.Send(t, New(up.Start(t), http.DefaultClient), &provider.Request{Endpoint: provider.Transcriptions, Model: "nova-3",
		Body: []byte(`{}`), Files: []provider.File{{Field: "file", Data: []byte("x")}}})
	if resp.StatusCode != 400 || !strings.Contains(body, "corrupt or unsupported data") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}
