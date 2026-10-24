package openai

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/textproto"
	"strings"
	"unicode/utf8"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// maxImageEvent bounds the stream events read for usage on image
// endpoints, whose completed events carry the whole image.
const maxImageEvent = 64 << 20

// encodeMultipart writes a multipart request from its fields and files.
// Fields are JSON strings or lists of them, as the gateway read them;
// other JSON values are written as their JSON text.
func encodeMultipart(body []byte, files []provider.File) ([]byte, string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for name, raw := range fields {
		for _, v := range fieldValues(raw) {
			if err := w.WriteField(name, v); err != nil {
				return nil, "", err
			}
		}
	}
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", multipart.FileContentDisposition(f.Field, f.Filename))
		if f.ContentType != "" {
			h.Set("Content-Type", f.ContentType)
		}
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(f.Data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

func fieldValues(raw json.RawMessage) []string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var out []string
		for _, item := range list {
			out = append(out, fieldValues(item)...)
		}
		return out
	}
	return []string{string(raw)}
}

// characters counts the characters of a speech request's input.
func characters(body []byte) int64 {
	var v struct {
		Input string `json:"input"`
	}
	_ = json.Unmarshal(body, &v)
	return int64(utf8.RuneCountInString(v.Input))
}

// mediaUsage adds what a JSON response or stream event says about media
// units to t: images returned, and seconds of audio transcribed.
func mediaUsage(ep provider.Endpoint, data []byte, t *usage.Tokens) {
	switch ep {
	case provider.ImageGenerations, provider.ImageEdits:
		var v struct {
			Type string            `json:"type"`
			Data []json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &v) != nil {
			return
		}
		t.Images += int64(len(v.Data))
		// image_generation.completed and image_edit.completed carry one
		// final image each.
		if strings.HasPrefix(v.Type, "image_") && strings.HasSuffix(v.Type, ".completed") {
			t.Images++
		}
	case provider.Transcriptions, provider.Translations:
		var v struct {
			Duration float64 `json:"duration"`
			Usage    *struct {
				Type    string  `json:"type"`
				Seconds float64 `json:"seconds"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &v) != nil {
			return
		}
		switch {
		case v.Usage != nil && v.Usage.Type == "duration":
			t.AudioSeconds = v.Usage.Seconds
		case v.Duration > 0:
			t.AudioSeconds = v.Duration
		}
	}
}

// readUsage reads tokens and media units from a JSON body or event,
// keeping counts already gathered from the request or earlier events.
func readUsage(ep provider.Endpoint, data []byte, t *usage.Tokens) {
	if tok, ok := usage.FromOpenAI(data); ok {
		t.InputTokens, t.OutputTokens = tok.InputTokens, tok.OutputTokens
		t.CachedInputTokens, t.ReasoningTokens = tok.CachedInputTokens, tok.ReasoningTokens
		t.Reported = true
	}
	mediaUsage(ep, data, t)
}
