package mcpgw

//lint:file-ignore SA1019 Sampling is deprecated by MCP 2026-07-28 but servers may use it for at least a year, and passing it on is opt-in.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/id"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
)

// Requests for input (docs/mcp.md#requests-for-input). A server may ask
// the client for something while it serves a call: input from the user
// (elicitation) or a completion from the client's model (sampling). The
// gateway passes such a request on only for servers whose
// client_requests names it, and only to clients on MCP 2026-07-28 or
// later, which get it as an input_required result and call again with
// the answer. That keeps the gateway stateless towards clients.
//
// A server on 2026-07-28 asks the same way, so its requests and request
// state pass through. An older server asks with a request of its own in
// the middle of the call. The gateway then holds that call open on this
// replica, hands the client an input_required result, and gives the
// server the answer when the client calls again.

const (
	// mrtrVersion is the first protocol version with input_required results.
	mrtrVersion = "2026-07-28"
	// inputWait is how long a held call waits for the client's answer.
	inputWait = 5 * time.Minute
	// maxHeld bounds the calls that wait for an answer on this replica.
	maxHeld = 1000
	// heldPrefix marks request state that names a held call.
	heldPrefix = "gog-held:"
	// heldInput is the id of a held call's one input request.
	heldInput = "input"
	// idleExclusive is how many sessions of a server are kept for reuse
	// by held calls.
	idleExclusive = 4
)

// input is what a client sends when it calls again with answers.
type input struct {
	responses mcp.InputResponseMap
	state     string
}

// needInput is what a call hands the client when it cannot finish
// without input.
type needInput struct {
	requests mcp.InputRequestMap
	state    string
}

// inputError says why a request for input was not passed on.
type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }

func (e *inputError) rpcError() error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidRequest, Message: e.msg}
}

// rpcInput turns an inputError into a JSON-RPC error, for methods whose
// results cannot carry an error.
func rpcInput(err error) error {
	var ie *inputError
	if errors.As(err, &ie) {
		return ie.rpcError()
	}
	return err
}

// held is an upstream call that stays open while the client answers a
// request the server made.
type held struct {
	id     string
	owner  string
	u      *upstream
	ctx    context.Context
	cancel context.CancelCauseFunc
	asks   chan *ask
	done   chan struct{}
	res    any
	err    error

	mu    sync.Mutex
	timer *time.Timer
	cur   *ask // the request the client is answering
}

// ask is one request from the server of a held call.
type ask struct {
	kind   string
	params mcp.InputRequest
	reply  chan mcp.InputResponse
}

// owner identifies who may answer a held call.
func owner(c *call) string {
	if c.caller == nil {
		return ""
	}
	return c.caller.Principal.KeyID + "\x00" + c.caller.Subject
}

// kindOf names what an input request asks for, or "" for requests the
// gateway never passes on.
func kindOf(r mcp.InputRequest) (kind, method string) {
	switch r.(type) {
	case *mcp.ElicitParams:
		return config.MCPElicitation, "elicitation/create"
	case *mcp.CreateMessageParams, *mcp.CreateMessageWithToolsParams:
		return config.MCPSampling, "sampling/createMessage"
	}
	return "", ""
}

// permit checks that the client can be handed the server's requests,
// and records each in the audit trail.
func (g *Gateway) permit(u *upstream, c *call, need *needInput) error {
	if c.protocolVersion < mrtrVersion {
		return &inputError{"Server " + u.name + " asked for input. The gateway passes such requests on only to clients that use MCP " + mrtrVersion + " or later."}
	}
	for _, r := range need.requests {
		kind, _ := kindOf(r)
		if kind == "" || !u.cfg.Relays(kind) {
			if kind == "" {
				kind = "something the gateway does not pass on"
			}
			return &inputError{"Server " + u.name + " asked the client for " + kind + ", which the gateway is not set to pass on for this server."}
		}
	}
	for _, r := range need.requests {
		if _, method := kindOf(r); c.caller != nil {
			g.Record(c.caller, c.virtualServer, method, u.name, c.Name, telemetry.MCPOk, 0, 0)
		}
	}
	return nil
}

// converse sends one request upstream that the server may answer by
// asking for input. It returns the result, or what the client must
// answer first.
func converse[R any](ctx context.Context, g *Gateway, u *upstream, method, name string, in input,
	f func(context.Context, *mcp.ClientSession, mcp.Meta) (R, error),
	needs func(R) (bool, mcp.InputRequestMap, string),
) (res R, need *needInput, err error) {
	c := callFrom(ctx)
	var h *held
	switch {
	case strings.HasPrefix(in.state, heldPrefix):
		if h, err = g.resume(c, u, in); err != nil {
			return res, nil, err
		}
	case u.holds():
		h = g.hold(ctx, u, c, func(ctx context.Context) (any, error) {
			var r R
			err := g.forward(ctx, u, method, name, func(ctx context.Context, cs *mcp.ClientSession, meta mcp.Meta) (err error) {
				r, err = f(ctx, cs, meta)
				return err
			})
			return r, err
		})
	default:
		err = g.forward(ctx, u, method, name, func(ctx context.Context, cs *mcp.ClientSession, meta mcp.Meta) (err error) {
			res, err = f(ctx, cs, meta)
			return err
		})
		if err != nil {
			return res, nil, err
		}
		if asks, requests, state := needs(res); asks {
			need = &needInput{requests: requests, state: state}
			if need.requests == nil {
				need.requests = mcp.InputRequestMap{}
			}
			if err := g.permit(u, c, need); err != nil {
				var zero R
				return zero, nil, err
			}
		}
		return res, need, nil
	}
	v, need, err := g.await(ctx, c, h)
	res, _ = v.(R)
	return res, need, err
}

// holds reports whether calls to u are held open for the server's
// requests: it may make some, and it is on a protocol version that
// makes them in the middle of a call.
func (u *upstream) holds() bool {
	if len(u.cfg.ClientRequests) == 0 {
		return false
	}
	c := u.catalog.Load()
	return c == nil || c.protocolVersion < mrtrVersion
}

// hold starts run as a held call. The call ends with its own deadline,
// not with the client's request.
func (g *Gateway) hold(ctx context.Context, u *upstream, c *call, run func(context.Context) (any, error)) *held {
	h := &held{id: id.New("mcpheld"), owner: owner(c), u: u, asks: make(chan *ask), done: make(chan struct{})}
	base := context.WithValue(trace.ContextWithSpan(context.Background(), trace.SpanFromContext(ctx)), callKey{}, &call{
		caller: c.caller, virtualServer: c.virtualServer, protocolVersion: c.protocolVersion, held: h,
	})
	h.ctx, h.cancel = context.WithCancelCause(base)
	h.timer = time.AfterFunc(u.cfg.Timeout.Std(), func() { h.cancel(context.DeadlineExceeded) })
	go func() {
		h.res, h.err = run(h.ctx)
		h.timer.Stop()
		h.cancel(nil)
		close(h.done)
		g.heldMu.Lock()
		delete(g.held, h.id)
		g.heldMu.Unlock()
	}()
	return h
}

// await returns when a held call ends or asks the client for input.
func (g *Gateway) await(ctx context.Context, c *call, h *held) (any, *needInput, error) {
	select {
	case <-h.done:
		return h.res, nil, h.err
	case <-ctx.Done():
		h.cancel(ctx.Err())
		return nil, nil, ctx.Err()
	case a := <-h.asks:
		need := &needInput{requests: mcp.InputRequestMap{heldInput: a.params}, state: heldPrefix + h.id}
		if err := g.permit(h.u, c, need); err != nil {
			h.cancel(err)
			return nil, nil, err
		}
		g.heldMu.Lock()
		full := len(g.held) >= maxHeld
		if !full {
			h.mu.Lock()
			h.cur = a
			h.timer.Reset(inputWait)
			h.mu.Unlock()
			g.held[h.id] = h
		}
		g.heldMu.Unlock()
		if full {
			h.cancel(errors.New("too many calls wait for input"))
			return nil, nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "too many calls are waiting for input; try again later"}
		}
		return nil, need, nil
	}
}

// resume gives a held call the client's answer.
func (g *Gateway) resume(c *call, u *upstream, in input) (*held, error) {
	g.heldMu.Lock()
	h := g.held[strings.TrimPrefix(in.state, heldPrefix)]
	if h != nil && (h.owner != owner(c) || h.u != u) {
		h = nil
	}
	if h != nil {
		delete(g.held, h.id)
	}
	g.heldMu.Unlock()
	if h == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "the call that asked for input is no longer waiting: it timed out, or another gateway replica holds it. Make the call again from the start."}
	}
	resp := in.responses[heldInput]
	if resp == nil {
		h.cancel(errors.New("the client sent no answer"))
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "inputResponses must answer the request " + heldInput}
	}
	h.mu.Lock()
	a := h.cur
	h.cur = nil
	h.timer.Reset(u.cfg.Timeout.Std())
	h.mu.Unlock()
	a.reply <- resp
	return h, nil
}

// askClient hands a request the server made on cs to the client of the
// call that holds cs, and waits for the answer.
func (u *upstream) askClient(ctx context.Context, cs *mcp.ClientSession, kind string, params mcp.InputRequest) (mcp.InputResponse, error) {
	v, _ := u.calls.Load(cs)
	h, _ := v.(*held)
	if h == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidRequest, Message: "no call is in progress that this request could belong to"}
	}
	a := &ask{kind: kind, params: params, reply: make(chan mcp.InputResponse, 1)}
	select {
	case h.asks <- a:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-h.ctx.Done():
		return nil, context.Cause(h.ctx)
	}
	select {
	case r := <-a.reply:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-h.ctx.Done():
		return nil, context.Cause(h.ctx)
	}
}

func (u *upstream) elicit(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	r, err := u.askClient(ctx, req.Session, config.MCPElicitation, req.Params)
	if err != nil {
		return nil, err
	}
	res, ok := r.(*mcp.ElicitResult)
	if !ok {
		return nil, errors.New("the client did not answer with an elicitation result")
	}
	return res, nil
}

func (u *upstream) sample(ctx context.Context, req *mcp.CreateMessageWithToolsRequest) (*mcp.CreateMessageWithToolsResult, error) {
	r, err := u.askClient(ctx, req.Session, config.MCPSampling, req.Params)
	if err != nil {
		return nil, err
	}
	res, ok := r.(*mcp.CreateMessageWithToolsResult)
	if !ok {
		return nil, errors.New("the client did not answer with a sampling result")
	}
	return res, nil
}

// clientOptions are the options of the gateway's client for u. Results
// that ask for input come back to the caller, which passes them on.
func (u *upstream) clientOptions() *mcp.ClientOptions {
	o := &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		Capabilities:   &mcp.ClientCapabilities{},
	}
	if u.cfg.Relays(config.MCPElicitation) {
		o.ElicitationHandler = u.elicit
		o.Capabilities.Elicitation = &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{}}
	}
	if u.cfg.Relays(config.MCPSampling) {
		o.CreateMessageWithToolsHandler = u.sample
	}
	return o
}

// exclusive runs f on a session that no other call uses, so that a
// request the server makes on it belongs to h.
func (u *upstream) exclusive(ctx context.Context, subject string, h *held, f func(context.Context, *mcp.ClientSession) error) error {
	for attempt := 0; ; attempt++ {
		cs, err := u.lease(ctx, subject)
		if err != nil {
			return err
		}
		u.calls.Store(cs, h)
		err = f(ctx, cs)
		u.calls.Delete(cs)
		// A session whose call was cut short may still carry its messages.
		keep := err == nil || (ctx.Err() == nil && !brokenSession(err))
		u.release(subject, cs, keep)
		if brokenSession(err) && attempt == 0 {
			continue
		}
		return err
	}
}

// lease takes an idle exclusive session of the shared credential, or
// opens a session.
func (u *upstream) lease(ctx context.Context, subject string) (*mcp.ClientSession, error) {
	if subject == "" {
		u.mu.Lock()
		if n := len(u.idle); n > 0 {
			cs := u.idle[n-1]
			u.idle = u.idle[:n-1]
			u.mu.Unlock()
			return cs, nil
		}
		u.mu.Unlock()
	}
	return u.open(ctx, subject)
}

func (u *upstream) release(subject string, cs *mcp.ClientSession, keep bool) {
	if keep && subject == "" {
		u.mu.Lock()
		if !u.closed && len(u.idle) < idleExclusive {
			u.idle = append(u.idle, cs)
			u.mu.Unlock()
			return
		}
		u.mu.Unlock()
	}
	go cs.Close()
}
