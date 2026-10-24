package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/guardrail"
	"github.com/rnikadi/genaiopengateway/internal/limits"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/router"
	"github.com/rnikadi/genaiopengateway/internal/store"
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

		p, ok := principalFrom(r.Context())
		if !ok {
			p, ok = s.authenticate(r)
		}
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
		var files []provider.File
		if ep.Multipart() && isMultipart(r) {
			// Text fields take the place of a JSON body; files ride beside.
			if body, files, err = readMultipart(r, raw); err != nil {
				fail(http.StatusBadRequest, codeInvalidRequest, "could not read the multipart body: "+err.Error())
				return
			}
			raw, _ = json.Marshal(body)
		} else if json.Unmarshal(raw, &body) != nil || body == nil {
			fail(http.StatusBadRequest, codeInvalidRequest, "request body must be a JSON object")
			return
		}
		if ep == provider.GenerateContent {
			if !googleRequest(r, body) {
				fail(http.StatusNotFound, codeModelNotFound, "unknown method: use generateContent or streamGenerateContent")
				return
			}
			raw, _ = json.Marshal(body)
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

		names := p.Guardrails()
		if envelope.Stream && (ep == provider.Transcriptions || ep == provider.Translations) && rt.guards.Applies(config.StageOutput, names) {
			// Transcript deltas cannot be checked before the client sees them.
			fail(http.StatusBadRequest, codeInvalidRequest, "streamed transcripts are not available when output guardrails apply; send the request without stream")
			return
		}
		meta := guardMeta(rec.RequestID, p)
		sent, in := s.guardRequest(r.Context(), rt, meta, names, raw)
		acted := in.Decisions
		if in.Blocked != nil {
			setGuardrailsHeader(w, acted)
			fail(http.StatusBadRequest, codeGuardrailBlocked, blockedMessage(in.Blocked))
			return
		}
		if in.Changed {
			raw, body = sent, nil
			if json.Unmarshal(raw, &body) != nil {
				fail(http.StatusInternalServerError, codeInvalidRequest, "the redacted request could not be encoded")
				return
			}
		}

		hdr := http.Header{}
		for _, h := range forwardedHeaders {
			if v := r.Header.Values(h); len(v) > 0 {
				hdr[h] = v
			}
		}
		res, err := rt.router.Route(r.Context(), ep, envelope.Model, body, provider.Request{Stream: envelope.Stream, Header: hdr, Files: files})
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
		contentType := resp.Header.Get("Content-Type")
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Header().Set("X-Gateway-Target", rec.Target)
		capture := s.newCapture(p)
		var src io.Reader = resp.Body
		if capture != nil {
			src = io.TeeReader(resp.Body, capture)
		}
		if resp.StatusCode == http.StatusOK && rt.guards.Applies(config.StageOutput, names) && textOutput(ep) && !binaryMedia(contentType) {
			// Output guardrails see the whole response before the client.
			held, err := readChecked(src)
			rec.UpstreamLatency = time.Since(start)
			var out guardrail.Result
			switch {
			case errors.Is(err, errResponseTooLarge):
				out.Blocked = &guardrail.Decision{Guardrail: rt.guards.Names(config.StageOutput, names)[0], Stage: config.StageOutput,
					Action: config.ActionBlock, Reason: err.Error()}
				out.Decisions = []guardrail.Decision{*out.Blocked}
				s.recordGuardrails(meta, out.Decisions)
			case err != nil:
				_ = resp.Body.Close()
				fail(http.StatusBadGateway, codeAllTargetsFailed, "reading the upstream response failed")
				return
			default:
				held, out = s.guardResponse(r.Context(), rt, meta, names, held, contentType)
			}
			acted = append(acted, out.Decisions...)
			setGuardrailsHeader(w, acted)
			if out.Blocked != nil {
				w.Header().Del("X-Gateway-Target")
				fail(http.StatusBadRequest, codeGuardrailBlocked, blockedMessage(out.Blocked))
			} else {
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(held)
			}
		} else {
			setGuardrailsHeader(w, acted)
			w.WriteHeader(resp.StatusCode)
			rec.UpstreamLatency = time.Since(start)
			copyFlush(w, src)
		}
		// Close before reading usage: adapters finalize it on close.
		_ = resp.Body.Close()
		if capture != nil {
			// Copy only what redaction reads, so the queue does not hold
			// whole large requests.
			logged := raw
			if files != nil {
				logged = withFileNotes(body, files)
			}
			req := bytes.Clone(logged[:min(len(logged), capture.max)])
			s.bodies.Add(capturedBody{
				RequestBody: store.RequestBody{
					RequestID: rec.RequestID, Time: start.UTC(), OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID,
					Kind: "model", Endpoint: string(ep), Truncated: capture.truncated || len(req) < len(logged),
				},
				request: req, response: capture.Bytes(), responseContentType: resp.Header.Get("Content-Type"),
			})
		}

		if resp.StatusCode == http.StatusOK {
			rec.Tokens = *resp.Usage
			rec.CostUSD = usage.Cost(rec.Tokens, res.Target.Price)
			rec.Unpriced = res.Target.Price.IsZero() && (rec.Tokens.Total() > 0 || rec.Tokens.Media())
			// The client may be gone; charging must still happen.
			ctx := context.WithoutCancel(r.Context())
			spent, err := s.limiter.Charge(ctx, p, rec.Tokens.Total(), rec.CostUSD)
			if err != nil {
				s.log.Error("charging limits failed", "request_id", rec.RequestID, "err", err)
			}
			s.budgetAlerts(ctx, p, spent)
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

// textOutput reports whether an endpoint answers with text that output
// guardrails check. Images and speech answer with media.
func textOutput(ep provider.Endpoint) bool {
	return ep != provider.ImageGenerations && ep != provider.ImageEdits && ep != provider.Speech
}

// setGuardrailsHeader names the guardrails that acted.
func setGuardrailsHeader(w http.ResponseWriter, acted []guardrail.Decision) {
	if len(acted) > 0 {
		w.Header().Set(guardrailsHeader, guardrailSummary(acted))
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
		slog.Int64("images", rec.Tokens.Images),
		slog.Int64("characters", rec.Tokens.Characters),
		slog.Float64("audio_seconds", rec.Tokens.AudioSeconds),
		slog.Float64("video_seconds", rec.Tokens.VideoSeconds),
		slog.Float64("cost_usd", rec.CostUSD),
		slog.Int("fallbacks", rec.Fallbacks),
		slog.String("error_code", rec.ErrorCode),
	)...)
}

// googleRequest adds the model and stream flag of a generateContent
// path, such as /v1beta/models/chat:streamGenerateContent, to the body
// so the request takes the same path as every model endpoint. The
// adapter removes them again.
func googleRequest(r *http.Request, body map[string]json.RawMessage) bool {
	model, method, _ := strings.Cut(r.PathValue("action"), ":")
	switch method {
	case "generateContent":
		body["stream"] = json.RawMessage("false")
	case "streamGenerateContent":
		body["stream"] = json.RawMessage("true")
	default:
		return false
	}
	body["model"], _ = json.Marshal(model)
	return true
}
