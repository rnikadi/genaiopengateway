package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/limits"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/router"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// forwardedHeaders are client headers passed to adapters, which decide
// whether to send them upstream.
var forwardedHeaders = []string{"Anthropic-Version", "Anthropic-Beta"}

// model serves one model endpoint: authenticate, check policy, route,
// stream the answer back and record usage.
func (s *Server) model(ep provider.Endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rt := s.rt.Load()
		rec := &requestLog{RequestID: r.Header.Get("X-Request-Id"), Endpoint: string(ep)}
		ctx := telemetry.Propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := telemetry.Tracer().Start(ctx, "gateway.request", trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("gen_ai.operation.name", telemetry.Operation(ep)),
				attribute.String("http.route", r.URL.Path),
				attribute.String("gog.request_id", rec.RequestID),
			))
		r = r.WithContext(ctx)
		defer func() { s.finish(rt, rec, span, start) }()

		fail := func(status int, code, msg string) {
			rec.Status, rec.ErrorCode = status, code
			writeError(w, ep, status, code, msg)
		}

		p, ok := s.authenticate(r)
		if !ok {
			fail(http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
			return
		}
		rec.OrgID, rec.TeamID, rec.KeyID, rec.KeyPrefix = p.OrgID, p.TeamID, p.KeyID, p.KeyPrefix

		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rt.cfg.Limits.MaxRequestBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				fail(http.StatusRequestEntityTooLarge, codeTooLarge, "request body is too large")
			} else {
				fail(http.StatusBadRequest, codeInvalidRequest, "could not read request body")
			}
			return
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(raw, &body) != nil || body == nil {
			fail(http.StatusBadRequest, codeInvalidRequest, "request body must be a JSON object")
			return
		}
		var envelope struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.Unmarshal(raw, &envelope) != nil || envelope.Model == "" {
			fail(http.StatusBadRequest, codeInvalidRequest, "model is required and must be a string")
			return
		}
		rec.Model = envelope.Model
		if !p.AllowsModel(envelope.Model) {
			fail(http.StatusForbidden, codeModelNotAllowed, "this key may not use model "+envelope.Model)
			return
		}

		d := s.limiter.Admit(r.Context(), p)
		setRateHeaders(w.Header(), d)
		if !d.Allowed {
			if d.Kind != "" {
				s.metrics.ObserveLimitRejection(d.Scope, d.Kind)
			}
			status := http.StatusTooManyRequests
			if d.Code == limits.CodeUnavailable {
				status = http.StatusServiceUnavailable
				s.log.Error("limiter unavailable", "request_id", rec.RequestID, "err", d.Message)
				d.Message = "limits could not be checked"
			}
			if d.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.FormatInt(limits.RetryAfterSeconds(d.RetryAfter), 10))
			}
			fail(status, d.Code, d.Message)
			return
		}

		hdr := http.Header{}
		for _, h := range forwardedHeaders {
			if v := r.Header.Values(h); len(v) > 0 {
				hdr[h] = v
			}
		}
		res, err := rt.router.Route(r.Context(), ep, envelope.Model, body, provider.Request{Stream: envelope.Stream, Header: hdr})
		if res != nil {
			rec.Fallbacks = len(res.Failed)
			for _, a := range res.Failed {
				s.metrics.ObserveUpstream(a.Target.String(), a.TTFB)
				s.metrics.ObserveFallback(envelope.Model, a.Target.String(), a.Status)
				s.log.Warn("target failed", "request_id", rec.RequestID, "target", a.Target.String(), "status", a.Status, "err", a.Err)
			}
		}
		switch {
		case errors.Is(err, router.ErrUnknownModel), errors.Is(err, router.ErrUnsupported):
			fail(http.StatusNotFound, codeModelNotFound, "model "+envelope.Model+": "+err.Error())
			return
		case errors.Is(err, context.Canceled):
			rec.Status, rec.ErrorCode = 499, "client_closed"
			return
		case err != nil:
			fail(http.StatusBadGateway, codeAllTargetsFailed, "all upstream targets failed")
			return
		}

		resp := res.Response
		defer resp.Body.Close()
		rec.Target = res.Target.String()
		rec.Provider, rec.UpstreamModel = res.Target.Provider, res.Target.Model
		rec.Status = resp.StatusCode
		s.metrics.ObserveUpstream(rec.Target, res.TTFB)
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("X-Gateway-Target", rec.Target)
		w.WriteHeader(resp.StatusCode)
		rec.UpstreamLatency = time.Since(start)
		copyFlush(w, resp.Body)
		// Close before reading usage: adapters finalize it on close.
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			rec.Tokens = *resp.Usage
			rec.CostUSD = usage.Cost(rec.Tokens, res.Target.Price)
			rec.Unpriced = res.Target.Price == (config.Price{}) && rec.Tokens.Total() > 0
			// The client may be gone; charging must still happen.
			if err := s.limiter.Charge(context.WithoutCancel(r.Context()), p, rec.Tokens.Total(), rec.CostUSD); err != nil {
				s.log.Error("charging limits failed", "request_id", rec.RequestID, "err", err)
			}
		}
		if s.usage != nil {
			s.usage.Add(usage.Record{
				RequestID: rec.RequestID, Time: start, OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID,
				Endpoint: string(ep), Model: rec.Model, Provider: res.Target.Provider, UpstreamModel: res.Target.Model,
				Status: rec.Status, LatencyMS: time.Since(start).Milliseconds(), Tokens: rec.Tokens, CostUSD: rec.CostUSD,
			})
		}
	}
}

// setRateHeaders adds OpenAI-style rate limit headers for the tightest
// per-minute limits that apply.
func setRateHeaders(h http.Header, d limits.Decision) {
	for kind, win := range map[string]limits.Window{"requests": d.Requests, "tokens": d.Tokens} {
		if win.Limit == 0 {
			continue
		}
		h.Set("X-Ratelimit-Limit-"+kind, strconv.FormatInt(win.Limit, 10))
		h.Set("X-Ratelimit-Remaining-"+kind, strconv.FormatInt(win.Remaining, 10))
		h.Set("X-Ratelimit-Reset-"+kind, (time.Duration(limits.RetryAfterSeconds(win.Reset)) * time.Second).String())
	}
}

// copyFlush copies src to w, flushing after every read so streamed
// tokens reach the client as soon as they arrive.
func copyFlush(w http.ResponseWriter, src io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// requestLog is what one model request reports: one log line, metrics
// and span attributes.
type requestLog struct {
	RequestID, Endpoint, Model, Target string
	Provider, UpstreamModel            string
	OrgID, TeamID, KeyID, KeyPrefix    string
	Status                             int
	ErrorCode                          string
	Fallbacks                          int
	UpstreamLatency                    time.Duration
	Tokens                             usage.Tokens
	CostUSD                            float64
	Unpriced                           bool
}

// finish reports a model request once it is served.
func (s *Server) finish(rt *runtime, rec *requestLog, span trace.Span, start time.Time) {
	latency := time.Since(start)
	// Only configured model names become metric labels, so clients cannot
	// grow cardinality.
	model := rec.Model
	if !rt.router.Has(model) {
		model = ""
	}
	s.metrics.ObserveRequest(telemetry.Request{
		Endpoint: rec.Endpoint, Model: model, Target: rec.Target, Status: rec.Status, ErrorCode: rec.ErrorCode,
		Duration: latency, Tokens: rec.Tokens, CostUSD: rec.CostUSD, Unpriced: rec.Unpriced,
	})

	span.SetAttributes(
		attribute.String("gog.virtual_model", rec.Model),
		attribute.String("gog.org_id", rec.OrgID),
		attribute.String("gog.team_id", rec.TeamID),
		attribute.Int("http.response.status_code", rec.Status),
	)
	if rec.Target != "" {
		span.SetAttributes(
			attribute.String("gen_ai.provider.name", rt.cfg.Providers[rec.Provider].Type),
			attribute.String("gen_ai.request.model", rec.UpstreamModel),
			attribute.String("gen_ai.response.model", rec.UpstreamModel),
			attribute.Int64("gen_ai.usage.input_tokens", rec.Tokens.InputTokens),
			attribute.Int64("gen_ai.usage.output_tokens", rec.Tokens.OutputTokens),
			attribute.Int("gog.fallbacks", rec.Fallbacks),
		)
	}
	if rec.Status >= 500 {
		span.SetStatus(codes.Error, rec.ErrorCode)
	}
	if rec.ErrorCode != "" {
		span.SetAttributes(attribute.String("error.type", rec.ErrorCode))
	}
	span.End()

	attrs := []slog.Attr{slog.String("request_id", rec.RequestID)}
	if sc := span.SpanContext(); sc.IsValid() {
		attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()))
	}
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "request", append(attrs,
		slog.String("org_id", rec.OrgID),
		slog.String("team_id", rec.TeamID),
		slog.String("key_id", rec.KeyID),
		slog.String("key_prefix", rec.KeyPrefix),
		slog.String("endpoint", rec.Endpoint),
		slog.String("model", rec.Model),
		slog.String("target", rec.Target),
		slog.Int("status", rec.Status),
		slog.Int64("latency_ms", latency.Milliseconds()),
		slog.Int64("upstream_latency_ms", rec.UpstreamLatency.Milliseconds()),
		slog.Int64("input_tokens", rec.Tokens.InputTokens),
		slog.Int64("output_tokens", rec.Tokens.OutputTokens),
		slog.Int64("cached_input_tokens", rec.Tokens.CachedInputTokens),
		slog.Int64("reasoning_tokens", rec.Tokens.ReasoningTokens),
		slog.Float64("cost_usd", rec.CostUSD),
		slog.Int("fallbacks", rec.Fallbacks),
		slog.String("error_code", rec.ErrorCode),
	)...)
}
