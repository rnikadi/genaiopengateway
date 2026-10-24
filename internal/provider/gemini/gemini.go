// Package gemini adapts Google's generateContent API, on the Gemini API
// and on Vertex AI. It serves /v1/chat/completions by translation and
// generateContent by passthrough; docs/cloud-providers.md has the
// translation table.
package gemini

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

// maxBody caps non-streaming responses held in memory.
const maxBody = 64 << 20

// Provider is a generateContent upstream.
type Provider struct {
	cfg    config.Provider
	client *http.Client
	up     Upstream
}

// Upstream says where and how requests are sent. The zero value is the
// Gemini API with the provider's API key.
type Upstream struct {
	// URL returns the URL of a model's method, generateContent or
	// streamGenerateContent.
	URL func(model, method string) string
	// Authorize sets credentials on an upstream request, after every
	// other header; a signer may read the body through GetBody. A
	// failure is returned to the client as a 502, which allows fallback.
	Authorize func(r *http.Request) error
}

// New returns an adapter for the Gemini API.
func New(cfg config.Provider, client *http.Client) *Provider {
	return &Provider{cfg: cfg, client: client}
}

// NewUpstream returns an adapter for generateContent served elsewhere,
// such as Vertex AI.
func NewUpstream(cfg config.Provider, client *http.Client, up Upstream) *Provider {
	return &Provider{cfg: cfg, client: client, up: up}
}

// Supports implements provider.Provider.
func (p *Provider) Supports(e provider.Endpoint) bool {
	return e == provider.ChatCompletions || e == provider.GenerateContent || e == provider.ImageGenerations || e == provider.Speech
}

// SupportsModel implements provider.ModelSupporter: Imagen models make
// images, text to speech models speak, and other models chat.
func (p *Provider) SupportsModel(e provider.Endpoint, model string) bool {
	switch {
	case isImagen(model):
		return e == provider.ImageGenerations
	case isTTS(model):
		return e == provider.Speech
	}
	return e == provider.ChatCompletions || e == provider.GenerateContent
}

func isImagen(model string) bool {
	return strings.HasPrefix(strings.TrimPrefix(model, "models/"), "imagen-")
}

func isTTS(model string) bool { return strings.HasSuffix(model, "-tts") }

// Send implements provider.Provider.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	switch req.Endpoint {
	case provider.ChatCompletions:
		return p.chat(ctx, req)
	case provider.ImageGenerations:
		return p.images(ctx, req)
	case provider.Speech:
		return p.speech(ctx, req)
	}
	// The gateway adds model and stream to the body; Google's API takes
	// them from the URL.
	var m map[string]json.RawMessage
	if json.Unmarshal(req.Body, &m) != nil {
		return upstreamError(http.StatusBadRequest, "request body must be a JSON object"), nil
	}
	delete(m, "model")
	delete(m, "stream")
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	resp, err := p.post(ctx, req.Model, body, req.Stream)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	if req.Stream {
		resp.Body = sse.Tap(resp.Body, func(data []byte) {
			if t, ok := tokensOf(data); ok {
				*resp.Usage = t
			}
		})
		return resp, nil
	}
	buf, err := readAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if t, ok := tokensOf(buf); ok {
		*resp.Usage = t
	}
	resp.Body = io.NopCloser(bytes.NewReader(buf))
	return resp, nil
}

// post sends a generateContent request, as server-sent events when
// streaming.
func (p *Provider) post(ctx context.Context, model string, body []byte, stream bool) (*provider.Response, error) {
	method := "generateContent"
	if stream {
		method = "streamGenerateContent"
	}
	return p.call(ctx, model, method, body, stream)
}

// call sends a request to a model's method.
func (p *Provider) call(ctx context.Context, model, method string, body []byte, stream bool) (*provider.Response, error) {
	u := p.cfg.BaseURL + "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(model, "models/")) + ":" + method
	if p.up.URL != nil {
		u = p.up.URL(model, method)
	}
	if stream {
		u += "?alt=sse"
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range p.cfg.Headers {
		hreq.Header.Set(k, v)
	}
	if p.up.Authorize != nil {
		if err := p.up.Authorize(hreq); err != nil {
			cancel()
			return upstreamError(http.StatusBadGateway, "upstream credentials: "+err.Error()), nil
		}
	} else if key := p.cfg.APIKey.Value(); key != "" {
		hreq.Header.Set("X-Goog-Api-Key", key)
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

// usageMetadata is Gemini's usage. Thinking tokens are billed as output
// but reported apart from candidate tokens.
type usageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
}

func (u usageMetadata) tokens() usage.Tokens {
	return usage.Tokens{
		InputTokens:       u.PromptTokenCount + u.ToolUsePromptTokenCount,
		OutputTokens:      u.CandidatesTokenCount + u.ThoughtsTokenCount,
		CachedInputTokens: u.CachedContentTokenCount,
		ReasoningTokens:   u.ThoughtsTokenCount,
		Reported:          true,
	}
}

// tokensOf reads usageMetadata from a response or stream chunk. Each
// chunk reports the totals so far, so the last one wins.
func tokensOf(data []byte) (usage.Tokens, bool) {
	var r struct {
		UsageMetadata *usageMetadata `json:"usageMetadata"`
	}
	if json.Unmarshal(data, &r) != nil || r.UsageMetadata == nil {
		return usage.Tokens{}, false
	}
	return r.UsageMetadata.tokens(), true
}

func readAll(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxBody))
}

// googleError is the error body of Google APIs.
type googleError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// upstreamError is an error response in Google's shape, for failures
// before the upstream answered.
func upstreamError(status int, msg string) *provider.Response {
	var e googleError
	e.Error.Code, e.Error.Message, e.Error.Status = status, msg, "UNAVAILABLE"
	if status == http.StatusBadRequest {
		e.Error.Status = "INVALID_ARGUMENT"
	}
	b, _ := json.Marshal(e)
	return &provider.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Usage:      &usage.Tokens{},
	}
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
