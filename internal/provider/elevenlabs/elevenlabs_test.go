package elevenlabs

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native/nativetest"
)

func TestSpeech(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write([]byte{1, 0, 2, 0})
	}}
	p := New(up.Start(t), http.DefaultClient)
	resp, body := nativetest.Send(t, p, &provider.Request{Endpoint: provider.Speech, Model: "eleven_flash_v2_5",
		Body: []byte(`{"model":"eleven_flash_v2_5","input":"héllo","voice":"JBFqnCBsd6RMkjVDRZzb","response_format":"wav","speed":1.1}`)})
	c := up.Calls[0]
	if c.Path != "/v1/text-to-speech/JBFqnCBsd6RMkjVDRZzb" || c.Query != "output_format=pcm_24000" || c.Header.Get("Xi-Api-Key") != "up-key" || c.Header.Get("X-Extra") != "1" {
		t.Fatalf("call %+v", c)
	}
	if c.Body != `{"model_id":"eleven_flash_v2_5","text":"héllo","voice_settings":{"speed":1.1}}` {
		t.Fatalf("body %s", c.Body)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" || !strings.HasPrefix(body, "RIFF") || resp.Usage.Characters != 5 {
		t.Fatalf("%d %v %q %+v", resp.StatusCode, resp.Header, body, resp.Usage)
	}

	resp, body = nativetest.Send(t, p, &provider.Request{Endpoint: provider.Speech, Model: "m", Body: []byte(`{"input":"x"}`)})
	if resp.StatusCode != 400 || !strings.Contains(body, "voice is required") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, _ = nativetest.Send(t, p, &provider.Request{Endpoint: provider.Speech, Model: "m", Body: []byte(`{"input":"x","voice":"v","response_format":"flac"}`)})
	if resp.StatusCode != 400 || len(up.Calls) != 1 {
		t.Fatalf("flac: %d, %d calls", resp.StatusCode, len(up.Calls))
	}
}

func TestTranscribe(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"language_code":"eng","text":"Hello world.","words":[{"text":"Hello","start":0.1,"end":0.5,"type":"word"},{"text":" ","start":0.5,"end":0.6,"type":"spacing"},{"text":"world.","start":0.6,"end":1.25,"type":"word"}]}`))
	}}
	p := New(up.Start(t), http.DefaultClient)
	resp, body := nativetest.Send(t, p, &provider.Request{Endpoint: provider.Transcriptions, Model: "scribe_v1",
		Body:  []byte(`{"model":"x","language":"en","response_format":"verbose_json"}`),
		Files: []provider.File{{Field: "file", Filename: "a.wav", ContentType: "audio/wav", Data: []byte("RIFFdata")}}})
	c := up.Calls[0]
	if c.Path != "/v1/speech-to-text" || !strings.Contains(c.Body, "scribe_v1") || !strings.Contains(c.Body, `name="language_code"`) || !strings.Contains(c.Body, "RIFFdata") {
		t.Fatalf("call %+v", c)
	}
	if resp.StatusCode != 200 || resp.Usage.AudioSeconds != 1.25 || !strings.Contains(body, `"text":"Hello world."`) || strings.Contains(body, `"word":" "`) {
		t.Fatalf("%d %s %+v", resp.StatusCode, body, resp.Usage)
	}
	if !p.SupportsModel(provider.Transcriptions, "scribe_v1") || p.SupportsModel(provider.Speech, "scribe_v1") || !p.SupportsModel(provider.Speech, "eleven_v3") {
		t.Error("SupportsModel")
	}
}

func TestError(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"detail":{"status":"invalid_api_key","message":"Invalid API key"}}`))
	}}
	resp, body := nativetest.Send(t, New(up.Start(t), http.DefaultClient), &provider.Request{Endpoint: provider.Speech, Model: "m", Body: []byte(`{"input":"x","voice":"v"}`)})
	if resp.StatusCode != 401 || !strings.Contains(body, `"message":"Invalid API key"`) || !strings.Contains(body, "authentication_error") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}
