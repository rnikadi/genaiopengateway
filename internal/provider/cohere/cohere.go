// Package cohere adapts the Cohere API: chat and embeddings through its
// OpenAI compatibility API, and rerank through its own v2 API, whose
// request shape the gateway's rerank endpoint already uses.
package cohere

import (
	"context"
	"net/http"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/openai"
)

// Provider is a Cohere upstream.
type Provider struct {
	compat, v2 *openai.Provider
}

// New returns an adapter for cfg, whose base URL is the API host, such
// as https://api.cohere.com.
func New(cfg config.Provider, client *http.Client) *Provider {
	compat, v2 := cfg, cfg
	compat.BaseURL += "/compatibility/v1"
	v2.BaseURL += "/v2"
	return &Provider{compat: openai.New(compat, client), v2: openai.New(v2, client)}
}

// Supports implements provider.Provider.
func (p *Provider) Supports(e provider.Endpoint) bool {
	return e == provider.ChatCompletions || e == provider.Embeddings || e == provider.Rerank
}

// SupportsModel implements provider.ModelSupporter: rerank models
// rerank, embed models embed, and the others chat.
func (p *Provider) SupportsModel(e provider.Endpoint, model string) bool {
	switch {
	case hasPrefix(model, "rerank"):
		return e == provider.Rerank
	case hasPrefix(model, "embed"):
		return e == provider.Embeddings
	}
	return e == provider.ChatCompletions
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// Send implements provider.Provider.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if req.Endpoint == provider.Rerank {
		return p.v2.Send(ctx, req)
	}
	return p.compat.Send(ctx, req)
}
