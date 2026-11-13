package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"sync"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/router"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
)

// Realtime over WebRTC (docs/modalities.md). The client sends its SDP
// offer to the gateway, which makes the call at the provider with the
// provider's key and returns the answer. Audio then runs between the
// client and the provider. The gateway stays on the call through the
// provider's WebSocket for it, which carries the call's events: it
// charges each response, and hangs up when a budget is spent or the
// session is too long.

var (
	errCallModel = errors.New("the session's model was changed")
	errCallEnded = errors.New("the caller hung up")
	errShutdown  = errors.New("the gateway is shutting down")
)

// callEndCode is the error code a request line carries for a call the
// gateway ended. A caller's own hangup is not an error.
func callEndCode(why error) string {
	switch {
	case errors.Is(why, errBudgetSpent):
		return "budget_exceeded"
	case errors.Is(why, errSessionExpired):
		return "session_expired"
	case errors.Is(why, errCallModel):
		return "model_changed"
	case errors.Is(why, errShutdown):
		return "gateway_shutdown"
	}
	return ""
}

// maxSDP bounds an SDP offer, which is a few kilobytes.
const maxSDP = 256 << 10

// rtcCall is one call a replica watches.
type rtcCall struct {
	id    string
	keyID string
	// end stops the watch with the reason the gateway ends the call.
	end context.CancelCauseFunc
	// done is closed when the call is over and recorded.
	done chan struct{}
}

// callOffer is a client's request for a call.
type callOffer struct {
	sdp     []byte
	session map[string]json.RawMessage
	model   string
}

// readCallOffer reads an offer sent as application/sdp, with the model
// in the query, or as a form with sdp and session parts.
func readCallOffer(r *http.Request) (*callOffer, string) {
	o := &callOffer{model: r.URL.Query().Get("model")}
	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	body := http.MaxBytesReader(nil, r.Body, maxSDP)
	switch mt {
	case "application/sdp":
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, "the SDP offer is too large"
		}
		o.sdp = b
	case "multipart/form-data":
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, "the form could not be read, or is too large"
			}
			b, err := io.ReadAll(part)
			if err != nil {
				return nil, "the form could not be read, or is too large"
			}
			switch part.FormName() {
			case "sdp":
				o.sdp = b
			case "session":
				if json.Unmarshal(b, &o.session) != nil {
					return nil, "session must be a JSON object"
				}
			}
		}
	default:
		return nil, "send the SDP offer as application/sdp, or as multipart/form-data with sdp and session parts"
	}
	if len(bytes.TrimSpace(o.sdp)) == 0 {
		return nil, "the SDP offer is required"
	}
	if raw, ok := o.session["model"]; ok {
		var m string
		if json.Unmarshal(raw, &m) != nil {
			return nil, "session.model must be a string"
		}
		if m != "" {
			o.model = m
		}
	}
	if o.model == "" {
		return nil, "name the model in session.model or in the model query parameter"
	}
	return o, ""
}

// form returns the offer as the provider takes it, with the target's
// model in the session.
func (o *callOffer) form(model string) ([]byte, string, error) {
	sess := map[string]json.RawMessage{"type": json.RawMessage(`"realtime"`)}
	for k, v := range o.session {
		sess[k] = v
	}
	sess["model"], _ = json.Marshal(model)
	js, err := json.Marshal(sess)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range []struct {
		name, typ string
		data      []byte
	}{{"sdp", "application/sdp", o.sdp}, {"session", "application/json", js}} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+f.name+`"`)
		h.Set("Content-Type", f.typ)
		pw, err := mw.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := pw.Write(f.data); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// allowAnyOrigin lets pages of any origin make calls, as any origin may
// open a realtime WebSocket: keys, not cookies, authenticate them.
func allowAnyOrigin(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Location, X-Request-Id")
}

// realtimeCallCORS answers a browser's preflight for a call.
func realtimeCallCORS(w http.ResponseWriter, r *http.Request) {
	allowAnyOrigin(w)
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// realtimeCall makes a realtime call over WebRTC at one target and
// watches it until it ends.
func (s *Server) realtimeCall(w http.ResponseWriter, r *http.Request) {
	const ep = provider.Realtime
	allowAnyOrigin(w)
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
	// A call that is made is recorded when it ends, with what it cost.
	watched := false
	defer func() {
		if !watched {
			s.finish(rt, rec, span, start)
		}
	}()
	fail := func(status int, code, msg string) {
		rec.Status, rec.ErrorCode, rec.ErrorMessage = status, code, msg
		writeError(w, ep, status, code, msg)
	}

	p, ok := s.keys.Load().Lookup(presentedKey(r))
	if !ok || p.Expired(time.Now()) {
		fail(http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
		return
	}
	rec.OrgID, rec.TeamID, rec.KeyID, rec.KeyPrefix = p.OrgID, p.TeamID, p.KeyID, p.KeyPrefix
	r = r.WithContext(s.callerContext(r.Context(), p))
	offer, problem := readCallOffer(r)
	if offer == nil {
		fail(http.StatusBadRequest, codeInvalidRequest, problem)
		return
	}
	model := offer.model
	rec.Model = model
	if !p.AllowsModel(model) {
		fail(http.StatusForbidden, codeModelNotAllowed, "this key may not use model "+model)
		return
	}
	if denied := s.modelDenied(r, p, string(ep), model, "", true); denied != "" {
		fail(http.StatusForbidden, codePolicyDenied, denied)
		return
	}
	if names := p.Guardrails(); rt.guards.Applies(config.StageInput, names) || rt.guards.Applies(config.StageOutput, names) {
		fail(http.StatusBadRequest, codeInvalidRequest, "realtime sessions are not available when guardrails apply, since audio cannot be checked")
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

	var (
		caller provider.RealtimeCaller
		target config.Target
		answer []byte
		callID string
	)
	for i, t := range targets {
		c, ok := rt.router.RealtimeCaller(r.Context(), t.Provider)
		if !ok {
			continue
		}
		body, contentType, err := offer.form(t.Model)
		if err != nil {
			fail(http.StatusBadRequest, codeInvalidRequest, "the offer could not be encoded")
			return
		}
		begin := time.Now()
		resp, err := c.Forward(r.Context(), http.MethodPost, "/realtime/calls", bytes.NewReader(body), contentType)
		s.metrics.ObserveUpstream(t.String(), time.Since(begin))
		status := 0
		if err == nil {
			status = resp.StatusCode
			if status == http.StatusCreated || status == http.StatusOK {
				answer, err = io.ReadAll(io.LimitReader(resp.Body, maxSDP))
				_ = resp.Body.Close()
				if err == nil {
					caller, target, callID = c, t, path.Base(resp.Header.Get("Location"))
					break
				}
				status = 0
			} else if !provider.Retryable(status) || i == len(targets)-1 {
				// The last target's own answer, as for model requests.
				defer resp.Body.Close()
				rec.Status = status
				w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
				w.WriteHeader(status)
				_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
				return
			} else {
				_ = resp.Body.Close()
			}
		}
		rec.Fallbacks++
		s.metrics.ObserveFallback(model, t.String(), status)
		s.log.Warn("target failed", "request_id", rec.RequestID, "target", t.String(), "status", status, "err", err)
	}
	if caller == nil {
		fail(http.StatusBadGateway, codeAllTargetsFailed, "all upstream targets failed")
		return
	}
	rec.Target, rec.Provider, rec.UpstreamModel = target.String(), target.Provider, target.Model
	// A call the gateway cannot watch would run unmetered, so it is not
	// handed to the client.
	if callID == "" || callID == "." || callID == "/" {
		s.log.Error("the provider named no call id; the call cannot be watched or ended", "request_id", rec.RequestID, "target", target.String())
		fail(http.StatusBadGateway, codeAllTargetsFailed, "the provider did not name the call")
		return
	}
	events, resp, err := caller.DialRealtimeCall(r.Context(), callID)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		s.hangUp(caller, callID, rec.RequestID)
		s.log.Error("opening the call's events failed; the call was ended", "request_id", rec.RequestID, "target", target.String(), "err", err)
		fail(http.StatusBadGateway, codeAllTargetsFailed, "the call's events could not be opened at the provider")
		return
	}
	events.SetReadLimit(rt.cfg.Limits.MaxRequestBytes)

	wctx, end := context.WithCancelCause(context.Background())
	call := &rtcCall{id: callID, keyID: p.KeyID, end: end, done: make(chan struct{})}
	s.rtc.Store(callID, call)
	watched = true
	rec.Status = http.StatusCreated
	rec.UpstreamLatency = time.Since(start)
	sess := &realtimeSession{s: s, p: p, rec: rec, target: target, model: model, start: start}
	maxLen := rt.cfg.Limits.MaxRealtimeSession.Std()
	go func() {
		defer close(call.done)
		defer s.rtc.Delete(callID)
		defer end(nil)
		why := s.watchCall(wctx, events, sess, maxLen)
		if why != nil {
			// The gateway ends the call; otherwise it ended by itself.
			s.hangUp(caller, callID, rec.RequestID)
			rec.ErrorCode = callEndCode(why)
		}
		_ = events.CloseNow()
		sess.mu.Lock()
		rec.Tokens, rec.CostUSD = sess.total, sess.cost
		sess.mu.Unlock()
		rec.Unpriced = target.Price.IsZero() && rec.Tokens.Total() > 0
		s.finish(rt, rec, span, start)
	}()

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/v1/realtime/calls/"+url.PathEscape(callID))
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(answer)
}

// watchCall reads a call's events until the call ends, charging each
// response. It returns why the gateway must end the call, or nil when
// the call ended by itself.
func (s *Server) watchCall(ctx context.Context, events *websocket.Conn, sess *realtimeSession, maxLen time.Duration) error {
	ctx, cancel := context.WithTimeoutCause(ctx, maxLen, errSessionExpired)
	defer cancel()
	// The model as the provider names it, which can be a dated name of
	// the target's.
	var model string
	for {
		_, data, err := events.Read(ctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return nil
		}
		if tok, ok := responseDoneUsage(data); ok {
			if sess.charge(tok) {
				return errBudgetSpent
			}
			continue
		}
		if !bytes.Contains(data, []byte(`"session.`)) {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Session struct {
				Model string `json:"model"`
			} `json:"session"`
		}
		if json.Unmarshal(data, &ev) != nil || ev.Session.Model == "" {
			continue
		}
		switch ev.Type {
		case "session.created":
			model = ev.Session.Model
		case "session.updated":
			// The client talks to the provider directly, so the gateway
			// cannot rewrite a session.update as it does on a WebSocket.
			if model == "" {
				model = ev.Session.Model
			} else if ev.Session.Model != model {
				return errCallModel
			}
		}
	}
}

// hangUp ends a call at the provider.
func (s *Server) hangUp(c provider.RealtimeCaller, callID, requestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := c.Forward(ctx, http.MethodPost, "/realtime/calls/"+url.PathEscape(callID)+"/hangup", nil, "")
	if err != nil {
		s.log.Error("hanging up a realtime call failed", "request_id", requestID, "call_id", callID, "err", err)
		return
	}
	defer resp.Body.Close()
	// A call that is already over has nothing to hang up.
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		s.log.Warn("hanging up a realtime call was refused", "request_id", requestID, "call_id", callID, "status", resp.StatusCode)
	}
}

// realtimeHangup ends a call for the key that made it.
func (s *Server) realtimeHangup(w http.ResponseWriter, r *http.Request) {
	const ep = provider.Realtime
	allowAnyOrigin(w)
	p, ok := s.keys.Load().Lookup(presentedKey(r))
	if !ok || p.Expired(time.Now()) {
		writeError(w, ep, http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
		return
	}
	call := s.callOf(p, r.PathValue("call_id"))
	if call == nil {
		// Also a call another replica watches: closing the peer
		// connection ends it wherever it is watched.
		writeError(w, ep, http.StatusNotFound, codeNotFound, "no such call on this replica; closing the peer connection ends a call too")
		return
	}
	call.end(errCallEnded)
	select {
	case <-call.done:
	case <-r.Context().Done():
	}
	w.WriteHeader(http.StatusOK)
}

// callOf returns a call the key made, or nil.
func (s *Server) callOf(p *auth.Principal, id string) *rtcCall {
	v, ok := s.rtc.Load(id)
	if !ok {
		return nil
	}
	if call := v.(*rtcCall); call.keyID == p.KeyID {
		return call
	}
	return nil
}

// endRealtimeCalls hangs up the calls this replica watches, since no
// one would meter them after it stops.
func (s *Server) endRealtimeCalls(ctx context.Context) {
	var wg sync.WaitGroup
	s.rtc.Range(func(_, v any) bool {
		call := v.(*rtcCall)
		call.end(errShutdown)
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-call.done:
			case <-ctx.Done():
			}
		}()
		return true
	})
	wg.Wait()
}
