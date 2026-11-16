package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rnikadi/genaiopengateway/internal/a2apb"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/id"
)

// The gRPC binding of A2A 1.0 (docs/a2a.md). A gRPC request has no
// path to name the agent in, so the tenant field of each request does,
// as the gateway's card says. Messages are handled through their
// descriptors and checked as their JSON form, which is the form the
// other bindings of A2A 1.0 carry, so every binding gets the same
// checks.

// a2aGRPCPath is where the A2A service's methods are served.
const a2aGRPCPath = "/" + a2apb.ServiceName + "/"

// grpcCodes are the gRPC codes of the names in restErrors.
var grpcCodes = map[string]codes.Code{
	"INVALID_ARGUMENT":    codes.InvalidArgument,
	"NOT_FOUND":           codes.NotFound,
	"FAILED_PRECONDITION": codes.FailedPrecondition,
	"INTERNAL":            codes.Internal,
}

// grpcError is a JSON-RPC or A2A error as the gRPC binding carries it:
// a status, with the A2A reason in an ErrorInfo.
func grpcError(code int, msg string) error {
	e, ok := restErrors[code]
	if !ok {
		e = restErrors[rpcInternalError]
	}
	st := status.New(grpcCodes[e.name], msg)
	if e.reason != "" {
		if with, err := st.WithDetails(&errdetails.ErrorInfo{Reason: e.reason, Domain: "a2a-protocol.org"}); err == nil {
			st = with
		}
	}
	return st.Err()
}

// grpcAddress is the host and port of the gateway's public URL, which
// is how a card names a gRPC interface.
func grpcAddress(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base
	}
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "http" {
		return net.JoinHostPort(u.Hostname(), "80")
	}
	return net.JoinHostPort(u.Hostname(), "443")
}

// newA2AGRPC returns the gRPC server that relays the A2A service. It
// has no listener: serveA2AGRPCRequest hands it requests.
func (s *Server) newA2AGRPC() *grpc.Server {
	return grpc.NewServer(grpc.UnknownServiceHandler(s.relayA2AGRPC), grpc.MaxRecvMsgSize(maxA2AResponse))
}

// serveA2AGRPCRequest serves a gRPC request for the A2A service, which
// needs HTTP/2.
func (s *Server) serveA2AGRPCRequest(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor != 2 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		writeError(w, "", http.StatusUnsupportedMediaType, codeInvalidRequest, "this path is the gRPC binding of A2A, which needs HTTP/2 and application/grpc")
		return
	}
	s.a2aGRPC.ServeHTTP(w, r)
}

// headersOnly takes the rate limit headers of a gRPC request, which
// has nowhere to show them.
type headersOnly struct{ h http.Header }

func (w headersOnly) Header() http.Header       { return w.h }
func (headersOnly) Write(b []byte) (int, error) { return len(b), nil }
func (headersOnly) WriteHeader(int)             {}

// relayA2AGRPC relays one call of the A2A service to the agent the
// request's tenant names, with the checks of serveA2A.
func (s *Server) relayA2AGRPC(_ any, stream grpc.ServerStream) error {
	start := time.Now()
	full, _ := grpc.MethodFromServerStream(stream)
	svc, err := a2apb.Service()
	if err != nil {
		s.log.Error("the A2A service's descriptors could not be read", "err", err)
		return status.Error(codes.Internal, "the gateway cannot serve gRPC")
	}
	method := protoreflect.Name(strings.TrimPrefix(full, a2aGRPCPath))
	md := svc.Methods().ByName(method)
	if md == nil || !strings.HasPrefix(full, a2aGRPCPath) {
		return status.Error(codes.Unimplemented, "the gateway serves only "+a2apb.ServiceName+" over gRPC")
	}
	ctx := stream.Context()
	in, _ := metadata.FromIncomingContext(ctx)
	// The caller, as the HTTP handlers know one.
	hr := (&http.Request{Header: http.Header{}}).WithContext(ctx)
	for _, h := range []string{"Authorization", "X-Api-Key", "X-Request-Id"} {
		if v := in.Get(strings.ToLower(h)); len(v) > 0 {
			hr.Header.Set(h, v[0])
		}
	}
	p, ok := s.authenticate(hr)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing or invalid API key")
	}
	rt := s.rt.Load()
	req := dynamicpb.NewMessage(md.Input())
	if err := stream.RecvMsg(req); err != nil {
		return status.Error(codes.InvalidArgument, "the request could not be read")
	}
	if int64(proto.Size(req)) > rt.cfg.Limits.MaxRequestBytes {
		return status.Error(codes.ResourceExhausted, "the request is too large")
	}
	tenant := md.Input().Fields().ByName("tenant")
	if tenant == nil {
		return status.Error(codes.Unimplemented, "method "+string(method)+" is not available through the gateway")
	}
	name := req.Get(tenant).String()
	agent, ok := rt.cfg.A2A.Agents[name]
	if !ok {
		if name == "" {
			return status.Error(codes.InvalidArgument, "name the agent in the request's tenant field, as the agent's card at the gateway says")
		}
		return status.Error(codes.NotFound, "agent "+name+" is not configured")
	}
	c := &a2aCall{s: s, rt: rt, name: name, agent: agent, p: p, rid: hr.Header.Get("X-Request-Id"), method: "other", status: "ok", seen: map[string]bool{}, grpc: true}
	if c.rid == "" {
		c.rid = id.New("req")
	}
	defer func() {
		d := time.Since(start)
		s.metrics.ObserveA2ARequest(c.name, c.method, c.status, d)
		s.log.Info("a2a request", "request_id", c.rid, "agent", c.name, "method", c.method, "status", c.status, "binding", "grpc",
			"org_id", p.OrgID, "team_id", p.TeamID, "key_id", p.KeyID, "duration_ms", d.Milliseconds())
	}()
	if !p.AllowsAgent(name) {
		c.status = "denied"
		return status.Error(codes.PermissionDenied, "you may not use agent "+name)
	}
	ctx = s.callerContext(ctx, p)
	hr = hr.WithContext(ctx)
	if code, _, msg, ok := s.admit(headersOnly{http.Header{}}, hr, p, c.rid); !ok {
		c.status = "rate_limited"
		if code == http.StatusServiceUnavailable {
			return status.Error(codes.Unavailable, msg)
		}
		return status.Error(codes.ResourceExhausted, msg)
	}
	fail := func(code int, st, msg string) error {
		c.rpcError(nil, code, st, msg)
		return c.grpcErr
	}
	if agent.GRPCURL == "" {
		return fail(a2aUnsupported, "invalid", "this agent is not offered over gRPC")
	}
	op, known := a2aMethods[string(method)]
	switch {
	case method == "ListTasks":
		return fail(a2aUnsupported, "invalid", listTasksRefused)
	case !known:
		c.status = "invalid"
		return status.Error(codes.Unimplemented, "method "+string(method)+" is not available through the gateway")
	}
	c.method = op.label
	if op.streams && rt.guards.Applies(config.StageOutput, agent.Guardrails) {
		return fail(a2aUnsupported, "invalid", "streaming is not available for this agent, since its output guardrails check whole results")
	}
	params, err := messageJSON(req)
	if err != nil {
		return fail(rpcInvalidParams, "invalid", "the request could not be read")
	}
	taskID, _ := params["id"].(string)
	if op.push != "" {
		if t, _ := params["taskId"].(string); t != "" {
			c.pushConfig, taskID = taskID, t
		}
	}
	if _, ok := c.check(nil, ctx, op, taskID, params); !ok {
		return c.grpcErr
	}
	// The agent knows itself by its own tenant, if it has one.
	delete(params, "tenant")
	if agent.GRPCTenant != "" {
		params["tenant"] = agent.GRPCTenant
	}
	out := dynamicpb.NewMessage(md.Input())
	if err := jsonMessage(params, out); err != nil {
		return fail(rpcInvalidParams, "invalid", "the request could not be read")
	}
	conn, err := s.a2aGRPCConn(agent.GRPCURL)
	if err != nil {
		return fail(rpcInternalError, "error", "the agent's gRPC address is not valid")
	}
	// The agent's own credentials, and the protocol version and
	// extensions the caller asked for.
	omd := metadata.MD{}
	for k, v := range agent.Headers {
		omd.Set(strings.ToLower(k), v.Value())
	}
	for _, h := range []string{"a2a-version", "a2a-extensions"} {
		if v := in.Get(h); len(v) > 0 {
			omd.Set(h, v...)
		}
	}
	uctx, cancel := context.WithCancel(metadata.NewOutgoingContext(ctx, omd))
	defer cancel()
	// A stream may run for as long as the agent works; the timeout only
	// bounds the wait for it to start.
	timer := time.AfterFunc(agent.Timeout.Std(), cancel)
	defer timer.Stop()

	if !op.streams {
		resp := dynamicpb.NewMessage(md.Output())
		if err := conn.Invoke(uctx, full, out, resp); err != nil {
			return c.agentError(err)
		}
		result, err := messageJSON(resp)
		if err != nil {
			return fail(a2aInvalidResponse, "upstream_error", "the agent's answer could not be read")
		}
		changed, blocked := c.settle(ctx, result)
		if blocked != "" {
			return fail(a2aInvalidResponse, "guardrail_blocked", blocked)
		}
		if changed {
			resp = dynamicpb.NewMessage(md.Output())
			if err := jsonMessage(result, resp); err != nil {
				return fail(a2aInvalidResponse, "upstream_error", "the agent's answer could not be read")
			}
		}
		return stream.SendMsg(resp)
	}

	up, err := conn.NewStream(uctx, &grpc.StreamDesc{ServerStreams: true}, full)
	if err == nil {
		err = up.SendMsg(out)
	}
	if err == nil {
		err = up.CloseSend()
	}
	if err != nil {
		return c.agentError(err)
	}
	for {
		ev := dynamicpb.NewMessage(md.Output())
		err := up.RecvMsg(ev)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return c.agentError(err)
		}
		timer.Stop()
		if m, err := messageJSON(ev); err == nil {
			c.record(ctx, m)
		}
		if err := stream.SendMsg(ev); err != nil {
			return err
		}
	}
}

// agentError is an agent's error as the caller gets it. One about the
// gateway's own credentials is not the caller's.
func (c *a2aCall) agentError(err error) error {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.Unauthenticated, codes.PermissionDenied:
		c.status = "upstream_error"
		c.s.log.Warn("a2a agent refused the gateway", "request_id", c.rid, "agent", c.name, "code", st.Code().String())
		return status.Error(codes.Internal, "the agent refused the gateway's credentials")
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown:
		c.status = "upstream_error"
		c.s.log.Warn("a2a agent failed", "request_id", c.rid, "agent", c.name, "err", err)
		if st.Code() == codes.Unknown {
			return status.Error(codes.Internal, "the agent could not be reached")
		}
		return status.Error(st.Code(), "the agent could not be reached")
	}
	c.status = "agent_error"
	return st.Err()
}

// messageJSON returns a message in the JSON form of A2A 1.0.
func messageJSON(m proto.Message) (map[string]any, error) {
	raw, err := protojson.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// jsonMessage reads the JSON form of a message into it.
func jsonMessage(v map[string]any, m proto.Message) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(raw, m)
}

// a2aGRPCConn returns the connection to an agent's gRPC address, made
// on first use and shared by its calls.
func (s *Server) a2aGRPCConn(raw string) (*grpc.ClientConn, error) {
	if v, ok := s.a2aConns.Load(raw); ok {
		return v.(*grpc.ClientConn), nil
	}
	target, useTLS, err := config.GRPCTarget(raw)
	if err != nil {
		return nil, err
	}
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(nil)
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxA2AResponse)))
	if err != nil {
		return nil, err
	}
	if v, loaded := s.a2aConns.LoadOrStore(raw, conn); loaded {
		_ = conn.Close()
		return v.(*grpc.ClientConn), nil
	}
	return conn, nil
}

// closeA2AGRPC closes the connections to agents.
func (s *Server) closeA2AGRPC() {
	s.a2aConns.Range(func(k, v any) bool {
		_ = v.(*grpc.ClientConn).Close()
		s.a2aConns.Delete(k)
		return true
	})
}
