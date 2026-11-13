// Package native holds what the adapters for providers with their own
// APIs share: OpenAI-shaped errors and transcripts, reading upstream
// replies, and audio helpers.
package native

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// MaxBody caps an upstream reply read into memory.
const MaxBody = 64 << 20

// ReadAll reads and closes a reply body, up to MaxBody.
func ReadAll(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, MaxBody))
}

// Error returns an OpenAI-shaped error response.
func Error(status int, typ, msg string) *provider.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": typ}})
	return Body(status, "application/json", b, &usage.Tokens{})
}

// BadRequest is Error with status 400.
func BadRequest(format string, args ...any) *provider.Response {
	return Error(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(format, args...))
}

// BadGateway is Error with status 502, for upstream replies the
// gateway cannot read.
func BadGateway(msg string) *provider.Response {
	return Error(http.StatusBadGateway, "api_error", msg)
}

// Body returns a response with a body held in memory.
func Body(status int, contentType string, b []byte, t *usage.Tokens) *provider.Response {
	if t == nil {
		t = &usage.Tokens{}
	}
	return &provider.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Usage:      t,
	}
}

// JSON returns a 200 response with v as its JSON body.
func JSON(v any, t *usage.Tokens) *provider.Response {
	b, _ := json.Marshal(v)
	return Body(http.StatusOK, "application/json", b, t)
}

// TranslateError reads an upstream error reply and returns it in the
// OpenAI shape with the same status, so fallback rules still apply. It
// finds the message in the shapes providers use: error.message, error,
// detail.message, detail, message and err_msg.
func TranslateError(resp *http.Response) *provider.Response {
	buf, _ := ReadAll(resp.Body)
	return translate(resp, buf)
}

func translate(resp *http.Response, buf []byte) *provider.Response {
	msg := Message(buf)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	typ := "api_error"
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		typ = "authentication_error"
	case resp.StatusCode == http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		typ = "invalid_request_error"
	}
	out := Error(resp.StatusCode, typ, msg)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		out.Header.Set("Retry-After", ra)
	}
	return out
}

// Message finds an error message in an upstream error body.
func Message(buf []byte) string {
	var v map[string]json.RawMessage
	if json.Unmarshal(buf, &v) != nil {
		s := strings.TrimSpace(string(buf))
		if len(s) > 500 {
			s = s[:500]
		}
		return s
	}
	for _, k := range []string{"error", "detail", "message", "err_msg", "errors"} {
		if m := text(v[k]); m != "" {
			return m
		}
	}
	return ""
}

// text reads a message from a string, an object with a message, or a
// list of either.
func text(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(raw, &o) == nil {
		for _, m := range []string{o.Message, o.Msg, o.Detail} {
			if m != "" {
				return m
			}
		}
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var parts []string
		for _, item := range list {
			if m := text(item); m != "" {
				parts = append(parts, m)
			}
		}
		return strings.Join(parts, "; ")
	}
	return ""
}

// Fields reads the non-file fields of a multipart request, which the
// gateway passes as a JSON object of strings and lists of strings. A
// repeated field keeps its first value.
func Fields(body []byte) map[string]string {
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(body, &raw)
	out := map[string]string{}
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out[k] = s
			continue
		}
		var list []string
		if json.Unmarshal(v, &list) == nil && len(list) > 0 {
			out[k] = list[0]
		}
	}
	return out
}

// File returns the multipart file named field, or nil.
func File(files []provider.File, field string) *provider.File {
	for i := range files {
		if files[i].Field == field {
			return &files[i]
		}
	}
	return nil
}

// Word is a word of a transcript with its times in seconds.
type Word struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Transcript is a transcription in a provider-neutral form.
type Transcript struct {
	Text     string
	Language string
	// Duration is the length of the audio in seconds, as the upstream
	// reported or measured it.
	Duration float64
	Words    []Word
}

// TranscriptFormats are the OpenAI response formats Respond writes.
var TranscriptFormats = []string{"json", "text", "verbose_json", "srt", "vtt"}

// CheckTranscriptFormat returns a 400 response for a response_format
// Respond cannot write, or nil.
func CheckTranscriptFormat(format string) *provider.Response {
	if format == "" {
		return nil
	}
	for _, f := range TranscriptFormats {
		if f == format {
			return nil
		}
	}
	return BadRequest("response_format must be one of %s", strings.Join(TranscriptFormats, ", "))
}

// Respond writes a transcript in an OpenAI response format, metering
// its duration.
func (t Transcript) Respond(format string) *provider.Response {
	u := &usage.Tokens{AudioSeconds: t.Duration, Reported: true}
	switch format {
	case "text":
		return Body(http.StatusOK, "text/plain; charset=utf-8", []byte(t.Text+"\n"), u)
	case "verbose_json":
		words := t.Words
		if words == nil {
			words = []Word{}
		}
		return JSON(map[string]any{
			"task": "transcribe", "language": t.Language, "duration": t.Duration, "text": t.Text,
			"words":    words,
			"segments": []any{map[string]any{"id": 0, "start": 0, "end": t.Duration, "text": t.Text}},
		}, u)
	case "srt", "vtt":
		return Body(http.StatusOK, "text/plain; charset=utf-8", []byte(t.subtitles(format)), u)
	}
	return JSON(map[string]any{"text": t.Text, "usage": map[string]any{"type": "duration", "seconds": t.Duration}}, u)
}

// subtitles writes the transcript as SRT or WebVTT cues of up to ten
// words, or one cue for the whole text when there are no word times.
func (t Transcript) subtitles(format string) string {
	type cue struct {
		start, end float64
		text       string
	}
	var cues []cue
	for i := 0; i < len(t.Words); i += 10 {
		ws := t.Words[i:min(i+10, len(t.Words))]
		var parts []string
		for _, w := range ws {
			parts = append(parts, w.Word)
		}
		cues = append(cues, cue{ws[0].Start, ws[len(ws)-1].End, strings.Join(parts, " ")})
	}
	if len(cues) == 0 {
		cues = []cue{{0, t.Duration, t.Text}}
	}
	var b strings.Builder
	sep := ","
	if format == "vtt" {
		b.WriteString("WEBVTT\n\n")
		sep = "."
	}
	for i, c := range cues {
		if format == "srt" {
			fmt.Fprintf(&b, "%d\n", i+1)
		}
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", stamp(c.start, sep), stamp(c.end, sep), c.text)
	}
	return b.String()
}

func stamp(sec float64, sep string) string {
	ms := int64(sec*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", ms/3600000, ms/60000%60, ms/1000%60, sep, ms%1000)
}

// WAV wraps 16-bit little-endian mono PCM at rate in a WAV header.
func WAV(pcm []byte, rate int) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + len(pcm)))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(1))
	w(uint32(rate))
	w(uint32(rate * 2))
	w(uint16(2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

// AudioTypes are the content types of the OpenAI speech formats.
var AudioTypes = map[string]string{
	"mp3":  "audio/mpeg",
	"opus": "audio/ogg",
	"aac":  "audio/aac",
	"flac": "audio/flac",
	"wav":  "audio/wav",
	"pcm":  "audio/pcm",
}

// SpeechRequest is an OpenAI text to speech request.
type SpeechRequest struct {
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	Instructions   string  `json:"instructions"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
}

// ReadSpeech parses a speech request, defaulting the format to mp3.
func ReadSpeech(body []byte) (SpeechRequest, *provider.Response) {
	var sr SpeechRequest
	if json.Unmarshal(body, &sr) != nil || sr.Input == "" {
		return sr, BadRequest("input is required")
	}
	if sr.ResponseFormat == "" {
		sr.ResponseFormat = "mp3"
	}
	if _, ok := AudioTypes[sr.ResponseFormat]; !ok {
		return sr, BadRequest("response_format must be mp3, opus, aac, flac, wav or pcm")
	}
	return sr, nil
}

// Client calls one configured upstream.
type Client struct {
	Cfg  config.Provider
	HTTP *http.Client
	// Auth sets the upstream's credentials on a request.
	Auth func(http.Header)
}

// Do sends a request to a path below the base URL, or to an absolute
// URL, and reads the whole reply within the provider's timeout. The
// returned response's body is already read and closed.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Cfg.Timeout.Std())
	defer cancel()
	u := path
	if !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
		u = c.Cfg.BaseURL + path
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range c.Cfg.Headers {
		req.Header.Set(k, v)
	}
	if c.Auth != nil {
		c.Auth(req.Header)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	buf, err := ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return resp, buf, nil
}

// Failed returns the OpenAI-shaped error for a non-2xx reply, or nil.
func Failed(resp *http.Response, buf []byte) *provider.Response {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return translate(resp, buf)
}
