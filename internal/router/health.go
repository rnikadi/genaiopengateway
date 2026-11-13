package router

import (
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// Health remembers how each target has been answering: a latency
// average for the least_latency strategy, and a circuit breaker that
// takes a failing target out of rotation (docs/routing.md). It outlives
// config reloads, so a reload does not forget a broken upstream.
type Health struct {
	mu      sync.Mutex
	targets map[string]*targetHealth
	rr      sync.Map // model name to *atomic.Uint64, for round_robin
	now     func() time.Time
}

type targetHealth struct {
	ewma      time.Duration // TTFB average; zero until the first answer
	failures  int           // in a row
	openUntil time.Time     // zero while closed
	probing   bool          // a half-open probe is in flight
}

// CircuitState is a target's breaker state as shown to operators.
type CircuitState struct {
	Target    string        `json:"target"`
	State     string        `json:"state"` // closed, open or half_open
	Failures  int           `json:"failures"`
	OpenUntil *time.Time    `json:"open_until,omitempty"`
	Latency   time.Duration `json:"latency_ns"`
}

// A nil *Health is valid: it remembers nothing, so every breaker stays
// closed and round_robin always starts at the first target.

// NewHealth returns an empty health record.
func NewHealth() *Health {
	return &Health{targets: map[string]*targetHealth{}, now: time.Now}
}

func (h *Health) get(key string) *targetHealth {
	t := h.targets[key]
	if t == nil {
		t = &targetHealth{}
		h.targets[key] = t
	}
	return t
}

// ewmaWeight is how much one answer moves the latency average.
const ewmaWeight = 0.2

// observe records one try's outcome. failed is a connection failure, a
// timeout, a 429 or a 5xx.
func (h *Health) observe(key string, ttfb time.Duration, failed bool, cb config.CircuitBreaker) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.get(key)
	t.probing = false
	if !failed {
		if t.ewma == 0 {
			t.ewma = ttfb
		} else {
			t.ewma = time.Duration(ewmaWeight*float64(ttfb) + (1-ewmaWeight)*float64(t.ewma))
		}
		t.failures, t.openUntil = 0, time.Time{}
		return
	}
	t.failures++
	if cb.Failures > 0 && t.failures >= cb.Failures {
		t.openUntil = h.now().Add(cb.Cooldown.Std())
	}
}

// admit reports whether a target may take a request: its breaker is
// closed, or its cooldown is over and no other probe is in flight, in
// which case this request becomes the probe.
func (h *Health) admit(key string) bool {
	if h == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.targets[key]
	if t == nil || t.openUntil.IsZero() {
		return true
	}
	if h.now().Before(t.openUntil) || t.probing {
		return false
	}
	t.probing = true
	return true
}

func (h *Health) latency(key string) time.Duration {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if t := h.targets[key]; t != nil {
		return t.ewma
	}
	return 0
}

// States lists every target the gateway has sent to, with its breaker
// state.
func (h *Health) States() map[string]CircuitState {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	out := make(map[string]CircuitState, len(h.targets))
	for key, t := range h.targets {
		s := CircuitState{Target: key, State: "closed", Failures: t.failures, Latency: t.ewma}
		if !t.openUntil.IsZero() {
			until := t.openUntil
			s.OpenUntil = &until
			s.State = "open"
			if !now.Before(until) {
				s.State = "half_open"
			}
		}
		out[key] = s
	}
	return out
}

func (h *Health) counter(model string) *atomic.Uint64 {
	c, _ := h.rr.LoadOrStore(model, new(atomic.Uint64))
	return c.(*atomic.Uint64)
}

// order returns the targets in the order the model's strategy tries
// them. stickyKey, when set, makes the order the same for every request
// with that key.
func (h *Health) order(name string, m config.Model, targets []config.Target, stickyKey string) []config.Target {
	out := slices.Clone(targets)
	if len(out) < 2 {
		return out
	}
	var rng *rand.Rand
	if stickyKey != "" {
		f := fnv.New64a()
		_, _ = f.Write([]byte(stickyKey))
		seed := f.Sum64()
		rng = rand.New(rand.NewPCG(seed, seed>>1|1))
	}
	switch m.Strategy {
	case config.StrategyWeighted:
		return weightedOrder(out, rng)
	case config.StrategyRoundRobin:
		var start int
		if rng != nil {
			start = rng.IntN(len(out))
		} else if h != nil {
			start = int((h.counter(name).Add(1) - 1) % uint64(len(out)))
		}
		return append(out[start:], out[:start]...)
	case config.StrategyLeastLatency:
		// Targets without an answer yet sort first, so each gets tried.
		slices.SortStableFunc(out, func(a, b config.Target) int {
			return int(h.latency(a.String()) - h.latency(b.String()))
		})
	case config.StrategyLowestCost:
		slices.SortStableFunc(out, func(a, b config.Target) int {
			ca, cb := unitCost(a.Price), unitCost(b.Price)
			switch {
			case ca < cb:
				return -1
			case ca > cb:
				return 1
			}
			return 0
		})
	}
	return out
}

// unitCost compares targets' prices: a typical request's tokens, plus
// the per-request and media prices that dominate other endpoints.
func unitCost(p config.Price) float64 {
	return p.InputPerMTok*3 + p.OutputPerMTok + p.PerRequest*1e6 + p.PerImage*1e6 + p.PerMChars + p.PerAudioMinute*1e3 + p.PerVideoSecond*1e6
}

// weightedOrder draws targets without replacement, each in proportion to
// its weight.
func weightedOrder(ts []config.Target, rng *rand.Rand) []config.Target {
	intn := rand.IntN
	if rng != nil {
		intn = rng.IntN
	}
	out := make([]config.Target, 0, len(ts))
	for len(ts) > 0 {
		total := 0
		for _, t := range ts {
			total += max(t.Weight, 1)
		}
		pick, i := intn(total), 0
		for ; i < len(ts)-1; i++ {
			pick -= max(ts[i].Weight, 1)
			if pick < 0 {
				break
			}
		}
		out = append(out, ts[i])
		ts = slices.Delete(ts, i, i+1)
	}
	return out
}
