// Package openai adapts any OpenAI-compatible API: OpenAI, Azure OpenAI
// (v1 API), Gemini's OpenAI endpoint, vLLM, Ollama and others.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/sse"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// maxBufferedBody caps how much of a non-streaming response is held in
// memory to read usage. Larger bodies pass through unmetered.
const maxBufferedBody = 64 << 20

var paths = map[provider.Endpoint]string{
	provider.ChatCompletions: "/chat/completions",
	provider.Embeddings:      "/embeddings",
	provider.Responses:       "/responses",
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
	body := req.Body
	if req.Stream && req.Endpoint == provider.ChatCompletions {
		body = withStreamUsage(body)
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+paths[req.Endpoint], bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	if key := p.cfg.APIKey.Value(); key != "" {
		hreq.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range p.cfg.Headers {
		hreq.Header.Set(k, v)
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
	if req.Stream {
		out.Body = sse.Tap(rc, func(data []byte) {
			if t, ok := usage.FromOpenAI(data); ok {
				*out.Usage = t
			}
		})
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
	if t, ok := usage.FromOpenAI(buf); ok {
		*out.Usage = t
	}
	out.Body = io.NopCloser(bytes.NewReader(buf))
	return out, nil
}

// withStreamUsage asks the upstream to send a final usage chunk, unless
// the client already chose. Bodies that are not JSON objects are left
// for the upstream to reject.
func withStreamUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	var opts map[string]json.RawMessage
	if raw, ok := m["stream_options"]; ok && json.Unmarshal(raw, &opts) != nil {
		return body
	}
	if opts == nil {
		opts = map[string]json.RawMessage{}
	}
	if _, set := opts["include_usage"]; set {
		return body
	}
	opts["include_usage"] = json.RawMessage("true")
	m["stream_options"], _ = json.Marshal(opts)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
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
