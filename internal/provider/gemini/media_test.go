package gemini

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/provider"
)

func TestImagen(t *testing.T) {
	f := &fakeUpstream{status: 200, body: `{"predictions":[{"bytesBase64Encoded":"aQ==","mimeType":"image/png"},{"raiFilteredReason":"filtered"}]}`}
	p := f.start(t)
	resp, body := send(t, p, provider.ImageGenerations, "imagen-4.0-generate-001", false, `{"model":"imagen-4.0-generate-001","prompt":"a falcon","n":2,"size":"1536x1024"}`)
	if f.url != "/v1beta/models/imagen-4.0-generate-001:predict" || f.header.Get("X-Goog-Api-Key") != "goog-key" {
		t.Fatalf("request %s %v", f.url, f.header)
	}
	if want := `{"instances":[{"prompt":"a falcon"}],"parameters":{"aspectRatio":"4:3","sampleCount":2}}`; f.raw != want {
		t.Fatalf("body = %s, want %s", f.raw, want)
	}
	// Filtered images are left out and not counted.
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &out) != nil || len(out.Data) != 1 || out.Data[0].B64 != "aQ==" || resp.Usage.Images != 1 {
		t.Fatalf("answer %s, usage %+v", body, *resp.Usage)
	}
	for size, want := range map[string]string{"1024x1024": "1:1", "1024x1536": "3:4", "1792x1024": "16:9", "auto": "", "": ""} {
		if got := aspectRatio(size); got != want {
			t.Errorf("aspectRatio(%q) = %q, want %q", size, got, want)
		}
	}
}

func TestImagenError(t *testing.T) {
	f := &fakeUpstream{status: 400, body: `{"error":{"code":400,"message":"bad prompt","status":"INVALID_ARGUMENT"}}`}
	resp, body := send(t, f.start(t), provider.ImageGenerations, "imagen-4.0-generate-001", false, `{"prompt":"x"}`)
	if resp.StatusCode != 400 || !strings.Contains(body, `"message":"bad prompt"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestSpeech(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0}
	f := &fakeUpstream{status: 200, body: `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"` +
		base64.StdEncoding.EncodeToString(pcm) + `"}}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":30}}`}
	p := f.start(t)
	resp, body := send(t, p, provider.Speech, "gemini-2.5-flash-preview-tts", false, `{"model":"x","input":"héllo","voice":"Kore","instructions":"Say cheerfully"}`)
	if f.url != "/v1beta/models/gemini-2.5-flash-preview-tts:generateContent" {
		t.Fatalf("url %s", f.url)
	}
	if !strings.Contains(f.raw, `"voiceName":"Kore"`) || !strings.Contains(f.raw, `"responseModalities":["AUDIO"]`) || !strings.Contains(f.raw, `"text":"Say cheerfully: héllo"`) {
		t.Fatalf("body %s", f.raw)
	}
	if resp.Header.Get("Content-Type") != "audio/wav" || !strings.HasPrefix(body, "RIFF") || body[len(body)-len(pcm):] != string(pcm) {
		t.Fatalf("answer %q %v", body, resp.Header)
	}
	if rate := binary.LittleEndian.Uint32([]byte(body[24:28])); rate != pcmRate {
		t.Fatalf("sample rate %d", rate)
	}
	if u := *resp.Usage; u.Characters != 5 || u.InputTokens != 4 || u.OutputTokens != 30 {
		t.Fatalf("usage %+v", u)
	}
	resp, body = send(t, p, provider.Speech, "gemini-2.5-flash-preview-tts", false, `{"input":"hi","response_format":"pcm"}`)
	if resp.Header.Get("Content-Type") != "audio/pcm" || body != string(pcm) {
		t.Fatalf("pcm answer %q", body)
	}
	if resp, _ := send(t, p, provider.Speech, "gemini-2.5-flash-preview-tts", false, `{"input":"hi","response_format":"mp3"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mp3 = %d", resp.StatusCode)
	}
}

func TestMediaModelSupport(t *testing.T) {
	p := (&fakeUpstream{}).start(t)
	for _, c := range []struct {
		model string
		ep    provider.Endpoint
		want  bool
	}{
		{"imagen-4.0-generate-001", provider.ImageGenerations, true},
		{"imagen-4.0-generate-001", provider.ChatCompletions, false},
		{"gemini-2.5-flash-preview-tts", provider.Speech, true},
		{"gemini-2.5-flash-preview-tts", provider.ChatCompletions, false},
		{"gemini-2.5-flash", provider.ChatCompletions, true},
		{"gemini-2.5-flash", provider.ImageGenerations, false},
	} {
		if got := p.SupportsModel(c.ep, c.model); got != c.want {
			t.Errorf("SupportsModel(%s, %s) = %v", c.ep, c.model, got)
		}
	}
}
