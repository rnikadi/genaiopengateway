package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/router"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// liveSetupTimeout bounds the wait for a Live session's setup message.
const liveSetupTimeout = 30 * time.Second

// live relays a Gemini Live API session (BidiGenerateContent) to one
// target. The client names a virtual model in its setup message, which
// the gateway reads before it dials, and each message with usage is
// charged as it passes (docs/modalities.md#gemini-live).
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	const ep = provider.Live
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
		rec.Status, rec.ErrorCode, rec.ErrorMessage = status, code, msg
		writeError(w, provider.GenerateContent, status, code, msg)
	}

	// Google's SDKs send the key in the query or in x-goog-api-key.
	key := presentedKey(r)
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	p, ok := s.keys.Load().Lookup(key)
	if !ok || p.Expired(time.Now()) {
		fail(http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
		return
	}
	rec.OrgID, rec.TeamID, rec.KeyID, rec.KeyPrefix = p.OrgID, p.TeamID, p.KeyID, p.KeyPrefix
	r = r.WithContext(s.callerContext(r.Context(), p))
	if names := p.Guardrails(); rt.guards.Applies(config.StageInput, names) || rt.guards.Applies(config.StageOutput, names) {
		fail(http.StatusBadRequest, codeInvalidRequest, "Live sessions are not available when guardrails apply, since audio cannot be checked")
		return
	}
	if status, code, msg, ok := s.admit(w, r, p, rec.RequestID); !ok {
		fail(status, code, msg)
		return
	}
	// Keys, not cookies, authenticate sessions, so any origin may connect.
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		rec.Status, rec.ErrorCode = http.StatusBadRequest, codeInvalidRequest
		return
	}
	defer client.CloseNow()
	limit := rt.cfg.Limits.MaxRequestBytes
	client.SetReadLimit(limit)
	refuse := func(status int, code, msg string) {
		rec.Status, rec.ErrorCode, rec.ErrorMessage = status, code, msg
		if len(msg) > 120 {
			msg = msg[:120]
		}
		_ = client.Close(websocket.StatusPolicyViolation, msg)
	}

	// The first message must be the setup, which names the model.
	sctx, cancel := context.WithTimeout(r.Context(), liveSetupTimeout)
	_, first, err := client.Read(sctx)
	cancel()
	if err != nil {
		refuse(http.StatusBadRequest, codeInvalidRequest, "no setup message")
		return
	}
	model, rewrite := liveSetup(first)
	if rewrite == nil {
		refuse(http.StatusBadRequest, codeInvalidRequest, "the first message must be a setup naming a model")
		return
	}
	rec.Model = model
	if !p.AllowsModel(model) {
		refuse(http.StatusForbidden, codeModelNotAllowed, "this key may not use model "+model)
		return
	}
	if denied := s.modelDenied(r, p, string(ep), model, "", true); denied != "" {
		refuse(http.StatusForbidden, codePolicyDenied, denied)
		return
	}
	targets, err := rt.router.Targets(r.Context(), ep, model)
	if errors.Is(err, router.ErrResidency) {
		refuse(http.StatusForbidden, codeResidency, "model "+model+": "+err.Error())
		return
	}
	if err != nil {
		refuse(http.StatusNotFound, codeModelNotFound, "model "+model+": "+err.Error())
		return
	}
	up, t := s.dialLive(r.Context(), rt, rec, model, targets)
	if up == nil {
		refuse(http.StatusBadGateway, codeAllTargetsFailed, "all upstream targets failed")
		return
	}
	defer up.CloseNow()
	rec.Target, rec.Provider, rec.UpstreamModel = t.String(), t.Provider, t.Model
	rec.Status = http.StatusSwitchingProtocols
	rec.UpstreamLatency = time.Since(start)
	up.SetReadLimit(limit)
	if err := up.Write(r.Context(), websocket.MessageText, rewrite(t.Model)); err != nil {
		refuse(http.StatusBadGateway, codeAllTargetsFailed, "the upstream closed the session")
		return
	}

	sess := &realtimeSession{s: s, client: client, p: p, rec: rec, target: t, model: model, start: start, endpoint: ep}
	ctx = r.Context()
	errc := make(chan error, 2)
	go func() { errc <- relayAll(ctx, client, up, liveClientMessage) }()
	go func() { errc <- relayAll(ctx, up, client, sess.observeLive) }()
	timer := time.NewTimer(rt.cfg.Limits.MaxRealtimeSession.Std())
	defer timer.Stop()
	select {
	case err = <-errc:
	case <-timer.C:
		err = errSessionExpired
	}
	switch {
	case errors.Is(err, errBudgetSpent):
		_ = client.Close(websocket.StatusPolicyViolation, "budget exceeded")
	case errors.Is(err, errSessionExpired):
		_ = client.Close(websocket.StatusPolicyViolation, "session expired")
	case websocket.CloseStatus(err) != -1:
		_ = client.Close(websocket.CloseStatus(err), "")
	default:
		_ = client.Close(websocket.StatusGoingAway, "")
	}
	_ = up.Close(websocket.StatusNormalClosure, "")
	sess.mu.Lock()
	rec.Tokens, rec.CostUSD = sess.total, sess.cost
	sess.mu.Unlock()
	rec.Unpriced = t.Price.IsZero() && rec.Tokens.Total() > 0
}

// dialLive opens a Live session on the first target that accepts it.
func (s *Server) dialLive(ctx context.Context, rt *runtime, rec *requestLog, model string, targets []config.Target) (*websocket.Conn, config.Target) {
	for _, t := range targets {
		d, ok := rt.router.LiveDialer(ctx, t.Provider)
		if !ok {
			continue
		}
		begin := time.Now()
		conn, resp, err := d.DialLive(ctx, http.Header{})
		s.metrics.ObserveUpstream(t.String(), time.Since(begin))
		if err == nil {
			return conn, t
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		}
		rec.Fallbacks++
		s.metrics.ObserveFallback(model, t.String(), status)
		s.log.Warn("target failed", "request_id", rec.RequestID, "target", t.String(), "status", status, "err", err)
	}
	return nil, config.Target{}
}

// liveSetup reads a setup message's model, without its models/ prefix,
// and returns a function that writes the message again for an upstream
// model. It returns a nil function when the message is not a setup
// with a model.
func liveSetup(data []byte) (string, func(string) []byte) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(data, &msg) != nil || msg["setup"] == nil {
		return "", nil
	}
	var setup map[string]json.RawMessage
	var model string
	if json.Unmarshal(msg["setup"], &setup) != nil || json.Unmarshal(setup["model"], &model) != nil {
		return "", nil
	}
	if model = strings.TrimPrefix(model, "models/"); model == "" {
		return "", nil
	}
	return model, func(upstream string) []byte {
		setup["model"], _ = json.Marshal("models/" + strings.TrimPrefix(upstream, "models/"))
		msg["setup"], _ = json.Marshal(setup)
		out, _ := json.Marshal(msg)
		return out
	}
}

// liveClientMessage drops a second setup, which could name another
// model; everything else passes.
func liveClientMessage(data []byte) []byte {
	// A setup key is in plain text unless it uses \u escapes.
	if bytes.Contains(data, []byte(`"setup"`)) || bytes.Contains(data, []byte(`\u`)) {
		var msg map[string]json.RawMessage
		if json.Unmarshal(data, &msg) == nil && msg["setup"] != nil {
			return []byte{}
		}
	}
	return data
}

// relayAll copies messages from src to dst, text and binary alike,
// since the Live API sends JSON in binary frames. edit sees every
// message: nil ends the relay with errBudgetSpent, and an empty result
// drops the message.
func relayAll(ctx context.Context, src, dst *websocket.Conn, edit func([]byte) []byte) error {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if data = edit(data); data == nil {
			return errBudgetSpent
		}
		if len(data) == 0 {
			continue
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return err
		}
	}
}

// liveUsage is a Live server message's usageMetadata.
type liveUsage struct {
	Prompt   int64 `json:"promptTokenCount"`
	Response int64 `json:"responseTokenCount"`
	Cached   int64 `json:"cachedContentTokenCount"`
	Thoughts int64 `json:"thoughtsTokenCount"`
	ToolUse  int64 `json:"toolUsePromptTokenCount"`
	PromptBy []struct {
		Modality string `json:"modality"`
		Count    int64  `json:"tokenCount"`
	} `json:"promptTokensDetails"`
	ResponseBy []struct {
		Modality string `json:"modality"`
		Count    int64  `json:"tokenCount"`
	} `json:"responseTokensDetails"`
}

// observeLive charges server messages that carry usageMetadata, which
// the Live API sends for each turn. It returns nil once a charge spends
// a budget, after passing the message on.
func (rs *realtimeSession) observeLive(data []byte) []byte {
	if !bytes.Contains(data, []byte(`"usageMetadata"`)) {
		return data
	}
	var msg struct {
		Usage *liveUsage `json:"usageMetadata"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.Usage == nil {
		return data
	}
	u := msg.Usage
	tok := usage.Tokens{
		InputTokens: u.Prompt + u.ToolUse, OutputTokens: u.Response + u.Thoughts, CachedInputTokens: u.Cached,
		ReasoningTokens: u.Thoughts, Reported: true,
	}
	for _, d := range u.PromptBy {
		if d.Modality == "AUDIO" {
			tok.AudioInputTokens += d.Count
		}
	}
	for _, d := range u.ResponseBy {
		if d.Modality == "AUDIO" {
			tok.AudioOutputTokens += d.Count
		}
	}
	if rs.charge(tok) {
		_ = rs.writeClientRaw(data)
		return nil
	}
	return data
}

// writeClientRaw sends a message as the Live API does, in a binary
// frame.
func (rs *realtimeSession) writeClientRaw(data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return rs.client.Write(ctx, websocket.MessageBinary, data)
}
