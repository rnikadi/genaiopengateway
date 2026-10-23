// Package server serves the gateway's HTTP APIs.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/id"
	"github.com/rnikadi/genaiopengateway/internal/limits"
	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/router"
	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// runtime is everything derived from one config version. It is replaced
// as a whole on reload so a request always sees a consistent view.
type runtime struct {
	cfg    *config.Config
	router *router.Router
}

// Server holds the live runtime and serves requests against it.
type Server struct {
	rt     atomic.Pointer[runtime]
	keys   atomic.Pointer[auth.Directory]
	ready  atomic.Bool
	log    *slog.Logger
	client *http.Client

	// store and usage are nil when no database is configured.
	store *store.Store
	usage *usage.Writer

	limiter *limits.Limiter
	valkey  *limits.Valkey // nil unless the valkey driver is used
	stop    context.CancelFunc

	metrics     *telemetry.Metrics
	mcp         *mcpgw.Gateway
	lastRefresh atomic.Int64 // unix nanoseconds of the last store load

	// mu serializes rebuilding the key directory from its two sources.
	mu        sync.Mutex
	storeKeys map[string]*auth.Principal
}

// New builds a server. st may be nil.
func New(cfg *config.Config, log *slog.Logger, st *store.Store) (*Server, error) {
	s := &Server{log: log, client: newUpstreamClient(), store: st}
	ctx, stop := context.WithCancel(context.Background())
	s.stop = stop
	var backend limits.Backend
	if lc := cfg.Limiter; lc.Driver == "valkey" {
		s.valkey = limits.NewValkey(lc.Address, lc.Password.Value())
		backend = s.valkey
	} else {
		backend = limits.NewMemory(ctx)
	}
	s.limiter = limits.New(backend, cfg.Limiter.FailOpenRateLimits, cfg.Limiter.FailOpenBudgets)
	if st != nil {
		s.usage = usage.NewWriter(st.InsertUsage, log)
	} else {
		s.ready.Store(true)
	}
	s.metrics = telemetry.NewMetrics(s.usageWriteErrors, s.snapshotAge)
	s.mcp = mcpgw.New(ctx, cfg.MCP, mcpgw.Options{Log: log, Transport: s.client.Transport, Limiter: s.limiter, Metrics: s.metrics})
	if err := s.Reload(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload swaps in a new config. In-flight requests finish on the old one.
func (s *Server) Reload(cfg *config.Config) error {
	r, err := router.New(cfg, s.client)
	if err != nil {
		return err
	}
	s.rt.Store(&runtime{cfg: cfg, router: r})
	s.mcp.Reload(cfg.MCP)
	s.rebuildKeys()
	return nil
}

// rebuildKeys merges static keys from config with keys from the store.
func (s *Server) rebuildKeys() {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := auth.StaticPrincipals(s.rt.Load().cfg.Keys)
	maps.Copy(all, s.storeKeys)
	if len(all) == 0 && s.store == nil {
		s.log.Warn("no API keys configured; every model request will be rejected")
	}
	s.keys.Store(auth.NewDirectory(all))
}

// Start runs background work until ctx is done: refreshing keys from the
// store and pruning old usage.
func (s *Server) Start(ctx context.Context) {
	if s.store == nil {
		return
	}
	go s.seedBudgets(ctx)
	go s.refreshLoop(ctx)
	if retention := s.rt.Load().cfg.Store.UsageRetention.Std(); retention > 0 {
		go s.pruneLoop(ctx, retention)
	}
}

// Close flushes queued usage records and releases background resources.
func (s *Server) Close(ctx context.Context) {
	if s.usage != nil {
		s.usage.Close(ctx)
	}
	s.stop()
	s.mcp.Close()
	if s.valkey != nil {
		_ = s.valkey.Close()
	}
}

// seedBudgets raises budget counters to the spend recorded in the store,
// so a restart does not reset budgets.
func (s *Server) seedBudgets(ctx context.Context) {
	for _, period := range []string{policy.PeriodDaily, policy.PeriodMonthly, policy.PeriodTotal} {
		spend, err := s.store.SpendSince(ctx, limits.PeriodStart(period, time.Now()))
		if err == nil {
			err = s.limiter.Seed(ctx, period, spend)
		}
		if err != nil {
			s.log.Error("seeding budget counters failed", "period", period, "err", err)
		}
	}
}

func (s *Server) refreshLoop(ctx context.Context) {
	for {
		if err := s.refresh(ctx); err != nil {
			s.log.Error("loading keys from the store failed; serving the previous snapshot", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.rt.Load().cfg.Store.RefreshInterval.Std()):
		}
	}
}

// refresh loads tenants and keys from the store into the directory.
func (s *Server) refresh(ctx context.Context) error {
	snap, err := s.store.LoadSnapshot(ctx)
	if err != nil {
		return err
	}
	keys := make(map[string]*auth.Principal, len(snap.Keys))
	for _, k := range snap.Keys {
		org, team := snap.Orgs[k.OrgID], snap.Teams[k.TeamID]
		if org == nil || team == nil {
			continue
		}
		keys[k.SecretHash] = &auth.Principal{
			OrgID: k.OrgID, TeamID: k.TeamID, KeyID: k.ID, KeyName: k.Name, KeyPrefix: k.Prefix,
			Policies:  [3]policy.Policy{org.Policy, team.Policy, k.Policy},
			ExpiresAt: k.ExpiresAt,
		}
	}
	s.mu.Lock()
	s.storeKeys = keys
	s.mu.Unlock()
	s.rebuildKeys()
	s.lastRefresh.Store(time.Now().UnixNano())
	s.ready.Store(true)
	return nil
}

func (s *Server) usageWriteErrors() float64 {
	if s.usage == nil {
		return 0
	}
	return float64(s.usage.Dropped())
}

// snapshotAge is zero without a store or before the first load, which
// /readyz already reports.
func (s *Server) snapshotAge() float64 {
	if s.store == nil {
		return 0
	}
	last := s.lastRefresh.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last)).Seconds()
}

func (s *Server) pruneLoop(ctx context.Context, retention time.Duration) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		n, err := s.store.PruneUsage(ctx, time.Now().Add(-retention))
		if err != nil {
			s.log.Error("pruning usage failed", "err", err)
		} else if n > 0 {
			s.log.Info("pruned usage records", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// MetricsHandler serves Prometheus metrics.
func (s *Server) MetricsHandler() http.Handler { return s.metrics.Handler() }

// Handler returns the HTTP handler with all routes registered. /metrics
// is included unless metrics have a listener of their own.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.rt.Load().cfg.Metrics.Listen == "" {
		mux.Handle("GET /metrics", s.MetricsHandler())
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("POST /v1/chat/completions", s.model(provider.ChatCompletions))
	mux.HandleFunc("POST /v1/embeddings", s.model(provider.Embeddings))
	mux.HandleFunc("POST /v1/responses", s.model(provider.Responses))
	mux.HandleFunc("POST /v1/messages", s.model(provider.Messages))
	mux.HandleFunc("/mcp/{name}", s.serveMCP)
	s.adminRoutes(mux)
	return s.recoverer(withRequestID(mux))
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		http.Error(w, "loading keys", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// authenticate resolves the presented key, rejecting expired keys.
func (s *Server) authenticate(r *http.Request) (*auth.Principal, bool) {
	p, ok := s.keys.Load().Lookup(presentedKey(r))
	if !ok || p.Expired(time.Now()) {
		return nil, false
	}
	return p, true
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authenticate(r)
	if !ok {
		writeError(w, provider.ChatCompletions, http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
		return
	}
	data := []map[string]any{}
	for _, name := range s.rt.Load().router.Models() {
		if p.AllowsModel(name) {
			data = append(data, map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "genaiopengateway"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// presentedKey returns the gateway key from Authorization: Bearer or,
// for the Anthropic SDK, x-api-key.
func presentedKey(r *http.Request) string {
	if v := r.Header.Get("Authorization"); len(v) > 7 && (v[:7] == "Bearer " || v[:7] == "bearer ") {
		return v[7:]
	}
	return r.Header.Get("X-Api-Key")
}

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// withRequestID keeps a safe client-supplied x-request-id or makes one,
// and echoes it on the response.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if !safeRequestID.MatchString(rid) {
			rid = id.New("req")
			r.Header.Set("X-Request-Id", rid)
		}
		w.Header().Set("X-Request-Id", rid)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic serving request", "request_id", r.Header.Get("X-Request-Id"), "panic", fmt.Sprint(v))
				writeError(w, provider.ChatCompletions, http.StatusInternalServerError, "internal_error", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// newUpstreamClient returns the HTTP client shared by all adapters.
// Per-request timeouts come from each provider's config.
func newUpstreamClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}
