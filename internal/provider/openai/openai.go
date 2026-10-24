// Package openai adapts any OpenAI-compatible API: OpenAI, Azure OpenAI
// (v1 API), Gemini's OpenAI endpoint, vLLM, Ollama and others.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/sse"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// maxBufferedBody caps how much of a non-streaming response is held in
// memory to read usage. Larger bodies pass through unmetered.
const maxBufferedBody = 64 << 20

var paths = map[provider.Endpoint]string{
	provider.ChatCompletions:  "/chat/completions",
	provider.Embeddings:       "/embeddings",
	provider.Responses:        "/responses",
	provider.ImageGenerations: "/images/generations",
	provider.ImageEdits:       "/images/edits",
	provider.Speech:           "/audio/speech",
	provider.Transcriptions:   "/audio/transcriptions",
	provider.Translations:     "/audio/translations",
	provider.Moderations:      "/moderations",
	provider.Rerank:           "/rerank",
	provider.Videos:           "/videos",
	provider.VideoRemix:       "/videos/{id}/remix",
}

// Provider is an OpenAI-compatible upstream.
type Provider struct {
	cfg    config.Provider
	client *http.Client
}

// New returns an adapter for cfg.
func New(cfg config.Provider, client *http.Client) *Provider {
	return &Provider{cfg: cfg, client: client}
}

// Supports implements provider.Provider.
func (p *Provider) Supports(e provider.Endpoint) bool {
	_, ok := paths[e]
	return ok
}

// Send implements provider.Provider.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	body, injected, contentType := req.Body, false, "application/json"
	if req.Stream && req.Endpoint == provider.ChatCompletions {
		body, injected = withStreamUsage(body)
	}
	if req.Endpoint == provider.VideoRemix {
		// A remix takes the original job's model; the field is refused.
		body = withoutModel(body)
	}
	if req.Files != nil {
		var err error
		if body, contentType, err = encodeMultipart(req.Body, req.Files); err != nil {
			return nil, err
		}
	}
	path := strings.Replace(paths[req.Endpoint], "{id}", url.PathEscape(req.JobID), 1)
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	hreq, err := p.newRequest(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	hreq.Header.Set("Content-Type", contentType)
	if req.Stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := p.client.Do(hreq)
	if err != nil {
		cancel()
		return nil, err
	}
	out := &provider.Response{StatusCode: resp.StatusCode, Header: resp.Header, Usage: &usage.Tokens{}}
	rc := &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

	if resp.StatusCode != http.StatusOK {
		out.Body = rc
		return out, nil
	}
	if req.Endpoint == provider.Speech {
		out.Usage.Characters = characters(req.Body)
	}
	mediaType := resp.Header.Get("Content-Type")
	if req.Stream || strings.HasPrefix(mediaType, "text/event-stream") {
		limit := 1 << 20
		if req.Endpoint == provider.ImageGenerations || req.Endpoint == provider.ImageEdits {
			limit = maxImageEvent
		}
		out.Body = sse.TapLimit(rc, func(data []byte) { readUsage(req.Endpoint, data, out.Usage) }, limit)
		if injected {
			// The client did not ask for the usage chunk; code that
			// reads choices[0] of every chunk would fail on it.
			out.Body = sse.Drop(out.Body, usageOnlyChunk)
		}
		return out, nil
	}
	if req.Endpoint == provider.Speech || ((req.Endpoint == provider.Transcriptions || req.Endpoint == provider.Translations) && !strings.Contains(mediaType, "json")) {
		// Audio, and transcripts as text: nothing to read usage from.
		out.Body = rc
		return out, nil
	}
	// Buffer non-streaming bodies to read usage before replying.
	buf, err := io.ReadAll(io.LimitReader(rc, maxBufferedBody+1))
	if err != nil {
		_ = rc.Close()
		return nil, err
	}
	if len(buf) > maxBufferedBody {
		out.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(buf), rc), rc}
		return out, nil
	}
	_ = rc.Close()
	readUsage(req.Endpoint, buf, out.Usage)
	out.Body = io.NopCloser(bytes.NewReader(buf))
	return out, nil
}

// Forward implements provider.Forwarder.
func (p *Provider) Forward(ctx context.Context, method, path string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	hreq, err := p.newRequest(ctx, method, path, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// newRequest builds an upstream request with the provider's key and
// headers.
func (p *Provider) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	hreq, err := http.NewRequestWithContext(ctx, method, p.cfg.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if key := p.cfg.APIKey.Value(); key != "" {
		hreq.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range p.cfg.Headers {
		hreq.Header.Set(k, v)
	}
	return hreq, nil
}

// withoutModel removes "model" from a JSON object body.
func withoutModel(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	delete(m, "model")
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// withStreamUsage asks the upstream to send a final usage chunk, unless
// the client already chose, and reports whether it did. Bodies that are
// not JSON objects are left for the upstream to reject.
func withStreamUsage(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	var opts map[string]json.RawMessage
	if raw, ok := m["stream_options"]; ok && json.Unmarshal(raw, &opts) != nil {
		return body, false
	}
	if opts == nil {
		opts = map[string]json.RawMessage{}
	}
	if _, set := opts["include_usage"]; set {
		return body, false
	}
	opts["include_usage"] = json.RawMessage("true")
	m["stream_options"], _ = json.Marshal(opts)
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// usageOnlyChunk matches the final chunk include_usage adds: usage with
// no choices.
func usageOnlyChunk(data []byte) bool {
	var c struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	return json.Unmarshal(data, &c) == nil && len(c.Choices) == 0 && len(c.Usage) > 0 && string(c.Usage) != "null"
}

// cancelOnClose releases the request context when the body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
