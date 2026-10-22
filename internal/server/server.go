// Package server serves the gateway's HTTP APIs.
package server

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/id"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/router"
)

// runtime is everything derived from one config version. It is replaced
// as a whole on reload so a request always sees a consistent view.
type runtime struct {
	cfg    *config.Config
	router *router.Router
	keys   *auth.Directory
}

// Server holds the live runtime and serves requests against it.
type Server struct {
	rt     atomic.Pointer[runtime]
	log    *slog.Logger
	client *http.Client
}

// New builds a server from config.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	s := &Server{log: log, client: newUpstreamClient()}
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
	keys := auth.NewDirectory(auth.StaticPrincipals(cfg.Keys))
	if keys.Len() == 0 {
		s.log.Warn("no API keys configured; every model request will be rejected")
	}
	s.rt.Store(&runtime{cfg: cfg, router: r, keys: keys})
	return nil
}

// Handler returns the HTTP handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("POST /v1/chat/completions", s.model(provider.ChatCompletions))
	mux.HandleFunc("POST /v1/embeddings", s.model(provider.Embeddings))
	mux.HandleFunc("POST /v1/responses", s.model(provider.Responses))
	mux.HandleFunc("POST /v1/messages", s.model(provider.Messages))
	return s.recoverer(withRequestID(mux))
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	if s.rt.Load() == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	rt := s.rt.Load()
	p, ok := rt.keys.Lookup(presentedKey(r))
	if !ok {
		writeError(w, provider.ChatCompletions, http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
		return
	}
	data := []map[string]any{}
	for _, name := range rt.router.Models() {
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
