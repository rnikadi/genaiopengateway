// Package deepgram adapts the Deepgram API: pre-recorded speech to text
// and Aura text to speech, served as OpenAI's audio endpoints.
package deepgram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// encodings maps OpenAI speech formats to Deepgram's encoding and
// container query parameters.
var encodings = map[string]url.Values{
	"mp3":  {"encoding": {"mp3"}},
	"opus": {"encoding": {"opus"}, "container": {"ogg"}},
	"aac":  {"encoding": {"aac"}},
	"flac": {"encoding": {"flac"}},
	"wav":  {"encoding": {"linear16"}, "container": {"wav"}, "sample_rate": {"24000"}},
	"pcm":  {"encoding": {"linear16"}, "container": {"none"}, "sample_rate": {"24000"}},
}

// Provider is a Deepgram upstream.
type Provider struct{ c native.Client }

// New returns an adapter for cfg.
func New(cfg config.Provider, client *http.Client) *Provider {
	return &Provider{c: native.Client{Cfg: cfg, HTTP: client, Auth: func(h http.Header) {
		if key := cfg.APIKey.Value(); key != "" {
			h.Set("Authorization", "Token "+key)
		}
	}}}
}

// Supports implements provider.Provider.
func (p *Provider) Supports(e provider.Endpoint) bool {
	return e == provider.Speech || e == provider.Transcriptions
}

// SupportsModel implements provider.ModelSupporter: Aura models speak,
// and the others transcribe.
func (p *Provider) SupportsModel(e provider.Endpoint, model string) bool {
	if strings.HasPrefix(model, "aura") {
		return e == provider.Speech
	}
	return e == provider.Transcriptions
}

// Send implements provider.Provider.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	switch req.Endpoint {
	case provider.Speech:
		return p.speech(ctx, req)
	case provider.Transcriptions:
		return p.transcribe(ctx, req)
	}
	return native.Error(http.StatusNotFound, "invalid_request_error", "Deepgram does not serve "+string(req.Endpoint)), nil
}

// speech serves OpenAI text to speech with Aura. The model names the
// voice, such as aura-2-thalia-en, so voice is not used.
func (p *Provider) speech(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	sr, bad := native.ReadSpeech(req.Body)
	if bad != nil {
		return bad, nil
	}
	q := url.Values{"model": {req.Model}}
	for k, v := range encodings[sr.ResponseFormat] {
		q[k] = v
	}
	b, _ := json.Marshal(map[string]string{"text": sr.Input})
	resp, buf, err := p.c.Do(ctx, http.MethodPost, "/v1/speak?"+q.Encode(), b, "application/json")
	if err != nil {
		return nil, err
	}
	if bad := native.Failed(resp, buf); bad != nil {
		return bad, nil
	}
	t := &usage.Tokens{Characters: int64(len([]rune(sr.Input))), Reported: true}
	return native.Body(http.StatusOK, native.AudioTypes[sr.ResponseFormat], buf, t), nil
}

// transcribe serves OpenAI transcriptions with a pre-recorded request.
func (p *Provider) transcribe(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	f := native.Fields(req.Body)
	if bad := native.CheckTranscriptFormat(f["response_format"]); bad != nil {
		return bad, nil
	}
	file := native.File(req.Files, "file")
	if file == nil {
		return native.BadRequest("file is required"), nil
	}
	q := url.Values{"model": {req.Model}, "smart_format": {"true"}}
	if f["language"] != "" {
		q.Set("language", f["language"])
	} else {
		q.Set("detect_language", "true")
	}
	ctype := file.ContentType
	if ctype == "" || ctype == "application/octet-stream" {
		ctype = "audio/*"
	}
	resp, buf, err := p.c.Do(ctx, http.MethodPost, "/v1/listen?"+q.Encode(), file.Data, ctype)
	if err != nil {
		return nil, err
	}
	if bad := native.Failed(resp, buf); bad != nil {
		return bad, nil
	}
	var lr struct {
		Metadata struct {
			Duration float64 `json:"duration"`
		} `json:"metadata"`
		Results struct {
			Channels []struct {
				DetectedLanguage string `json:"detected_language"`
				Alternatives     []struct {
					Transcript string `json:"transcript"`
					Words      []struct {
						Word       string  `json:"word"`
						Punctuated string  `json:"punctuated_word"`
						Start      float64 `json:"start"`
						End        float64 `json:"end"`
					} `json:"words"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if json.Unmarshal(buf, &lr) != nil || len(lr.Results.Channels) == 0 || len(lr.Results.Channels[0].Alternatives) == 0 {
		return native.BadGateway("the upstream answer is not a Deepgram transcript"), nil
	}
	ch := lr.Results.Channels[0]
	alt := ch.Alternatives[0]
	t := native.Transcript{Text: alt.Transcript, Language: f["language"], Duration: lr.Metadata.Duration}
	if t.Language == "" {
		t.Language = ch.DetectedLanguage
	}
	for _, w := range alt.Words {
		word := w.Punctuated
		if word == "" {
			word = w.Word
		}
		t.Words = append(t.Words, native.Word{Word: word, Start: w.Start, End: w.End})
	}
	return t.Respond(f["response_format"]), nil
}
