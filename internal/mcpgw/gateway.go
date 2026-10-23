// Package mcpgw is the MCP gateway: it serves virtual MCP servers built
// from the tools, prompts and resources of upstream MCP servers. See
// docs/mcp.md.
package mcpgw

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/limits"
	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
)

// Version is reported to clients and upstreams as the implementation
// version. It is set by main.
var Version = "dev"

// Options are the gateway's dependencies.
type Options struct {
	Log       *slog.Logger
	Transport http.RoundTripper
	Limiter   *limits.Limiter
	Metrics   *telemetry.Metrics
	// Pins keeps tool pins; nil keeps them in memory.
	Pins Pins
	// Record receives the audit record of every call; it may be nil.
	Record func(store.MCPCall)
	// Audit appends to the admin audit log; it may be nil.
	Audit func(context.Context, *store.AuditEntry) error
	// OnPending, which may be nil, is told when a tool is hidden until
	// approval: new in strict mode, or changed since it was approved.
	OnPending func(server, tool, hash string, changed bool)
	// LogBody, which may be nil, receives the params and result of each
	// call, for body logging. It decides whether to log.
	LogBody func(caller *Caller, virtualServer, method string, params any, res mcp.Result, err error)
}

// Gateway serves every virtual server in the config.
type Gateway struct {
	opts Options
	ctx  context.Context
	pins Pins

	// mu guards cfg, upstreams and loops, and serializes rebuilds.
	mu        sync.Mutex
	cfg       config.MCP
	upstreams map[string]*upstream
	loops     map[string]context.CancelFunc

	servers atomic.Pointer[map[string]*mcp.Server]
	handler http.Handler
}

// New starts the gateway. Upstreams are listed in the background until
// ctx is done.
func New(ctx context.Context, cfg config.MCP, opts Options) *Gateway {
	g := &Gateway{opts: opts, ctx: ctx, pins: opts.Pins, upstreams: map[string]*upstream{}, loops: map[string]context.CancelFunc{}}
	if g.pins == nil {
		g.pins = newMemPins()
	}
	g.servers.Store(&map[string]*mcp.Server{})
	sdk := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return (*g.servers.Load())[r.PathValue("name")]
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// Every request carries a bearer token, which a browser tricked
		// by DNS rebinding cannot add, and the localhost check would
		// reject requests from a reverse proxy on the same host.
		DisableLocalhostProtection: true,
		Logger:                     opts.Log,
	})
	// The SDK passes request context to handlers only through TokenInfo,
	// so the caller the server authenticated travels there.
	g.handler = sdkauth.RequireBearerToken(func(_ context.Context, _ string, r *http.Request) (*sdkauth.TokenInfo, error) {
		c, ok := r.Context().Value(callerKey{}).(*Caller)
		if !ok {
			return nil, sdkauth.ErrInvalidToken
		}
		return &sdkauth.TokenInfo{Extra: map[string]any{callerExtra: c}}, nil
	}, &sdkauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(sdk)
	g.Reload(cfg)
	return g
}

// Caller is who is calling, as authenticated by the server.
type Caller struct {
	Principal *auth.Principal
	// Subject is the OAuth subject, when the caller used a token.
	Subject   string
	RequestID string
}

type callerKey struct{}

const callerExtra = "gog.caller"

// WithCaller attaches the authenticated caller to a request context.
func WithCaller(ctx context.Context, c *Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// Has reports whether a virtual server is configured.
func (g *Gateway) Has(name string) bool {
	_, ok := (*g.servers.Load())[name]
	return ok
}

// ServeHTTP serves the virtual server named by the {name} path value.
// The request must carry a caller from WithCaller.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.handler.ServeHTTP(w, r) }

// Reload applies a new config. Upstreams whose settings are unchanged
// keep their sessions and catalogs.
func (g *Gateway) Reload(cfg config.MCP) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for name, u := range g.upstreams {
		if next, ok := cfg.Servers[name]; ok && reflect.DeepEqual(next, u.cfg) {
			continue
		}
		g.loops[name]()
		delete(g.loops, name)
		delete(g.upstreams, name)
		u.close()
		g.opts.Metrics.ForgetMCPUpstream(name)
	}
	g.cfg = cfg
	for name, sc := range cfg.Servers {
		if _, ok := g.upstreams[name]; ok {
			continue
		}
		u := newUpstream(name, sc, g.opts.Transport)
		g.upstreams[name] = u
		ctx, cancel := context.WithCancel(g.ctx)
		g.loops[name] = cancel
		go g.refreshLoop(ctx, u, cfg.RefreshInterval.Std())
	}
	g.rebuildLocked()
}

// Refresh lists every upstream now and rebuilds the virtual servers.
func (g *Gateway) Refresh(ctx context.Context) {
	g.mu.Lock()
	ups := maps.Clone(g.upstreams)
	g.mu.Unlock()
	var wg sync.WaitGroup
	for _, u := range ups {
		wg.Go(func() { g.refresh(ctx, u) })
	}
	wg.Wait()
	g.rebuild()
}

// Close ends every upstream session.
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for name, u := range g.upstreams {
		g.loops[name]()
		u.close()
	}
}

// refreshLoop lists an upstream every interval. An upstream that fails
// is retried sooner, backing off up to the interval.
func (g *Gateway) refreshLoop(ctx context.Context, u *upstream, interval time.Duration) {
	backoff := time.Second
	for {
		wait := interval
		if changed, ok := g.refresh(ctx, u); !ok {
			wait = min(backoff, interval)
			backoff *= 2
		} else {
			backoff = time.Second
			if changed {
				g.rebuild()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// refresh lists one upstream and reports whether its catalog changed
// and whether the listing succeeded. A failed upstream keeps its last
// catalog.
func (g *Gateway) refresh(ctx context.Context, u *upstream) (changed, ok bool) {
	c, err := u.list(ctx)
	if ctx.Err() != nil {
		return false, true
	}
	wasUp := u.up.Swap(err == nil)
	g.opts.Metrics.SetMCPUpstreamUp(u.name, err == nil)
	if err != nil {
		if wasUp || u.catalog.Load() == nil {
			g.opts.Log.Warn("listing MCP server failed; keeping its last catalog", "server", u.name, "err", err)
		}
		return false, false
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := g.applyPins(pctx, u, c); err != nil {
		// Without pins the catalog cannot be trusted; keep the last one.
		g.opts.Log.Error("checking MCP tool pins failed; keeping the last catalog", "server", u.name, "err", err)
		return false, true
	}
	c.seal()
	g.opts.Metrics.SetMCPPendingTools(u.name, len(c.hidden))
	prev := u.catalog.Swap(c)
	if !wasUp {
		g.opts.Log.Info("MCP server listed", "server", u.name, "protocol_version", c.protocolVersion,
			"tools", len(c.Tools), "prompts", len(c.Prompts), "resources", len(c.Resources))
	}
	return prev == nil || prev.hash != c.hash, true
}

func (g *Gateway) rebuild() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rebuildLocked()
}

// rebuildLocked builds every virtual server from the current catalogs
// and swaps them in at once.
func (g *Gateway) rebuildLocked() {
	next := make(map[string]*mcp.Server, len(g.cfg.VirtualServers))
	for name, vs := range g.cfg.VirtualServers {
		next[name] = g.buildServer(name, vs)
	}
	g.servers.Store(&next)
}
