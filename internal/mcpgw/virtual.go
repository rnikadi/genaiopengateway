package mcpgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/limits"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
)

// toolName is the alphabet and length the MCP spec allows for names.
var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// buildServer makes the MCP server for one virtual server from the
// catalogs of its upstreams. Items that are not allowed, or that would
// clash with an item already added, are left out.
func (g *Gateway) buildServer(name string, vs config.VirtualServer) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "genaiopengateway/" + name, Version: Version}, &mcp.ServerOptions{
		Instructions: vs.Description,
		// Advertised even when empty, so a client keeps listing as
		// upstreams come and go.
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{}, Prompts: &mcp.PromptCapabilities{}, Resources: &mcp.ResourceCapabilities{},
		},
		Logger: g.opts.Log,
	})
	s.AddReceivingMiddleware(g.middleware(name))
	log := g.opts.Log.With("virtual_server", name)
	tools, prompts, uris := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, ref := range vs.Servers {
		u := g.upstreams[ref.Server]
		c := u.catalog.Load()
		if c == nil {
			continue
		}
		prefix := ref.NamePrefix()
		allowed := map[string]bool{}
		for _, t := range ref.Tools {
			allowed[t] = true
		}
		for _, t := range c.Tools {
			if ref.Tools != nil && !allowed[t.Name] {
				continue
			}
			exposed := prefix + t.Name
			if !toolName.MatchString(exposed) || tools[exposed] {
				log.Warn("skipping MCP tool with an invalid or clashing name", "server", u.name, "tool", t.Name, "name", exposed)
				continue
			}
			tt := *t
			tt.Name = exposed
			if err := addTool(s, &tt, g.forwardTool(u, t.Name)); err != nil {
				log.Warn("skipping invalid MCP tool", "server", u.name, "tool", t.Name, "err", err)
				continue
			}
			tools[exposed] = true
		}
		if ref.ExposesPrompts() {
			for _, p := range c.Prompts {
				exposed := prefix + p.Name
				if !toolName.MatchString(exposed) || prompts[exposed] {
					log.Warn("skipping MCP prompt with an invalid or clashing name", "server", u.name, "prompt", p.Name, "name", exposed)
					continue
				}
				pp := *p
				pp.Name = exposed
				s.AddPrompt(&pp, g.forwardPrompt(u, p.Name))
				prompts[exposed] = true
			}
		}
		if ref.ExposesResources() {
			read := g.forwardRead(u)
			for _, r := range c.Resources {
				if uris[r.URI] || !absoluteURI(r.URI) {
					log.Warn("skipping MCP resource with an invalid or clashing URI", "server", u.name, "uri", r.URI)
					continue
				}
				s.AddResource(r, read)
				uris[r.URI] = true
			}
			for _, t := range c.Templates {
				if uris[t.URITemplate] {
					log.Warn("skipping MCP resource template with a clashing URI", "server", u.name, "uri_template", t.URITemplate)
					continue
				}
				if err := addTemplate(s, t, read); err != nil {
					log.Warn("skipping invalid MCP resource template", "server", u.name, "uri_template", t.URITemplate, "err", err)
					continue
				}
				uris[t.URITemplate] = true
			}
		}
	}
	return s
}

// addTool adds a tool, turning the SDK's panic on an invalid definition
// into an error: definitions come from upstreams the gateway does not
// control.
func addTool(s *mcp.Server, t *mcp.Tool, h mcp.ToolHandler) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("%v", v)
		}
	}()
	s.AddTool(t, h)
	return nil
}

func addTemplate(s *mcp.Server, t *mcp.ResourceTemplate, h mcp.ResourceHandler) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("%v", v)
		}
	}()
	s.AddResourceTemplate(t, h)
	return nil
}

func absoluteURI(uri string) bool {
	scheme, _, ok := strings.Cut(uri, ":")
	return ok && scheme != ""
}

// call is what the gateway learns about one request while serving it.
type call struct {
	Server    string
	Name      string
	ErrorCode int64
}

type callKey struct{}

func callFrom(ctx context.Context) *call {
	c, _ := ctx.Value(callKey{}).(*call)
	if c == nil {
		return &call{}
	}
	return c
}

func (g *Gateway) forwardTool(u *upstream, name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c := callFrom(ctx)
		c.Server, c.Name = u.name, name
		var res *mcp.CallToolResult
		err := g.forward(ctx, u, "tools/call", name, func(ctx context.Context, cs *mcp.ClientSession, meta mcp.Meta) (err error) {
			res, err = cs.CallTool(ctx, &mcp.CallToolParams{Meta: meta, Name: name, Arguments: req.Params.Arguments})
			return err
		})
		status := telemetry.MCPOk
		switch {
		case err != nil:
			status = telemetry.MCPError
		case res.IsError:
			status = telemetry.MCPToolError
		}
		g.opts.Metrics.ObserveMCPToolCall(u.name, name, status)
		return res, err
	}
}

func (g *Gateway) forwardPrompt(u *upstream, name string) mcp.PromptHandler {
	return func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		c := callFrom(ctx)
		c.Server, c.Name = u.name, name
		var res *mcp.GetPromptResult
		err := g.forward(ctx, u, "prompts/get", name, func(ctx context.Context, cs *mcp.ClientSession, meta mcp.Meta) (err error) {
			res, err = cs.GetPrompt(ctx, &mcp.GetPromptParams{Meta: meta, Name: name, Arguments: req.Params.Arguments})
			return err
		})
		return res, err
	}
}

func (g *Gateway) forwardRead(u *upstream) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		c := callFrom(ctx)
		c.Server, c.Name = u.name, uri
		var res *mcp.ReadResourceResult
		err := g.forward(ctx, u, "resources/read", uri, func(ctx context.Context, cs *mcp.ClientSession, meta mcp.Meta) (err error) {
			res, err = cs.ReadResource(ctx, &mcp.ReadResourceParams{Meta: meta, URI: uri})
			return err
		})
		return res, err
	}
}

// forward sends one request upstream inside an mcp.upstream span, and
// turns transport failures into JSON-RPC errors. Errors the upstream
// returned over JSON-RPC are passed through.
func (g *Gateway) forward(ctx context.Context, u *upstream, method, name string, f func(context.Context, *mcp.ClientSession, mcp.Meta) error) error {
	ctx, span := telemetry.Tracer().Start(ctx, "mcp.upstream", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("mcp.method.name", method),
			attribute.String("gog.mcp_server", u.name),
		))
	defer span.End()
	if method == "tools/call" {
		span.SetAttributes(attribute.String("gen_ai.tool.name", name))
	}
	meta := mcp.Meta{}
	telemetry.Propagator.Inject(ctx, metaCarrier(meta))
	ctx, cancel := detach(ctx)
	defer cancel()
	err := u.do(ctx, func(ctx context.Context, cs *mcp.ClientSession) error {
		span.SetAttributes(attribute.String("mcp.protocol.version", cs.InitializeResult().ProtocolVersion))
		return f(ctx, cs, meta)
	})
	if err == nil {
		return nil
	}
	span.SetStatus(codes.Error, err.Error())
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) && !brokenSession(err) {
		return rpcErr
	}
	msg := "upstream MCP server " + u.name + " failed"
	if errors.Is(err, context.DeadlineExceeded) {
		msg = "upstream MCP server " + u.name + " timed out"
	}
	g.opts.Log.Warn("MCP upstream request failed", "server", u.name, "method", method, "err", err)
	data, _ := json.Marshal(map[string]string{"server": u.name})
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: msg, Data: data}
}

// detach returns a context that ends with parent and carries its span,
// but none of its other values. The SDK keeps the client's protocol
// version in the context of a request it serves and reads it back when
// sending one, so the upstream call must not inherit it: each side
// negotiates its own version.
func detach(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(trace.ContextWithSpan(context.Background(), trace.SpanFromContext(parent)))
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() { stop(); cancel() }
}

// metaCarrier lets OpenTelemetry write trace context into _meta.
type metaCarrier mcp.Meta

func (m metaCarrier) Get(k string) string {
	v, _ := m[k].(string)
	return v
}
func (m metaCarrier) Set(k, v string) { m[k] = v }
func (m metaCarrier) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// Error codes in the range the spec leaves to implementations.
const codeRateLimited = -32005

// counted methods are admitted through the limiter and logged.
var counted = map[string]bool{"tools/call": true, "prompts/get": true, "resources/read": true}

// knownMethods bounds the method label of metrics.
var knownMethods = map[string]bool{
	"initialize": true, "server/discover": true, "ping": true,
	"tools/list": true, "tools/call": true, "prompts/list": true, "prompts/get": true,
	"resources/list": true, "resources/templates/list": true, "resources/read": true,
}

// middleware wraps every request to a virtual server: it continues the
// caller's trace, applies rate limits to calls, and records metrics and
// the request log line.
func (g *Gateway) middleware(vs string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if strings.HasPrefix(method, "notifications/") {
				return next(ctx, method, req)
			}
			start := time.Now()
			caller := callerOf(req)
			ctx = extractTrace(ctx, req)
			label := method
			if !knownMethods[label] {
				label = "other"
			}
			ctx, span := telemetry.Tracer().Start(ctx, "mcp.request", trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("mcp.method.name", method),
					attribute.String("gog.virtual_server", vs),
				))
			defer span.End()
			c := &call{}
			ctx = context.WithValue(ctx, callKey{}, c)

			status := telemetry.MCPOk
			var res mcp.Result
			var err error
			if counted[method] && caller != nil {
				err = g.admit(ctx, caller)
			}
			if err != nil {
				status = telemetry.MCPRateLimited
			} else {
				res, err = next(ctx, method, req)
			}
			var rpcErr *jsonrpc.Error
			switch {
			case errors.As(err, &rpcErr):
				c.ErrorCode = rpcErr.Code
				if rpcErr.Code != codeRateLimited {
					status = telemetry.MCPError
				}
			case err != nil:
				status = telemetry.MCPError
			}
			if r, ok := res.(*mcp.CallToolResult); ok && r != nil && r.IsError {
				status = telemetry.MCPToolError
			}

			d := time.Since(start)
			g.opts.Metrics.ObserveMCPRequest(vs, label, status, d)
			if caller != nil {
				p := caller.Principal
				span.SetAttributes(attribute.String("gog.org_id", p.OrgID), attribute.String("gog.team_id", p.TeamID))
			}
			if c.Server != "" {
				span.SetAttributes(attribute.String("gog.mcp_server", c.Server))
			}
			if method == "tools/call" && c.Name != "" {
				span.SetAttributes(attribute.String("gen_ai.tool.name", c.Name))
			}
			if status != telemetry.MCPOk && status != telemetry.MCPToolError {
				span.SetStatus(codes.Error, status)
			}
			if counted[method] {
				g.logCall(ctx, caller, vs, method, c, status, d)
			}
			return res, err
		}
	}
}

// admit applies the caller's rate limits to one call.
func (g *Gateway) admit(ctx context.Context, caller *Caller) error {
	d := g.opts.Limiter.Admit(ctx, caller.Principal)
	if d.Allowed {
		return nil
	}
	if d.Code == limits.CodeUnavailable {
		g.opts.Log.Error("limiter unavailable", "request_id", caller.RequestID, "err", d.Message)
		return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "limits could not be checked"}
	}
	if d.Kind != "" {
		g.opts.Metrics.ObserveLimitRejection(d.Scope, d.Kind)
	}
	data, _ := json.Marshal(map[string]any{"retryAfterSeconds": limits.RetryAfterSeconds(d.RetryAfter)})
	return &jsonrpc.Error{Code: codeRateLimited, Message: d.Message, Data: data}
}

func (g *Gateway) logCall(ctx context.Context, caller *Caller, vs, method string, c *call, status string, d time.Duration) {
	attrs := []any{
		"virtual_server", vs, "method", method, "server", c.Server, "name", c.Name,
		"status", status, "latency_ms", d.Milliseconds(),
	}
	if c.ErrorCode != 0 {
		attrs = append(attrs, "error_code", c.ErrorCode)
	}
	if caller != nil {
		p := caller.Principal
		attrs = append(attrs, "request_id", caller.RequestID, "org_id", p.OrgID, "team_id", p.TeamID, "key_id", p.KeyID)
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, "trace_id", sc.TraceID().String())
	}
	g.opts.Log.Info("mcp request", attrs...)
}

func callerOf(req mcp.Request) *Caller {
	extra := req.GetExtra()
	if extra == nil || extra.TokenInfo == nil {
		return nil
	}
	c, _ := extra.TokenInfo.Extra[callerExtra].(*Caller)
	return c
}

// extractTrace continues a trace from the request's _meta, as the MCP
// semantic conventions propagate it, or else from its HTTP headers.
func extractTrace(ctx context.Context, req mcp.Request) context.Context {
	if p := req.GetParams(); p != nil && !reflect.ValueOf(p).IsNil() {
		if m := p.GetMeta(); m != nil {
			if _, ok := m["traceparent"]; ok {
				return telemetry.Propagator.Extract(ctx, metaCarrier(m))
			}
		}
	}
	if extra := req.GetExtra(); extra != nil && extra.Header != nil {
		return telemetry.Propagator.Extract(ctx, propagation.HeaderCarrier(extra.Header))
	}
	return ctx
}
