package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

// bidiDrain is how long a stream may keep sending output, such as its
// last usage, after the client has closed.
const bidiDrain = 5 * time.Second

// bidi relays a Bedrock bidirectional stream, as Nova Sonic uses, over
// a WebSocket: each message either way is one event's JSON, the
// payload Bedrock carries base64 encoded in its event stream. The path
// names a virtual model, the gateway signs the upstream stream, and
// each usageEvent is charged as it passes
// (docs/modalities.md#nova-sonic).
func (s *Server) bidi(w http.ResponseWriter, r *http.Request) {
	const ep = provider.BidiStream
	start := time.Now()
	rt := s.rt.Load()
	rec := &requestLog{RequestID: r.Header.Get("X-Request-Id"), Endpoint: string(ep)}
	ctx := telemetry.Propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, span := telemetry.Tracer().Start(ctx, "gateway.request", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", telemetry.Operation(ep)),
			attribute.String("http.route", "/bedrock/model/{model}/invoke-with-bidirectional-stream"),
			attribute.String("gog.request_id", rec.RequestID),
		))
	r = r.WithContext(ctx)
	defer func() { s.finish(rt, rec, span, start) }()
	fail := func(status int, code, msg string) {
		rec.Status, rec.ErrorCode = status, code
		writeError(w, ep, status, code, msg)
	}

	// Browsers cannot set headers on a WebSocket, so the key may also
	// come in the query.
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
	model := r.PathValue("model")
	rec.Model = model
	if names := p.Guardrails(); rt.guards.Applies(config.StageInput, names) || rt.guards.Applies(config.StageOutput, names) {
		fail(http.StatusBadRequest, codeInvalidRequest, "speech sessions are not available when guardrails apply, since audio cannot be checked")
		return
	}
	if !p.AllowsModel(model) {
		fail(http.StatusForbidden, codeModelNotAllowed, "this key may not use model "+model)
		return
	}
	if denied := s.modelDenied(r, p, string(ep), model, "", true); denied != "" {
		fail(http.StatusForbidden, codePolicyDenied, denied)
		return
	}
	targets, err := rt.router.Targets(r.Context(), ep, model)
	if errors.Is(err, router.ErrResidency) {
		fail(http.StatusForbidden, codeResidency, "model "+model+": "+err.Error())
		return
	}
	if err != nil {
		fail(http.StatusNotFound, codeModelNotFound, "model "+model+": "+err.Error())
		return
	}
	if status, code, msg, ok := s.admit(w, r, p, rec.RequestID); !ok {
		fail(status, code, msg)
		return
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		rec.Status, rec.ErrorCode = http.StatusBadRequest, codeInvalidRequest
		return
	}
	defer client.CloseNow()
	limit := rt.cfg.Limits.MaxRequestBytes
	client.SetReadLimit(limit)
	refuse := func(status int, code, msg string) {
		rec.Status, rec.ErrorCode = status, code
		if len(msg) > 120 {
			msg = msg[:120]
		}
		_ = client.Close(websocket.StatusPolicyViolation, msg)
	}

	// The stream opens with the client's first event, usually
	// sessionStart, so the upstream has input when it answers.
	sctx, cancel := context.WithTimeout(r.Context(), liveSetupTimeout)
	_, first, err := client.Read(sctx)
	cancel()
	if err != nil || !json.Valid(first) {
		refuse(http.StatusBadRequest, codeInvalidRequest, "the first message must be an event")
		return
	}
	up, t := s.openBidi(r.Context(), rt, rec, model, targets, first)
	if up == nil {
		refuse(http.StatusBadGateway, codeAllTargetsFailed, "all upstream targets failed")
		return
	}
	defer up.Close()
	rec.Target, rec.Provider, rec.UpstreamModel = t.String(), t.Provider, t.Model
	rec.Status = http.StatusSwitchingProtocols
	rec.UpstreamLatency = time.Since(start)

	sess := &realtimeSession{s: s, client: client, p: p, rec: rec, target: t, model: model, start: start, endpoint: ep}
	ctx = r.Context()
	fromClient := make(chan error, 1)
	fromUp := make(chan error, 1)
	go func() { fromClient <- bidiSend(ctx, client, up) }()
	go func() { fromUp <- sess.bidiReceive(ctx, up) }()
	timer := time.NewTimer(rt.cfg.Limits.MaxRealtimeSession.Std())
	defer timer.Stop()
	select {
	case err = <-fromClient:
		// The client is done: end the input and collect what is left,
		// such as the final usage.
		_ = up.CloseSend()
		select {
		case <-fromUp:
		case <-time.After(bidiDrain):
		}
	case err = <-fromUp:
	case <-timer.C:
		err = errSessionExpired
	}
	var upErr *bidiUpstreamError
	switch {
	case errors.Is(err, errBudgetSpent):
		_ = client.Close(websocket.StatusPolicyViolation, "budget exceeded")
	case errors.Is(err, errSessionExpired):
		_ = client.Close(websocket.StatusPolicyViolation, "session expired")
	case errors.Is(err, io.EOF):
		_ = client.Close(websocket.StatusNormalClosure, "")
	case errors.As(err, &upErr):
		msg := upErr.Error()
		if len(msg) > 120 {
			msg = msg[:120]
		}
		_ = client.Close(websocket.StatusInternalError, msg)
	case websocket.CloseStatus(err) != -1:
		_ = client.Close(websocket.CloseStatus(err), "")
	default:
		_ = client.Close(websocket.StatusGoingAway, "")
	}
	sess.mu.Lock()
	rec.Tokens, rec.CostUSD = sess.total, sess.cost
	sess.mu.Unlock()
	rec.Unpriced = t.Price.IsZero() && rec.Tokens.Total() > 0
}

// openBidi opens the stream on the first target that accepts it.
func (s *Server) openBidi(ctx context.Context, rt *runtime, rec *requestLog, model string, targets []config.Target, first []byte) (provider.Bidi, config.Target) {
	for _, t := range targets {
		o, ok := rt.router.BidiOpener(ctx, t.Provider)
		if !ok {
			continue
		}
		begin := time.Now()
		up, status, err := o.OpenBidi(ctx, t.Model, first)
		s.metrics.ObserveUpstream(t.String(), time.Since(begin))
		if err == nil {
			return up, t
		}
		rec.Fallbacks++
		s.metrics.ObserveFallback(model, t.String(), status)
		s.log.Warn("target failed", "request_id", rec.RequestID, "target", t.String(), "status", status, "err", err)
	}
	return nil, config.Target{}
}

// bidiSend copies the client's events to the stream.
func bidiSend(ctx context.Context, client *websocket.Conn, up provider.Bidi) error {
	for {
		_, data, err := client.Read(ctx)
		if err != nil {
			return err
		}
		if err := up.Send(data); err != nil {
			return &bidiUpstreamError{err}
		}
	}
}

// bidiUpstreamError is a stream that failed upstream.
type bidiUpstreamError struct{ err error }

func (e *bidiUpstreamError) Error() string { return e.err.Error() }

// bidiReceive copies the stream's events to the client and charges
// usage. Once the client is gone it keeps reading, so that usage sent
// at the end is still charged.
func (rs *realtimeSession) bidiReceive(ctx context.Context, up provider.Bidi) error {
	clientOK := true
	for {
		data, err := up.Recv()
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		if err != nil {
			return &bidiUpstreamError{err}
		}
		spent := rs.observeBidi(data)
		if clientOK {
			if err := rs.client.Write(ctx, websocket.MessageText, data); err != nil {
				clientOK = false
			}
		}
		if spent {
			return errBudgetSpent
		}
	}
}

// sonicTokens are a usageEvent's speech and text tokens one way.
type sonicTokens struct {
	Speech int64 `json:"speechTokens"`
	Text   int64 `json:"textTokens"`
}

// observeBidi charges a Nova Sonic usageEvent by its delta, the tokens
// since the last one. It reports whether a budget is spent.
func (rs *realtimeSession) observeBidi(data []byte) bool {
	if !bytes.Contains(data, []byte(`"usageEvent"`)) {
		return false
	}
	var msg struct {
		Event struct {
			Usage *struct {
				Details struct {
					Delta struct {
						Input  sonicTokens `json:"input"`
						Output sonicTokens `json:"output"`
					} `json:"delta"`
				} `json:"details"`
			} `json:"usageEvent"`
		} `json:"event"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.Event.Usage == nil {
		return false
	}
	d := msg.Event.Usage.Details.Delta
	tok := usage.Tokens{
		InputTokens: d.Input.Speech + d.Input.Text, OutputTokens: d.Output.Speech + d.Output.Text,
		AudioInputTokens: d.Input.Speech, AudioOutputTokens: d.Output.Speech, Reported: true,
	}
	if tok.Total() == 0 {
		return false
	}
	return rs.charge(tok)
}
