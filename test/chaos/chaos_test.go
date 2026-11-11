// Package chaos checks the gateway's failover against upstreams that
// fail on purpose: refused connections, error statuses, Retry-After,
// stalls before headers, and recovery (docs/runbook.md#chaos-test).
package chaos

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/server"
)

const key = "chaos-key-0123456789"

// flaky is an OpenAI-compatible upstream whose behavior can change
// while the test runs.
type flaky struct {
	mu     sync.Mutex
	status int
	header http.Header
	stall  time.Duration
	hits   atomic.Int64
	srv    *httptest.Server
}

func newFlaky(t *testing.T, status int) *flaky {
	f := &flaky{status: status}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		status, header, stall := f.status, f.header, f.stall
		f.mu.Unlock()
		if stall > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(stall):
			}
		}
		for k, v := range header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *flaky) set(status int, header http.Header, stall time.Duration) {
	f.mu.Lock()
	f.status, f.header, f.stall = status, header, stall
	f.mu.Unlock()
}

// refused is a URL nothing listens on.
func refused(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

func gateway(t *testing.T, yaml string) (*server.Server, http.Handler) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	s, err := server.New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, s.Handler()
}

func chat(h http.Handler, model string) int {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func circuits(t *testing.T, h http.Handler) string {
	r := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out []string
	for _, l := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(l, "gog_target_circuit_state{") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestFailover(t *testing.T) {
	primary, backup := newFlaky(t, http.StatusOK), newFlaky(t, http.StatusOK)
	_, h := gateway(t, `
providers:
  dead: {base_url: "`+refused(t)+`"}
  primary: {base_url: "`+primary.srv.URL+`"}
  backup: {base_url: "`+backup.srv.URL+`"}
routing: {circuit_breaker: {failures: 3, cooldown: 200ms}}
models:
  chat:
    retries: {attempts: 1, backoff: 1ms, max_backoff: 50ms}
    targets:
      - {provider: dead, model: m}
      - {provider: primary, model: m, timeout: 100ms}
      - {provider: backup, model: m}
keys: [{name: chaos, key: `+key+`}]
`)
	serve := func(n int) {
		t.Helper()
		for i := range n {
			if code := chat(h, "chat"); code != http.StatusOK {
				t.Fatalf("request %d: status %d", i, code)
			}
		}
	}

	t.Run("refused connections open the breaker", func(t *testing.T) {
		serve(3)
		if !strings.Contains(circuits(t, h), `target="dead/m"} 1`) {
			t.Fatalf("dead target not open:\n%s", circuits(t, h))
		}
	})

	t.Run("5xx and 429 fail over to the backup", func(t *testing.T) {
		before := backup.hits.Load()
		primary.set(http.StatusInternalServerError, nil, 0)
		serve(2)
		primary.set(http.StatusTooManyRequests, nil, 0)
		serve(2)
		if backup.hits.Load()-before < 3 {
			t.Fatalf("backup served %d", backup.hits.Load()-before)
		}
	})

	t.Run("a long Retry-After moves on at once", func(t *testing.T) {
		primary.set(http.StatusServiceUnavailable, http.Header{"Retry-After": {"30"}}, 0)
		start := time.Now()
		serve(1)
		if time.Since(start) > 2*time.Second {
			t.Fatal("waited for a Retry-After longer than max_backoff")
		}
	})

	t.Run("a stall before headers times out", func(t *testing.T) {
		time.Sleep(250 * time.Millisecond) // let primary's breaker go half open
		primary.set(http.StatusOK, nil, time.Second)
		start := time.Now()
		serve(1)
		if time.Since(start) > 800*time.Millisecond {
			t.Fatalf("stalled target held the request for %s", time.Since(start))
		}
	})

	t.Run("a recovered target closes its breaker", func(t *testing.T) {
		primary.set(http.StatusOK, nil, 0)
		time.Sleep(250 * time.Millisecond)
		serve(3)
		if !strings.Contains(circuits(t, h), `target="primary/m"} 0`) {
			t.Fatalf("primary not closed:\n%s", circuits(t, h))
		}
	})

	t.Run("everything down still answers with an error, quickly", func(t *testing.T) {
		primary.set(http.StatusBadGateway, nil, 0)
		backup.set(http.StatusBadGateway, nil, 0)
		start := time.Now()
		for range 10 {
			if code := chat(h, "chat"); code < 500 {
				t.Fatalf("status %d with every target down", code)
			}
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("10 failing requests took %s", time.Since(start))
		}
	})
}
