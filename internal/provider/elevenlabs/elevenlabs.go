// Package elevenlabs adapts the ElevenLabs API: text to speech and
// Scribe speech to text, served as OpenAI's audio endpoints.
package elevenlabs

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// pcmRate is the sample rate asked for pcm and wav, OpenAI's 24 kHz.
const pcmRate = 24000

// formats maps OpenAI speech formats to ElevenLabs output formats. wav
// is pcm with a header added here.
var formats = map[string]string{
	"mp3":  "mp3_44100_128",
	"opus": "opus_48000_128",
	"pcm":  "pcm_24000",
	"wav":  "pcm_24000",
}

// Provider is an ElevenLabs upstream.
type Provider struct{ c native.Client }

// New returns an adapter for cfg.
func New(cfg config.Provider, client *http.Client) *Provider {
	return &Provider{c: native.Client{Cfg: cfg, HTTP: client, Auth: func(h http.Header) {
		if key := cfg.APIKey.Value(); key != "" {
			h.Set("xi-api-key", key)
		}
	}}}
}

// Supports implements provider.Provider.
func (p *Provider) Supports(e provider.Endpoint) bool {
	return e == provider.Speech || e == provider.Transcriptions
}

// SupportsModel implements provider.ModelSupporter: Scribe models
// transcribe, and the others speak.
func (p *Provider) SupportsModel(e provider.Endpoint, model string) bool {
	if isScribe(model) {
		return e == provider.Transcriptions
	}
	return e == provider.Speech
}

func isScribe(model string) bool { return len(model) >= 6 && model[:6] == "scribe" }

// Send implements provider.Provider.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	switch req.Endpoint {
	case provider.Speech:
		return p.speech(ctx, req)
	case provider.Transcriptions:
		return p.transcribe(ctx, req)
	}
	return native.Error(http.StatusNotFound, "invalid_request_error", "ElevenLabs does not serve "+string(req.Endpoint)), nil
}

// speech serves OpenAI text to speech. voice is an ElevenLabs voice ID.
func (p *Provider) speech(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	sr, bad := native.ReadSpeech(req.Body)
	if bad != nil {
		return bad, nil
	}
	format, ok := formats[sr.ResponseFormat]
	if !ok {
		return native.BadRequest("ElevenLabs answers in mp3, opus, wav or pcm"), nil
	}
	if sr.Voice == "" {
		return native.BadRequest("voice is required: an ElevenLabs voice ID"), nil
	}
	body := map[string]any{"text": sr.Input, "model_id": req.Model}
	if sr.Speed > 0 {
		body["voice_settings"] = map[string]any{"speed": sr.Speed}
	}
	b, _ := json.Marshal(body)
	path := "/v1/text-to-speech/" + url.PathEscape(sr.Voice) + "?output_format=" + format
	resp, buf, err := p.c.Do(ctx, http.MethodPost, path, b, "application/json")
	if err != nil {
		return nil, err
	}
	if bad := native.Failed(resp, buf); bad != nil {
		return bad, nil
	}
	if sr.ResponseFormat == "wav" {
		buf = native.WAV(buf, pcmRate)
	}
	t := &usage.Tokens{Characters: int64(len([]rune(sr.Input))), Reported: true}
	return native.Body(http.StatusOK, native.AudioTypes[sr.ResponseFormat], buf, t), nil
}

// transcribe serves OpenAI transcriptions with Scribe.
func (p *Provider) transcribe(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	f := native.Fields(req.Body)
	if bad := native.CheckTranscriptFormat(f["response_format"]); bad != nil {
		return bad, nil
	}
	file := native.File(req.Files, "file")
	if file == nil {
		return native.BadRequest("file is required"), nil
	}
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("model_id", req.Model)
	if f["language"] != "" {
		_ = w.WriteField("language_code", f["language"])
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", multipart.FileContentDisposition("file", file.Filename))
	if file.ContentType != "" {
		h.Set("Content-Type", file.ContentType)
	}
	part, _ := w.CreatePart(h)
	_, _ = part.Write(file.Data)
	_ = w.Close()
	resp, buf, err := p.c.Do(ctx, http.MethodPost, "/v1/speech-to-text", b.Bytes(), w.FormDataContentType())
	if err != nil {
		return nil, err
	}
	if bad := native.Failed(resp, buf); bad != nil {
		return bad, nil
	}
	var sr struct {
		Text     string `json:"text"`
		Language string `json:"language_code"`
		Words    []struct {
			Text  string  `json:"text"`
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Type  string  `json:"type"`
		} `json:"words"`
	}
	if json.Unmarshal(buf, &sr) != nil {
		return native.BadGateway("the upstream answer is not an ElevenLabs transcript"), nil
	}
	t := native.Transcript{Text: sr.Text, Language: sr.Language}
	for _, wd := range sr.Words {
		// Scribe bills the audio's length; the last word's end is the
		// nearest measure it returns.
		t.Duration = max(t.Duration, wd.End)
		if wd.Type == "word" {
			t.Words = append(t.Words, native.Word{Word: wd.Text, Start: wd.Start, End: wd.End})
		}
	}
	return t.Respond(f["response_format"]), nil
}
