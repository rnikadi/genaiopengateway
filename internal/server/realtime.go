package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// keyProtocol is the subprotocol prefix browsers use to send a key,
// since they cannot set headers on a WebSocket.
const keyProtocol = "openai-insecure-api-key."

var (
	errBudgetSpent    = errors.New("a budget is spent")
	errSessionExpired = errors.New("the session reached limits.max_realtime_session")
)

// realtimeKey returns the key a realtime client presented, and the
// subprotocols it offered other than the key.
func realtimeKey(r *http.Request) (string, []string) {
	key := presentedKey(r)
	var rest []string
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			p = strings.TrimSpace(p)
			if k, ok := strings.CutPrefix(p, keyProtocol); ok {
				if key == "" {
					key = k
				}
				continue
			}
			if p != "" {
				rest = append(rest, p)
			}
		}
	}
	return key, rest
}

// realtime relays an OpenAI realtime session to one target, charging
// each response as it finishes (docs/modalities.md).
func (s *Server) realtime(w http.ResponseWriter, r *http.Request) {
	const ep = provider.Realtime
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

	key, protocols := realtimeKey(r)
	p, ok := s.keys.Load().Lookup(key)
	if !ok || p.Expired(time.Now()) {
		fail(http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
		return
	}
	rec.OrgID, rec.TeamID, rec.KeyID, rec.KeyPrefix = p.OrgID, p.TeamID, p.KeyID, p.KeyPrefix
	r = r.WithContext(s.teamContext(r.Context(), p.TeamID))
	model := r.URL.Query().Get("model")
	if model == "" {
		fail(http.StatusBadRequest, codeInvalidRequest, "the model query parameter is required")
		return
	}
	rec.Model = model
	if !p.AllowsModel(model) {
		fail(http.StatusForbidden, codeModelNotAllowed, "this key may not use model "+model)
		return
	}
	if names := p.Guardrails(); rt.guards.Applies(config.StageInput, names) || rt.guards.Applies(config.StageOutput, names) {
		fail(http.StatusBadRequest, codeInvalidRequest, "realtime sessions are not available when guardrails apply, since audio cannot be checked")
		return
	}
	targets, err := rt.router.Targets(ep, model)
	if err != nil {
		fail(http.StatusNotFound, codeModelNotFound, "model "+model+": "+err.Error())
		return
	}
	if status, code, msg, ok := s.admit(w, r, p, rec.RequestID); !ok {
		fail(status, code, msg)
		return
	}

	up, t, refused := s.dialRealtime(r.Context(), rt, rec, model, targets, r.Header, protocols)
	if up == nil {
		if refused != nil {
			// The last target's own answer, as for model requests.
			defer refused.Body.Close()
			rec.Status = refused.StatusCode
			w.Header().Set("Content-Type", refused.Header.Get("Content-Type"))
			w.WriteHeader(refused.StatusCode)
			_, _ = io.Copy(w, io.LimitReader(refused.Body, 1<<20))
			return
		}
		fail(http.StatusBadGateway, codeAllTargetsFailed, "all upstream targets failed")
		return
	}
	defer up.CloseNow()
	rec.Target, rec.Provider, rec.UpstreamModel = t.String(), t.Provider, t.Model
	var accept []string
	if sp := up.Subprotocol(); sp != "" {
		accept = []string{sp}
	}
	// Keys, not cookies, authenticate sessions, so any origin may connect.
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: accept, InsecureSkipVerify: true})
	if err != nil {
		rec.Status, rec.ErrorCode = http.StatusBadRequest, codeInvalidRequest
		return
	}
	defer client.CloseNow()
	rec.Status = http.StatusSwitchingProtocols
	rec.UpstreamLatency = time.Since(start)
	limit := rt.cfg.Limits.MaxRequestBytes
	client.SetReadLimit(limit)
	up.SetReadLimit(limit)

	sess := &realtimeSession{s: s, client: client, p: p, rec: rec, target: t, model: model, start: start}
	// A context ending closes a connection, so the session's lifetime is
	// a timer rather than a deadline on the relays.
	ctx = r.Context()
	errc := make(chan error, 2)
	go func() {
		errc <- relay(ctx, client, up, func(data []byte) []byte { return withSessionModel(data, t.Model) })
	}()
	go func() { errc <- relay(ctx, up, client, sess.observe) }()
	timer := time.NewTimer(rt.cfg.Limits.MaxRealtimeSession.Std())
	defer timer.Stop()
	select {
	case err = <-errc:
	case <-timer.C:
		err = errSessionExpired
	}
	switch {
	case errors.Is(err, errBudgetSpent):
		sess.sendError("budget_exceeded", "a budget is spent; the session is closed")
		_ = client.Close(websocket.StatusPolicyViolation, "budget exceeded")
	case errors.Is(err, errSessionExpired):
		sess.sendError("session_expired", "the session reached its maximum length")
		_ = client.Close(websocket.StatusPolicyViolation, "session expired")
	case websocket.CloseStatus(err) != -1:
		// One side closed; pass its status to the other.
		_ = client.Close(websocket.CloseStatus(err), "")
	default:
		_ = client.Close(websocket.StatusGoingAway, "")
	}
	_ = up.Close(websocket.StatusNormalClosure, "")
	// The request line totals the session's responses.
	sess.mu.Lock()
	rec.Tokens, rec.CostUSD = sess.total, sess.cost
	sess.mu.Unlock()
	rec.Unpriced = t.Price.IsZero() && rec.Tokens.Total() > 0
}

// dialRealtime opens a session on the first target that accepts it.
// When none does, it returns the last target's refusal, if any.
func (s *Server) dialRealtime(ctx context.Context, rt *runtime, rec *requestLog, model string, targets []config.Target, h http.Header, protocols []string) (*websocket.Conn, config.Target, *http.Response) {
	hdr := http.Header{}
	if v := h.Values("Openai-Beta"); len(v) > 0 {
		hdr["Openai-Beta"] = v
	}
	for i, t := range targets {
		d, ok := rt.router.Dialer(ctx, t.Provider)
		if !ok {
			continue
		}
		begin := time.Now()
		conn, resp, err := d.DialRealtime(ctx, t.Model, hdr, protocols)
		s.metrics.ObserveUpstream(t.String(), time.Since(begin))
		if err == nil {
			return conn, t, nil
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
			if resp.Body == nil {
				resp.Body = http.NoBody
			}
		}
		if status != 0 && (!provider.Retryable(status) || i == len(targets)-1) {
			return nil, t, resp
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		rec.Fallbacks++
		s.metrics.ObserveFallback(model, t.String(), status)
		s.log.Warn("target failed", "request_id", rec.RequestID, "target", t.String(), "status", status, "err", err)
	}
	return nil, config.Target{}, nil
}

// relay copies messages from src to dst until either side fails. edit
// may change or inspect each text message; a nil result ends the relay
// with errBudgetSpent.
func relay(ctx context.Context, src, dst *websocket.Conn, edit func([]byte) []byte) error {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if typ == websocket.MessageText {
			if data = edit(data); data == nil {
				return errBudgetSpent
			}
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return err
		}
	}
}

// withSessionModel replaces a model named in a session.update event with
// the target's, so a client cannot leave the virtual model.
func withSessionModel(data []byte, model string) []byte {
	if !bytes.Contains(data, []byte(`"session.update"`)) {
		return data
	}
	var ev map[string]json.RawMessage
	if json.Unmarshal(data, &ev) != nil || string(ev["type"]) != `"session.update"` {
		return data
	}
	var sess map[string]json.RawMessage
	if json.Unmarshal(ev["session"], &sess) != nil || sess["model"] == nil {
		return data
	}
	sess["model"], _ = json.Marshal(model)
	ev["session"], _ = json.Marshal(sess)
	out, err := json.Marshal(ev)
	if err != nil {
		return data
	}
	return out
}

// realtimeSession charges one session's responses.
type realtimeSession struct {
	s      *Server
	client *websocket.Conn
	p      *auth.Principal
	rec    *requestLog
	target config.Target
	model  string
	start  time.Time

	mu    sync.Mutex
	total usage.Tokens
	cost  float64
}

// realtimeUsage is the usage of a realtime response.done event.
type realtimeUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	InputTokenDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
		AudioTokens  int64 `json:"audio_tokens"`
	} `json:"input_token_details"`
	OutputTokenDetails struct {
		AudioTokens int64 `json:"audio_tokens"`
	} `json:"output_token_details"`
}

// observe charges response.done events as they pass to the client. It
// returns nil once a charge spends a budget, after passing nothing on.
func (rs *realtimeSession) observe(data []byte) []byte {
	if !bytes.Contains(data, []byte(`"response.done"`)) {
		return data
	}
	var ev struct {
		Type     string `json:"type"`
		Response struct {
			Usage *realtimeUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.Type != "response.done" || ev.Response.Usage == nil {
		return data
	}
	u := ev.Response.Usage
	tok := usage.Tokens{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedInputTokens: u.InputTokenDetails.CachedTokens,
		AudioInputTokens: u.InputTokenDetails.AudioTokens, AudioOutputTokens: u.OutputTokenDetails.AudioTokens, Reported: true,
	}
	cost := usage.Cost(tok, rs.target.Price)
	rs.mu.Lock()
	rs.total.InputTokens += tok.InputTokens
	rs.total.OutputTokens += tok.OutputTokens
	rs.total.CachedInputTokens += tok.CachedInputTokens
	rs.cost += cost
	rs.mu.Unlock()

	s := rs.s
	ctx := context.Background()
	spent, err := s.limiter.Charge(ctx, rs.p, tok.Total(), cost)
	if err != nil {
		s.log.Error("charging limits failed", "request_id", rs.rec.RequestID, "err", err)
	}
	s.budgetAlerts(ctx, rs.p, spent)
	if s.usage != nil {
		s.usage.Add(usage.Record{
			RequestID: rs.rec.RequestID, Time: time.Now(), OrgID: rs.p.OrgID, TeamID: rs.p.TeamID, KeyID: rs.p.KeyID,
			Endpoint: string(provider.Realtime), Model: rs.model, Provider: rs.target.Provider, UpstreamModel: rs.target.Model,
			Status: http.StatusOK, LatencyMS: time.Since(rs.start).Milliseconds(), Tokens: tok, CostUSD: cost,
		})
	}
	for _, sp := range spent {
		if sp.After >= sp.Budget.AmountUSD {
			// The client still gets the response it paid for.
			_ = rs.writeClient(data)
			return nil
		}
	}
	return data
}

// writeClient sends the client a message outside the relay loop.
func (rs *realtimeSession) writeClient(data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return rs.client.Write(ctx, websocket.MessageText, data)
}

// sendError tells the client why the gateway closes the session, as a
// realtime error event.
func (rs *realtimeSession) sendError(code, msg string) {
	ev, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "invalid_request_error", "code": code, "message": msg},
	})
	_ = rs.writeClient(ev)
}
