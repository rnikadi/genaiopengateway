package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// script answers each call with the next status in its list, repeating
// the last one.
type script struct {
	mu       sync.Mutex
	statuses []int
	header   http.Header
	calls    int
	wait     time.Duration
}

func (s *script) Supports(provider.Endpoint) bool { return true }

func (s *script) Send(ctx context.Context, _ *provider.Request) (*provider.Response, error) {
	s.mu.Lock()
	i := min(s.calls, len(s.statuses)-1)
	s.calls++
	status, wait := s.statuses[i], s.wait
	s.mu.Unlock()
	if wait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return &provider.Response{StatusCode: status, Header: s.header, Body: io.NopCloser(strings.NewReader("{}")), Usage: &usage.Tokens{}}, nil
}

func (s *script) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// routed builds a router with one model over the given providers, and
// sleeps that only record what they were asked to wait.
func routed(m config.Model, ps map[string]*script, names ...string) (*Router, *[]time.Duration) {
	var waits []time.Duration
	r := &Router{models: map[string]config.Model{}, providers: map[string]provider.Provider{}, health: NewHealth(),
		breaker: config.CircuitBreaker{Failures: 2, Cooldown: config.Duration(time.Minute)},
		sleep:   func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}
	for _, n := range names {
		r.providers[n] = ps[n]
		if len(m.Targets) < len(names) {
			m.Targets = append(m.Targets, config.Target{Provider: n, Model: n})
		}
	}
	r.models["m"] = m
	return r, &waits
}

func route(t *testing.T, r *Router, b map[string]json.RawMessage) *Result {
	t.Helper()
	if b == nil {
		b = body()
	}
	res, err := r.Route(context.Background(), provider.ChatCompletions, "m", b, provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func ok() *script { return &script{statuses: []int{200}} }

func TestRoundRobin(t *testing.T) {
	ps := map[string]*script{"a": ok(), "b": ok(), "c": ok()}
	r, _ := routed(config.Model{Strategy: config.StrategyRoundRobin}, ps, "a", "b", "c")
	var got []string
	for range 6 {
		got = append(got, route(t, r, nil).Target.Provider)
	}
	if strings.Join(got, "") != "abcabc" {
		t.Fatalf("order = %v", got)
	}
}

func TestWeighted(t *testing.T) {
	ps := map[string]*script{"a": ok(), "b": ok()}
	m := config.Model{Strategy: config.StrategyWeighted, Targets: []config.Target{{Provider: "a", Model: "a", Weight: 9}, {Provider: "b", Model: "b", Weight: 1}}}
	r, _ := routed(m, ps, "a", "b")
	for range 2000 {
		route(t, r, nil)
	}
	if a := ps["a"].count(); a < 1650 || a > 1950 {
		t.Fatalf("a got %d of 2000, want about 1800", a)
	}
}

func TestStickyUser(t *testing.T) {
	ps := map[string]*script{"a": ok(), "b": ok(), "c": ok(), "d": ok()}
	r, _ := routed(config.Model{Strategy: config.StrategyRoundRobin, Sticky: "user"}, ps, "a", "b", "c", "d")
	users := map[string]string{}
	for i := range 40 {
		b := body()
		u := string(rune('a' + i%8))
		if i%2 == 0 {
			b["user"], _ = json.Marshal(u)
		} else {
			b["metadata"], _ = json.Marshal(map[string]string{"user_id": u})
		}
		got := route(t, r, b).Target.Provider
		if prev, seen := users[u]; seen && prev != got {
			t.Fatalf("user %s went to %s then %s", u, prev, got)
		}
		users[u] = got
	}
	spread := map[string]bool{}
	for _, p := range users {
		spread[p] = true
	}
	if len(spread) < 2 {
		t.Fatalf("all users on one target: %v", users)
	}
}

func TestLeastLatency(t *testing.T) {
	slow, fast := &script{statuses: []int{200}, wait: 20 * time.Millisecond}, ok()
	r, _ := routed(config.Model{Strategy: config.StrategyLeastLatency}, map[string]*script{"slow": slow, "fast": fast}, "slow", "fast")
	// The first two requests learn each target's latency.
	route(t, r, nil)
	route(t, r, nil)
	for range 5 {
		if got := route(t, r, nil).Target.Provider; got != "fast" {
			t.Fatalf("target = %s", got)
		}
	}
}

func TestLowestCost(t *testing.T) {
	m := config.Model{Strategy: config.StrategyLowestCost, Targets: []config.Target{
		{Provider: "dear", Model: "dear", Price: config.Price{InputPerMTok: 3, OutputPerMTok: 15}},
		{Provider: "cheap", Model: "cheap", Price: config.Price{InputPerMTok: 0.1, OutputPerMTok: 0.4}},
	}}
	r, _ := routed(m, map[string]*script{"dear": ok(), "cheap": ok()}, "dear", "cheap")
	if got := route(t, r, nil).Target.Provider; got != "cheap" {
		t.Fatalf("target = %s", got)
	}
}

func TestRetriesSameTarget(t *testing.T) {
	a := &script{statuses: []int{503, 429, 200}}
	m := config.Model{Retries: config.Retries{Attempts: 2, Backoff: config.Duration(100 * time.Millisecond), MaxBackoff: config.Duration(time.Second)}}
	r, waits := routed(m, map[string]*script{"a": a}, "a")
	res := route(t, r, nil)
	if res.Response.StatusCode != 200 || a.count() != 3 || len(res.Failed) != 2 {
		t.Fatalf("status %d after %d calls, %d failed", res.Response.StatusCode, a.count(), len(res.Failed))
	}
	if len(*waits) != 2 || (*waits)[0] > 100*time.Millisecond || (*waits)[1] > 200*time.Millisecond {
		t.Fatalf("waits = %v", *waits)
	}
}

func TestRetriesExhaustedFallBack(t *testing.T) {
	a, b := &script{statuses: []int{500}}, ok()
	m := config.Model{Retries: config.Retries{Attempts: 1, Backoff: config.Duration(time.Millisecond), MaxBackoff: config.Duration(time.Second)}}
	r, _ := routed(m, map[string]*script{"a": a, "b": b}, "a", "b")
	if got := route(t, r, nil).Target.Provider; got != "b" || a.count() != 2 {
		t.Fatalf("target %s after %d calls to a", got, a.count())
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	a := &script{statuses: []int{429, 200}, header: http.Header{"Retry-After": {"2"}}}
	m := config.Model{Retries: config.Retries{Attempts: 1, Backoff: config.Duration(time.Millisecond), MaxBackoff: config.Duration(5 * time.Second)}}
	r, waits := routed(m, map[string]*script{"a": a}, "a")
	route(t, r, nil)
	if len(*waits) != 1 || (*waits)[0] != 2*time.Second {
		t.Fatalf("waits = %v", *waits)
	}
}

func TestTargetTimeout(t *testing.T) {
	slow, fast := &script{statuses: []int{200}, wait: time.Second}, ok()
	m := config.Model{Targets: []config.Target{{Provider: "slow", Model: "slow", Timeout: config.Duration(20 * time.Millisecond)}, {Provider: "fast", Model: "fast"}}}
	r, _ := routed(m, map[string]*script{"slow": slow, "fast": fast}, "slow", "fast")
	start := time.Now()
	res := route(t, r, nil)
	if res.Target.Provider != "fast" || len(res.Failed) != 1 || !errors.Is(res.Failed[0].Err, errTargetTimeout) {
		t.Fatalf("target %s, failed %+v", res.Target.Provider, res.Failed)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("timeout not applied")
	}
}

func TestTimeoutSparesStreamingBody(t *testing.T) {
	a := ok()
	m := config.Model{Targets: []config.Target{{Provider: "a", Model: "a", Timeout: config.Duration(10 * time.Millisecond)}}}
	r, _ := routed(m, map[string]*script{"a": a}, "a")
	res := route(t, r, nil)
	time.Sleep(30 * time.Millisecond)
	if _, err := io.ReadAll(res.Response.Body); err != nil {
		t.Fatalf("body read after timeout: %v", err)
	}
	_ = res.Response.Body.Close()
}

func TestCircuitBreaker(t *testing.T) {
	a, b := &script{statuses: []int{500}}, ok()
	r, _ := routed(config.Model{}, map[string]*script{"a": a, "b": b}, "a", "b")
	now := time.Now()
	r.health.now = func() time.Time { return now }
	route(t, r, nil)
	route(t, r, nil) // second failure opens a's breaker
	if st := r.health.States()["a/a"]; st.State != "open" {
		t.Fatalf("state = %+v", st)
	}
	route(t, r, nil)
	if a.count() != 2 {
		t.Fatalf("open breaker still sent: %d calls", a.count())
	}
	// After the cooldown one request probes a; it fails, so a opens again.
	now = now.Add(2 * time.Minute)
	if st := r.health.States()["a/a"]; st.State != "half_open" {
		t.Fatalf("state = %+v", st)
	}
	route(t, r, nil)
	route(t, r, nil)
	if a.count() != 3 {
		t.Fatalf("calls to a = %d, want 3 (one probe)", a.count())
	}
	// A successful probe closes it.
	now = now.Add(2 * time.Minute)
	a.mu.Lock()
	a.statuses = []int{200}
	a.mu.Unlock()
	if got := route(t, r, nil).Target.Provider; got != "a" {
		t.Fatalf("probe target = %s", got)
	}
	if st := r.health.States()["a/a"]; st.State != "closed" {
		t.Fatalf("state = %+v", st)
	}
}

func TestAllOpenStillTried(t *testing.T) {
	a := &script{statuses: []int{500, 500, 200}}
	r, _ := routed(config.Model{}, map[string]*script{"a": a}, "a")
	route(t, r, nil)
	route(t, r, nil)
	if res := route(t, r, nil); res.Response.StatusCode != 200 {
		t.Fatalf("status = %d", res.Response.StatusCode)
	}
}
