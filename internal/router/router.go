// Package router resolves virtual models to provider targets and falls
// back across them.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	configs   map[string]config.Provider
	types     map[string]string // provider name to type
	client    *http.Client
}

// Credentials are adapters that replace providers of the same name for
// one request, such as those built from a team's own provider keys
// (docs/team-credentials.md).
type Credentials map[string]provider.Provider

type credentialsKey struct{}

// WithCredentials returns a context whose requests use c in place of the
// providers it names.
func WithCredentials(ctx context.Context, c Credentials) context.Context {
	if len(c) == 0 {
		return ctx
	}
	return context.WithValue(ctx, credentialsKey{}, c)
}

// adapter returns the adapter for a provider, honoring the context's
// credentials.
func (r *Router) adapter(ctx context.Context, name string) provider.Provider {
	if c, ok := ctx.Value(credentialsKey{}).(Credentials); ok {
		if p, ok := c[name]; ok {
			return p
		}
	}
	return r.providers[name]
}

// credentialTypes are the provider types whose api_key a team may replace.
var credentialTypes = []string{config.ProviderOpenAI, config.ProviderAnthropic, config.ProviderGemini, config.ProviderBedrock}

// CheckCredential reports why a provider cannot take a team's API key.
func (r *Router) CheckCredential(providerName string) error {
	c, ok := r.configs[providerName]
	if !ok {
		return fmt.Errorf("unknown provider %q", providerName)
	}
	if !slices.Contains(credentialTypes, c.Type) {
		return fmt.Errorf("provider %q is of type %s, which does not take an API key per team", providerName, c.Type)
	}
	return nil
}

// Unavailable returns an adapter whose requests fail with err, so
// fallback moves to the next target. It stands in for a provider whose
// team credential cannot be used.
func Unavailable(err error) provider.Provider { return unavailable{err} }

type unavailable struct{ err error }

func (u unavailable) Supports(provider.Endpoint) bool { return true }
func (u unavailable) Send(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, u.err
}

// WithAPIKey builds an adapter for a provider that uses apiKey in place
// of the provider's own.
func (r *Router) WithAPIKey(providerName, apiKey string) (provider.Provider, error) {
	if err := r.CheckCredential(providerName); err != nil {
		return unavailable{fmt.Errorf("team credential: %w", err)}, err
	}
	c := r.configs[providerName]
	c.APIKey = config.Secret(apiKey)
	return factories[c.Type](c, r.client), nil
}

// New builds a router. All adapters share one HTTP client so connections
// are pooled per upstream host.
func New(c *config.Config, client *http.Client) (*Router, error) {
	r := &Router{models: c.Models, providers: map[string]provider.Provider{}, configs: c.Providers, types: map[string]string{}, client: client}
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
	return r.route(ctx, ep, model, nil, body, req)
}

// RouteTo sends a request to one target of the model, named by its
// provider and upstream model, such as the target that created a video
// job.
func (r *Router) RouteTo(ctx context.Context, ep provider.Endpoint, model, providerName, upstreamModel string, body map[string]json.RawMessage, req provider.Request) (*Result, error) {
	return r.route(ctx, ep, model, func(t config.Target) bool { return t.Provider == providerName && t.Model == upstreamModel }, body, req)
}

func (r *Router) route(ctx context.Context, ep provider.Endpoint, model string, keep func(config.Target) bool, body map[string]json.RawMessage, req provider.Request) (*Result, error) {
	all, err := r.Targets(ep, model)
	if err != nil {
		return nil, err
	}
	var targets []config.Target
	for _, t := range all {
		if keep == nil || keep(t) {
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
	resp, err := r.adapter(ctx, t.Provider).Send(ctx, req)
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

// Targets lists the targets of a model that serve an endpoint, in
// order, for callers that try them themselves, such as realtime.
func (r *Router) Targets(ep provider.Endpoint, model string) ([]config.Target, error) {
	m, ok := r.models[model]
	if !ok {
		return nil, ErrUnknownModel
	}
	var out []config.Target
	for _, t := range m.Targets {
		if provider.SupportsModel(r.providers[t.Provider], ep, t.Model) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, ErrUnsupported
	}
	return out, nil
}

// Dialer returns a provider's realtime adapter.
func (r *Router) Dialer(ctx context.Context, providerName string) (provider.Dialer, bool) {
	d, ok := r.adapter(ctx, providerName).(provider.Dialer)
	return d, ok
}

// Forwarder returns a provider's adapter for calls on upstream objects,
// or false when the provider is gone or does not serve them.
func (r *Router) Forwarder(ctx context.Context, providerName string) (provider.Forwarder, bool) {
	f, ok := r.adapter(ctx, providerName).(provider.Forwarder)
	return f, ok
}

// AllEndpoints lists every endpoint the gateway serves, in display order.
var AllEndpoints = []provider.Endpoint{
	provider.ChatCompletions, provider.Responses, provider.Messages, provider.GenerateContent, provider.Embeddings,
	provider.ImageGenerations, provider.ImageEdits, provider.Speech, provider.Transcriptions, provider.Translations,
	provider.Moderations, provider.Rerank, provider.Videos, provider.Realtime, provider.Batches,
}

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
