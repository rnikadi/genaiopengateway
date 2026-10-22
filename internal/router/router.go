// Package router resolves virtual models to provider targets and falls
// back across them.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/openai"
)

// Errors returned by Route.
var (
	ErrUnknownModel     = errors.New("model not found")
	ErrUnsupported      = errors.New("model does not support this endpoint")
	ErrAllTargetsFailed = errors.New("all targets failed")
)

// factories builds an adapter for each provider type.
var factories = map[string]func(config.Provider, *http.Client) provider.Provider{
	config.ProviderOpenAI: func(c config.Provider, h *http.Client) provider.Provider { return openai.New(c, h) },
}

// Router dispatches requests to the targets of a virtual model.
type Router struct {
	models    map[string]config.Model
	providers map[string]provider.Provider
}

// New builds a router. All adapters share one HTTP client so connections
// are pooled per upstream host.
func New(c *config.Config, client *http.Client) (*Router, error) {
	r := &Router{models: c.Models, providers: map[string]provider.Provider{}}
	for name, p := range c.Providers {
		f, ok := factories[p.Type]
		if !ok {
			return nil, fmt.Errorf("provider %q: type %q is not available", name, p.Type)
		}
		r.providers[name] = f(p, client)
	}
	return r, nil
}

// Models lists virtual model names in sorted order.
func (r *Router) Models() []string {
	names := make([]string, 0, len(r.models))
	for name := range r.models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Attempt records one failed try, for logs and metrics.
type Attempt struct {
	Target config.Target
	Status int
	Err    error
}

// Result is the outcome of routing.
type Result struct {
	Response *provider.Response
	Target   config.Target
	// Failed lists targets tried before the one that answered.
	Failed []Attempt
}

// Route sends a request to each target of the model that supports the
// endpoint, in order, and returns the first response that is not
// retryable. The last target's response is returned even when it is
// retryable, so clients see the real upstream error.
func (r *Router) Route(ctx context.Context, ep provider.Endpoint, model string, body map[string]json.RawMessage, req provider.Request) (*Result, error) {
	m, ok := r.models[model]
	if !ok {
		return nil, ErrUnknownModel
	}
	var targets []config.Target
	for _, t := range m.Targets {
		if r.providers[t.Provider].Supports(ep) {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return nil, ErrUnsupported
	}

	res := &Result{}
	for i, t := range targets {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		body["model"], _ = json.Marshal(t.Model)
		raw, err := json.Marshal(body)
		if err != nil {
			return res, err
		}
		req.Endpoint, req.Model, req.Body = ep, t.Model, raw
		resp, err := r.providers[t.Provider].Send(ctx, &req)
		if err != nil {
			res.Failed = append(res.Failed, Attempt{Target: t, Err: err})
			continue
		}
		if provider.Retryable(resp.StatusCode) && i < len(targets)-1 {
			res.Failed = append(res.Failed, Attempt{Target: t, Status: resp.StatusCode})
			provider.Drain(resp)
			continue
		}
		res.Response, res.Target = resp, t
		return res, nil
	}
	return res, ErrAllTargetsFailed
}
