// Package anthropic adapts the Anthropic Messages API. It serves
// /v1/messages as passthrough and /v1/chat/completions by translation;
// docs/providers.md has the translation table.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

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
	up     Upstream
}

// Upstream adapts the Messages API as a cloud serves it, such as Claude
// on Vertex AI or Bedrock. The zero value is the Anthropic API.
type Upstream struct {
	// URL returns where to send a request for model.
	URL func(model string, stream bool) string
	// Version, when set, is sent in the body as anthropic_version, the
	// model is removed from the body, and the anthropic-version header is
	// not sent.
	Version string
	// BetasInBody sends the client's anthropic-beta headers as
	// anthropic_beta in the body, as Bedrock expects.
	BetasInBody bool
	// Authorize sets credentials on an upstream request, after every
	// other header; a signer may read the body through GetBody. A
	// failure is returned to the client as a 502, which allows fallback.
	Authorize func(r *http.Request) error
	// Decode turns a streamed response into Anthropic server-sent events.
	Decode func(io.ReadCloser) io.ReadCloser
}

// New returns an adapter for cfg.
func New(cfg config.Provider, client *http.Client) *Provider {
	return &Provider{cfg: cfg, client: client}
}

// NewUpstream returns an adapter for Claude served by a cloud.
func NewUpstream(cfg config.Provider, client *http.Client, up Upstream) *Provider {
	return &Provider{cfg: cfg, client: client, up: up}
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
	url := p.cfg.BaseURL + "/v1/messages"
	if p.up.URL != nil {
		var model string
		var err error
		var betas []string
		if p.up.BetasInBody {
			betas = clientHeader.Values("Anthropic-Beta")
		}
		if body, model, err = cloudBody(body, p.up.Version, betas); err != nil {
			return upstreamError(http.StatusBadRequest, "invalid_request_error", err.Error()), nil
		}
		url = p.up.URL(model, stream)
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if p.up.Version == "" {
		hreq.Header.Set("Anthropic-Version", DefaultVersion)
		if v := clientHeader.Get("Anthropic-Version"); v != "" {
			hreq.Header.Set("Anthropic-Version", v)
		}
	}
	if !p.up.BetasInBody {
		for _, v := range clientHeader.Values("Anthropic-Beta") {
			hreq.Header.Add("Anthropic-Beta", v)
		}
	}
	if stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range p.cfg.Headers {
		hreq.Header.Set(k, v)
	}
	if p.up.Authorize != nil {
		if err := p.up.Authorize(hreq); err != nil {
			cancel()
			return upstreamError(http.StatusBadGateway, "api_error", "upstream credentials: "+err.Error()), nil
		}
	} else if key := p.cfg.APIKey.Value(); key != "" {
		hreq.Header.Set("X-Api-Key", key)
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		cancel()
		return nil, err
	}
	out := &provider.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       &cancelOnClose{ReadCloser: resp.Body, cancel: cancel},
		Usage:      &usage.Tokens{},
	}
	if stream && resp.StatusCode == http.StatusOK && p.up.Decode != nil {
		out.Body = p.up.Decode(out.Body)
		out.Header = out.Header.Clone()
		out.Header.Set("Content-Type", "text/event-stream")
	}
	return out, nil
}

// cloudBody moves the model out of a Messages body and sets
// anthropic_version, as Vertex AI and Bedrock expect.
func cloudBody(body []byte, version string, betas []string) ([]byte, string, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil, "", errors.New("request body must be a JSON object")
	}
	var model string
	_ = json.Unmarshal(m["model"], &model)
	delete(m, "model")
	if version != "" {
		m["anthropic_version"], _ = json.Marshal(version)
	}
	if len(betas) > 0 {
		var list []string
		for _, b := range betas {
			for v := range strings.SplitSeq(b, ",") {
				if v = strings.TrimSpace(v); v != "" {
					list = append(list, v)
				}
			}
		}
		m["anthropic_beta"], _ = json.Marshal(list)
	}
	out, err := json.Marshal(m)
	return out, model, err
}

// upstreamError is an error response in the Messages API shape, for
// failures before the upstream answered.
func upstreamError(status int, typ, msg string) *provider.Response {
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
	return &provider.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Usage:      &usage.Tokens{},
	}
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
