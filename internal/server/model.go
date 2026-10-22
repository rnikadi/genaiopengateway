package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/router"
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
		defer func() { s.logRequest(rec, start) }()

		fail := func(status int, code, msg string) {
			rec.Status, rec.ErrorCode = status, code
			writeError(w, ep, status, code, msg)
		}

		p, ok := rt.keys.Lookup(presentedKey(r))
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
		rec.Status = resp.StatusCode
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("X-Gateway-Target", rec.Target)
		w.WriteHeader(resp.StatusCode)
		rec.UpstreamLatency = time.Since(start)
		copyFlush(w, resp.Body)

		if resp.StatusCode == http.StatusOK {
			rec.Tokens = *resp.Usage
			rec.CostUSD = usage.Cost(rec.Tokens, res.Target.Price)
		}
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

// requestLog is the one log line written per model request.
type requestLog struct {
	RequestID, Endpoint, Model, Target string
	OrgID, TeamID, KeyID, KeyPrefix    string
	Status                             int
	ErrorCode                          string
	Fallbacks                          int
	UpstreamLatency                    time.Duration
	Tokens                             usage.Tokens
	CostUSD                            float64
}

func (s *Server) logRequest(rec *requestLog, start time.Time) {
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "request",
		slog.String("request_id", rec.RequestID),
		slog.String("org_id", rec.OrgID),
		slog.String("team_id", rec.TeamID),
		slog.String("key_id", rec.KeyID),
		slog.String("key_prefix", rec.KeyPrefix),
		slog.String("endpoint", rec.Endpoint),
		slog.String("model", rec.Model),
		slog.String("target", rec.Target),
		slog.Int("status", rec.Status),
		slog.Int64("latency_ms", time.Since(start).Milliseconds()),
		slog.Int64("upstream_latency_ms", rec.UpstreamLatency.Milliseconds()),
		slog.Int64("input_tokens", rec.Tokens.InputTokens),
		slog.Int64("output_tokens", rec.Tokens.OutputTokens),
		slog.Int64("cached_input_tokens", rec.Tokens.CachedInputTokens),
		slog.Int64("reasoning_tokens", rec.Tokens.ReasoningTokens),
		slog.Float64("cost_usd", rec.CostUSD),
		slog.Int("fallbacks", rec.Fallbacks),
		slog.String("error_code", rec.ErrorCode),
	)
}
