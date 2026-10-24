package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rnikadi/genaiopengateway/internal/provider"
)

// imagenRatios are the aspect ratios Imagen makes.
var imagenRatios = []string{"1:1", "3:4", "4:3", "9:16", "16:9"}

// aspectRatio returns the Imagen aspect ratio nearest to an OpenAI size
// such as 1536x1024, or "" for auto and sizes it cannot read.
func aspectRatio(size string) string {
	w, h, ok := strings.Cut(size, "x")
	wf, err1 := strconv.ParseFloat(w, 64)
	hf, err2 := strconv.ParseFloat(h, 64)
	if !ok || err1 != nil || err2 != nil || wf <= 0 || hf <= 0 {
		return ""
	}
	best, diff := "", math.Inf(1)
	for _, r := range imagenRatios {
		a, b, _ := strings.Cut(r, ":")
		af, _ := strconv.ParseFloat(a, 64)
		bf, _ := strconv.ParseFloat(b, 64)
		if d := math.Abs(math.Log(wf/hf) - math.Log(af/bf)); d < diff {
			best, diff = r, d
		}
	}
	return best
}

// images serves OpenAI image generation with Imagen's predict method.
func (p *Provider) images(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	var ir struct {
		Prompt string `json:"prompt"`
		N      int    `json:"n"`
		Size   string `json:"size"`
	}
	if json.Unmarshal(req.Body, &ir) != nil || ir.Prompt == "" {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "prompt is required"), nil
	}
	params := map[string]any{"sampleCount": max(ir.N, 1)}
	if r := aspectRatio(ir.Size); r != "" {
		params["aspectRatio"] = r
	}
	body, _ := json.Marshal(map[string]any{"instances": []any{map[string]string{"prompt": ir.Prompt}}, "parameters": params})
	resp, err := p.call(ctx, req.Model, "predict", body, false)
	if err != nil || resp.StatusCode != http.StatusOK {
		if err == nil {
			return translateError(resp)
		}
		return resp, err
	}
	buf, err := readAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var pr struct {
		Predictions []struct {
			Bytes    string `json:"bytesBase64Encoded"`
			MimeType string `json:"mimeType"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal(buf, &pr); err != nil {
		return errorResponse(http.StatusBadGateway, "api_error", "the upstream answer is not an Imagen prediction"), nil
	}
	data := []map[string]string{}
	format := "png"
	for _, pred := range pr.Predictions {
		// Images filtered for safety come back without bytes.
		if pred.Bytes == "" {
			continue
		}
		data = append(data, map[string]string{"b64_json": pred.Bytes})
		if f, ok := strings.CutPrefix(pred.MimeType, "image/"); ok {
			format = f
		}
	}
	out, _ := json.Marshal(map[string]any{"created": time.Now().Unix(), "data": data, "output_format": format})
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.Header = http.Header{"Content-Type": {"application/json"}}
	resp.Usage.Images = int64(len(data))
	return resp, nil
}

// pcmRate is the sample rate of Gemini's speech: 16-bit mono PCM.
const pcmRate = 24000

// speech serves OpenAI text to speech with a Gemini TTS model.
func (p *Provider) speech(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	var sr struct {
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		Instructions   string `json:"instructions"`
		ResponseFormat string `json:"response_format"`
	}
	if json.Unmarshal(req.Body, &sr) != nil || sr.Input == "" {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "input is required"), nil
	}
	format := sr.ResponseFormat
	if format == "" {
		format = "wav"
	}
	if format != "wav" && format != "pcm" {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "this model answers in wav or pcm"), nil
	}
	text := sr.Input
	if sr.Instructions != "" {
		// Gemini takes style directions in the prompt.
		text = sr.Instructions + ": " + sr.Input
	}
	gen := map[string]any{"responseModalities": []string{"AUDIO"}}
	if sr.Voice != "" {
		gen["speechConfig"] = map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]string{"voiceName": sr.Voice}}}
	}
	body, _ := json.Marshal(map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": text}}}},
		"generationConfig": gen,
	})
	resp, err := p.post(ctx, req.Model, body, false)
	if err != nil || resp.StatusCode != http.StatusOK {
		if err == nil {
			return translateError(resp)
		}
		return resp, err
	}
	buf, err := readAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var gr struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						Data string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	_ = json.Unmarshal(buf, &gr)
	var pcm []byte
	for _, c := range gr.Candidates {
		for _, part := range c.Content.Parts {
			if part.InlineData != nil {
				b, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err != nil {
					return errorResponse(http.StatusBadGateway, "api_error", "the upstream audio is not valid base64"), nil
				}
				pcm = append(pcm, b...)
			}
		}
	}
	if len(pcm) == 0 {
		return errorResponse(http.StatusBadGateway, "api_error", "the upstream answered without audio"), nil
	}
	if t, ok := tokensOf(buf); ok {
		*resp.Usage = t
	}
	resp.Usage.Characters = int64(utf8.RuneCountInString(sr.Input))
	contentType := "audio/pcm"
	if format == "wav" {
		pcm, contentType = wav(pcm), "audio/wav"
	}
	resp.Header = http.Header{"Content-Type": {contentType}}
	resp.Body = io.NopCloser(bytes.NewReader(pcm))
	return resp, nil
}

// wav wraps 16-bit mono PCM at pcmRate in a WAV header.
func wav(pcm []byte) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + len(pcm)))
	b.WriteString("WAVEfmt ")
	w(uint32(16))          // fmt chunk size
	w(uint16(1))           // PCM
	w(uint16(1))           // mono
	w(uint32(pcmRate))     // sample rate
	w(uint32(pcmRate * 2)) // byte rate
	w(uint16(2))           // block align
	w(uint16(16))          // bits per sample
	b.WriteString("data")
	w(uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}
