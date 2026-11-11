// Package router resolves virtual models to provider targets and falls
// back across them.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/anthropic"
	"github.com/rnikadi/genaiopengateway/internal/provider/assemblyai"
	"github.com/rnikadi/genaiopengateway/internal/provider/bedrock"
	"github.com/rnikadi/genaiopengateway/internal/provider/cohere"
	"github.com/rnikadi/genaiopengateway/internal/provider/deepgram"
	"github.com/rnikadi/genaiopengateway/internal/provider/elevenlabs"
	"github.com/rnikadi/genaiopengateway/internal/provider/fal"
	"github.com/rnikadi/genaiopengateway/internal/provider/gemini"
	"github.com/rnikadi/genaiopengateway/internal/provider/openai"
	"github.com/rnikadi/genaiopengateway/internal/provider/stability"
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
	config.ProviderOpenAI:     func(c config.Provider, h *http.Client) provider.Provider { return openai.New(c, h) },
	config.ProviderAnthropic:  func(c config.Provider, h *http.Client) provider.Provider { return anthropic.New(c, h) },
	config.ProviderGemini:     func(c config.Provider, h *http.Client) provider.Provider { return gemini.New(c, h) },
	config.ProviderVertex:     func(c config.Provider, h *http.Client) provider.Provider { return vertex.New(c, h) },
	config.ProviderBedrock:    func(c config.Provider, h *http.Client) provider.Provider { return bedrock.New(c, h) },
	config.ProviderElevenLabs: func(c config.Provider, h *http.Client) provider.Provider { return elevenlabs.New(c, h) },
	config.ProviderDeepgram:   func(c config.Provider, h *http.Client) provider.Provider { return deepgram.New(c, h) },
	config.ProviderAssemblyAI: func(c config.Provider, h *http.Client) provider.Provider { return assemblyai.New(c, h) },
	config.ProviderStability:  func(c config.Provider, h *http.Client) provider.Provider { return stability.New(c, h) },
	config.ProviderFal:        func(c config.Provider, h *http.Client) provider.Provider { return fal.New(c, h) },
	config.ProviderCohere:     func(c config.Provider, h *http.Client) provider.Provider { return cohere.New(c, h) },
}

// Router dispatches requests to the targets of a virtual model.
type Router struct {
	models    map[string]config.Model
	providers map[string]provider.Provider
	configs   map[string]config.Provider
	types     map[string]string // provider name to type
	client    *http.Client
	health    *Health
	breaker   config.CircuitBreaker
	sleep     func(context.Context, time.Duration) error
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
var credentialTypes = []string{config.ProviderOpenAI, config.ProviderAnthropic, config.ProviderGemini, config.ProviderBedrock, config.ProviderElevenLabs, config.ProviderDeepgram, config.ProviderAssemblyAI, config.ProviderStability, config.ProviderFal, config.ProviderCohere}

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
// are pooled per upstream host. Health is kept across routers so a
// config reload keeps what the gateway learned about its targets; nil
// starts afresh.
func New(c *config.Config, client *http.Client, h *Health) (*Router, error) {
	if h == nil {
		h = NewHealth()
	}
	r := &Router{models: c.Models, providers: map[string]provider.Provider{}, configs: c.Providers, types: map[string]string{}, client: client,
		health: h, breaker: c.Routing.CircuitBreaker, sleep: sleepCtx}
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
	all, err := r.Targets(ctx, ep, model)
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
	m := r.models[model]
	if keep == nil {
		key := ""
		if m.Sticky == "user" {
			key = stickyKey(body)
		}
		targets = r.health.order(model, m, targets, key)
	}
	targets = r.admitted(targets)

	res := &Result{}
	for i, t := range targets {
		last := i == len(targets)-1
		body["model"], _ = json.Marshal(t.Model)
		raw, err := json.Marshal(body)
		if err != nil {
			return res, err
		}
		for try := 0; ; try++ {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			req.Endpoint, req.Model, req.Body = ep, t.Model, raw
			a := r.send(ctx, t, &req)
			failed := a.Err != nil || provider.Retryable(a.Status)
			r.health.observe(t.String(), a.TTFB, failed, r.breaker)
			if !failed {
				res.Response, res.Target, res.TTFB = a.resp, t, a.TTFB
				return res, nil
			}
			retry := try < m.Retries.Attempts
			if !retry && last && a.Err == nil {
				// The last target's error goes to the client as it is.
				res.Response, res.Target, res.TTFB = a.resp, t, a.TTFB
				return res, nil
			}
			res.Failed = append(res.Failed, a.Attempt)
			wait := backoff(m.Retries, try)
			if a.resp != nil {
				if ra, ok := retryAfter(a.resp.Header); ok && ra <= m.Retries.MaxBackoff.Std() {
					wait = ra
				}
				provider.Drain(a.resp)
			}
			if !retry {
				break
			}
			sleep := r.sleep
			if sleep == nil {
				sleep = sleepCtx
			}
			if err := sleep(ctx, wait); err != nil {
				return res, err
			}
		}
	}
	return res, ErrAllTargetsFailed
}

// admitted drops targets whose circuit breaker is open. When every
// target is open, all of them are tried anyway: a request that might
// succeed beats one that surely fails.
func (r *Router) admitted(targets []config.Target) []config.Target {
	var out []config.Target
	for _, t := range targets {
		if r.health.admit(t.String()) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return targets
	}
	return out
}

// stickyKey is who a request is for: OpenAI's user field, or Anthropic's
// metadata.user_id.
func stickyKey(body map[string]json.RawMessage) string {
	var user string
	if raw, ok := body["user"]; ok && json.Unmarshal(raw, &user) == nil && user != "" {
		return user
	}
	var meta struct {
		UserID string `json:"user_id"`
	}
	if raw, ok := body["metadata"]; ok && json.Unmarshal(raw, &meta) == nil {
		return meta.UserID
	}
	return ""
}

// backoff is the wait before retry number try+1: exponential from
// Backoff, capped at MaxBackoff, with full jitter.
func backoff(rt config.Retries, try int) time.Duration {
	d := rt.Backoff.Std() << min(try, 20)
	if d <= 0 || d > rt.MaxBackoff.Std() {
		d = rt.MaxBackoff.Std()
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)) + 1)
}

// retryAfter reads a Retry-After header in seconds or as an HTTP date.
func retryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Health returns what the router has learned about its targets.
func (r *Router) Health() *Health { return r.health }

type sent struct {
	Attempt
	resp *provider.Response
}

// send tries one target inside a GenAI client span, named for the
// operation and upstream model as the semantic conventions say. The span
// ends at response headers; streaming time belongs to the request span.
func (r *Router) send(ctx context.Context, t config.Target, req *provider.Request) sent {
	op := telemetry.Operation(req.Endpoint)
	ctx, span := telemetry.Tracer().Start(ctx, telemetry.SpanName(op, t.Model), trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(telemetry.RequestAttributes(req.Body),
			attribute.String("gen_ai.operation.name", op),
			attribute.String("gen_ai.provider.name", r.types[t.Provider]),
			attribute.String("gen_ai.request.model", t.Model),
			attribute.String("gog.target", t.String()),
		)...))
	defer span.End()
	start := time.Now()
	resp, err := r.sendWithin(ctx, t, req)
	s := sent{Attempt: Attempt{Target: t, Err: err, TTFB: time.Since(start)}, resp: resp}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "no response")
		errType := "connection_error"
		if errors.Is(err, errTargetTimeout) || errors.Is(err, context.DeadlineExceeded) {
			errType = "timeout"
		}
		span.SetAttributes(attribute.String("error.type", errType))
		return s
	}
	s.Status = resp.StatusCode
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
		span.SetAttributes(attribute.String("error.type", strconv.Itoa(resp.StatusCode)))
	}
	return s
}

// ErrResidency is a model with targets for the endpoint, none of them in
// a residency the caller is pinned to.
var ErrResidency = errors.New("no target of the model is in a data residency this key may use")

type residencyKey struct{}

// WithResidency returns a context whose requests only go to providers
// whose residency label is in every one of levels. Nil or empty levels
// do not restrict.
func WithResidency(ctx context.Context, levels [][]string) context.Context {
	if len(levels) == 0 {
		return ctx
	}
	return context.WithValue(ctx, residencyKey{}, levels)
}

// residencyAllows reports whether ctx's residency pins allow a provider.
func (r *Router) residencyAllows(ctx context.Context, providerName string) bool {
	levels, _ := ctx.Value(residencyKey{}).([][]string)
	label := r.configs[providerName].Residency
	for _, l := range levels {
		if !slices.Contains(l, label) {
			return false
		}
	}
	return true
}

// errTargetTimeout is a target's timeout to response headers passing.
var errTargetTimeout = errors.New("no response headers within the target's timeout")

// sendWithin sends to a target, giving up when its timeout passes before
// response headers. The body keeps streaming after that, so the
// deadline is lifted once headers arrive, and the request's context is
// only cancelled when the body is closed.
func (r *Router) sendWithin(ctx context.Context, t config.Target, req *provider.Request) (*provider.Response, error) {
	p := r.adapter(ctx, t.Provider)
	if t.Timeout <= 0 {
		return p.Send(ctx, req)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(t.Timeout.Std(), func() { cancel(errTargetTimeout) })
	resp, err := p.Send(ctx, req)
	if !timer.Stop() && context.Cause(ctx) == errTargetTimeout {
		if resp != nil {
			provider.Drain(resp)
		}
		cancel(nil)
		return nil, errTargetTimeout
	}
	if err != nil {
		cancel(nil)
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: func() { cancel(nil) }}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel func()
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// Targets lists the targets of a model that serve an endpoint, in
// order, for callers that try them themselves, such as realtime.
func (r *Router) Targets(ctx context.Context, ep provider.Endpoint, model string) ([]config.Target, error) {
	m, ok := r.models[model]
	if !ok {
		return nil, ErrUnknownModel
	}
	var out []config.Target
	pinned := false
	for _, t := range m.Targets {
		if !provider.SupportsModel(r.providers[t.Provider], ep, t.Model) {
			continue
		}
		if !r.residencyAllows(ctx, t.Provider) {
			pinned = true
			continue
		}
		out = append(out, t)
	}
	switch {
	case len(out) > 0:
		return out, nil
	case pinned:
		return nil, ErrResidency
	}
	return nil, ErrUnsupported
}

// Dialer returns a provider's realtime adapter.
func (r *Router) Dialer(ctx context.Context, providerName string) (provider.Dialer, bool) {
	d, ok := r.adapter(ctx, providerName).(provider.Dialer)
	return d, ok
}

// LiveDialer returns a provider's adapter for Gemini Live sessions, or
// false when the provider is gone or does not serve them.
func (r *Router) LiveDialer(ctx context.Context, providerName string) (provider.LiveDialer, bool) {
	d, ok := r.adapter(ctx, providerName).(provider.LiveDialer)
	return d, ok
}

// BidiOpener returns a provider's adapter for Bedrock bidirectional
// streams, or false when the provider is gone or does not serve them.
func (r *Router) BidiOpener(ctx context.Context, providerName string) (provider.BidiOpener, bool) {
	o, ok := r.adapter(ctx, providerName).(provider.BidiOpener)
	return o, ok
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
	provider.Moderations, provider.Rerank, provider.Videos, provider.Realtime, provider.Live, provider.BidiStream, provider.Batches,
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
