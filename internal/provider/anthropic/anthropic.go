// Package anthropic adapts the Anthropic Messages API. It serves
// /v1/messages as passthrough and /v1/chat/completions by translation;
// docs/providers.md has the translation table.
package anthropic

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

// DefaultVersion is sent as anthropic-version when the client sends none.
const DefaultVersion = "2023-06-01"

// maxBody caps non-streaming responses held in memory.
const maxBody = 64 << 20

// Provider is an Anthropic upstream.
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
	return e == provider.Messages || e == provider.ChatCompletions
}

// Send implements provider.Provider.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if req.Endpoint == provider.ChatCompletions {
		return p.chat(ctx, req)
	}
	resp, err := p.post(ctx, req.Body, req.Stream, req.Header)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	if req.Stream {
		var acc streamUsage
		resp.Body = sse.Tap(resp.Body, func(data []byte) {
			acc.observe(data)
			*resp.Usage = acc.tokens()
		})
		return resp, nil
	}
	buf, err := readAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var m struct {
		Usage *anthUsage `json:"usage"`
	}
	if json.Unmarshal(buf, &m) == nil && m.Usage != nil {
		*resp.Usage = m.Usage.tokens()
	}
	resp.Body = io.NopCloser(bytes.NewReader(buf))
	return resp, nil
}

// post sends a Messages API request. Only anthropic-version and
// anthropic-beta are taken from the client.
func (p *Provider) post(ctx context.Context, body []byte, stream bool, clientHeader http.Header) (*provider.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Anthropic-Version", DefaultVersion)
	if v := clientHeader.Get("Anthropic-Version"); v != "" {
		hreq.Header.Set("Anthropic-Version", v)
	}
	for _, v := range clientHeader.Values("Anthropic-Beta") {
		hreq.Header.Add("Anthropic-Beta", v)
	}
	if stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	if key := p.cfg.APIKey.Value(); key != "" {
		hreq.Header.Set("X-Api-Key", key)
	}
	for k, v := range p.cfg.Headers {
		hreq.Header.Set(k, v)
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		cancel()
		return nil, err
	}
	return &provider.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       &cancelOnClose{ReadCloser: resp.Body, cancel: cancel},
		Usage:      &usage.Tokens{},
	}, nil
}

func readAll(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxBody))
}

// anthUsage is Anthropic's usage object. Input tokens exclude cache
// reads and writes, which are reported separately.
type anthUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (u anthUsage) tokens() usage.Tokens {
	return usage.Tokens{
		InputTokens:       u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		Reported:          true,
	}
}

// streamUsage accumulates usage across Messages stream events: input
// counts arrive in message_start, cumulative output in message_delta.
type streamUsage struct {
	u    anthUsage
	seen bool
}

func (s *streamUsage) observe(data []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage *anthUsage `json:"usage"`
		} `json:"message"`
		Usage *anthUsage `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch {
	case ev.Type == "message_start" && ev.Message.Usage != nil:
		s.u, s.seen = *ev.Message.Usage, true
	case ev.Type == "message_delta" && ev.Usage != nil:
		s.u.OutputTokens = ev.Usage.OutputTokens
		// Newer API versions also report final input counts here.
		if ev.Usage.InputTokens > 0 {
			s.u.InputTokens = ev.Usage.InputTokens
		}
		if ev.Usage.CacheReadInputTokens > 0 {
			s.u.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
		}
		if ev.Usage.CacheCreationInputTokens > 0 {
			s.u.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
		}
		s.seen = true
	}
}

func (s *streamUsage) tokens() usage.Tokens {
	if !s.seen {
		return usage.Tokens{}
	}
	return s.u.tokens()
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
