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
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/anthropic"
	"github.com/rnikadi/genaiopengateway/internal/provider/bedrock"
	"github.com/rnikadi/genaiopengateway/internal/provider/gemini"
	"github.com/rnikadi/genaiopengateway/internal/provider/openai"
	"github.com/rnikadi/genaiopengateway/internal/provider/vertex"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
)

// Errors returned by Route.
var (
	ErrUnknownModel     = errors.New("model not found")
	ErrUnsupported      = errors.New("model does not support this endpoint")
	ErrAllTargetsFailed = errors.New("all targets failed")
)

// factories builds an adapter for each provider type.
var factories = map[string]func(config.Provider, *http.Client) provider.Provider{
	config.ProviderOpenAI:    func(c config.Provider, h *http.Client) provider.Provider { return openai.New(c, h) },
	config.ProviderAnthropic: func(c config.Provider, h *http.Client) provider.Provider { return anthropic.New(c, h) },
	config.ProviderGemini:    func(c config.Provider, h *http.Client) provider.Provider { return gemini.New(c, h) },
	config.ProviderVertex:    func(c config.Provider, h *http.Client) provider.Provider { return vertex.New(c, h) },
	config.ProviderBedrock:   func(c config.Provider, h *http.Client) provider.Provider { return bedrock.New(c, h) },
}

// Router dispatches requests to the targets of a virtual model.
type Router struct {
	models    map[string]config.Model
	providers map[string]provider.Provider
	types     map[string]string // provider name to type
}

// New builds a router. All adapters share one HTTP client so connections
// are pooled per upstream host.
func New(c *config.Config, client *http.Client) (*Router, error) {
	r := &Router{models: c.Models, providers: map[string]provider.Provider{}, types: map[string]string{}}
	for name, p := range c.Providers {
		f, ok := factories[p.Type]
		if !ok {
			return nil, fmt.Errorf("provider %q: type %q is not available", name, p.Type)
		}
		r.providers[name] = f(p, client)
		r.types[name] = p.Type
	}
	return r, nil
}

// Has reports whether model is a configured virtual model.
func (r *Router) Has(model string) bool {
	_, ok := r.models[model]
	return ok
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

// Attempt records one try, for logs and metrics.
type Attempt struct {
	Target config.Target
	Status int   // zero when no response was received
	Err    error // set when no response was received
	// TTFB is the time from sending to response headers or failure.
	TTFB time.Duration
}

// Result is the outcome of routing.
type Result struct {
	Response *provider.Response
	Target   config.Target
	TTFB     time.Duration
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
		if provider.SupportsModel(r.providers[t.Provider], ep, t.Model) {
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
		a := r.send(ctx, t, &req)
		if a.Err != nil {
			res.Failed = append(res.Failed, a.Attempt)
			continue
		}
		if provider.Retryable(a.resp.StatusCode) && i < len(targets)-1 {
			res.Failed = append(res.Failed, a.Attempt)
			provider.Drain(a.resp)
			continue
		}
		res.Response, res.Target, res.TTFB = a.resp, t, a.TTFB
		return res, nil
	}
	return res, ErrAllTargetsFailed
}

type sent struct {
	Attempt
	resp *provider.Response
}

// send tries one target inside a gateway.upstream span. The span ends at
// response headers; streaming time belongs to the request span.
func (r *Router) send(ctx context.Context, t config.Target, req *provider.Request) sent {
	ctx, span := telemetry.Tracer().Start(ctx, "gateway.upstream", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", telemetry.Operation(req.Endpoint)),
			attribute.String("gen_ai.provider.name", r.types[t.Provider]),
			attribute.String("gen_ai.request.model", t.Model),
			attribute.String("gog.target", t.String()),
		))
	defer span.End()
	start := time.Now()
	resp, err := r.providers[t.Provider].Send(ctx, req)
	s := sent{Attempt: Attempt{Target: t, Err: err, TTFB: time.Since(start)}, resp: resp}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "no response")
		return s
	}
	s.Status = resp.StatusCode
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
	}
	return s
}

// AllEndpoints lists every endpoint the gateway serves, in display order.
var AllEndpoints = []provider.Endpoint{provider.ChatCompletions, provider.Responses, provider.Messages, provider.GenerateContent, provider.Embeddings}

// Endpoints lists what a provider serves for an upstream model.
func (r *Router) Endpoints(providerName, model string) []provider.Endpoint {
	p, ok := r.providers[providerName]
	if !ok {
		return nil
	}
	out := []provider.Endpoint{}
	for _, e := range AllEndpoints {
		if provider.SupportsModel(p, e, model) {
			out = append(out, e)
		}
	}
	return out
}
