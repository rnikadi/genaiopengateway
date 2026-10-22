package limits

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/policy"
)

func backendsFor(t *testing.T) map[string]Backend {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mr := miniredis.RunT(t)
	v := NewValkey(mr.Addr(), "")
	t.Cleanup(func() { v.Close() })
	out := map[string]Backend{"memory": NewMemory(ctx), "miniredis": v}
	// A real server runs when GOG_TEST_VALKEY_ADDR is set; its database
	// is flushed.
	if addr := os.Getenv("GOG_TEST_VALKEY_ADDR"); addr != "" {
		real := NewValkey(addr, "")
		if err := real.c.FlushDB(ctx).Err(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { real.Close() })
		out["valkey"] = real
	}
	return out
}

func principal(org, team, key policy.Policy) *auth.Principal {
	return &auth.Principal{OrgID: "org_a", TeamID: "team_a", KeyID: "key_a", Policies: [3]policy.Policy{org, team, key}}
}

// fixedClock pins the limiter to a time inside a minute window.
func fixedClock(l *Limiter) {
	at := time.Date(2026, 10, 22, 12, 0, 30, 0, time.UTC)
	l.now = func() time.Time { return at }
}

func TestRequestsPerMinuteTightestLevelWins(t *testing.T) {
	for name, b := range backendsFor(t) {
		t.Run(name, func(t *testing.T) {
			l := New(b, true, false)
			fixedClock(l)
			p := principal(policy.Policy{RPM: 10}, policy.Policy{}, policy.Policy{RPM: 2})
			ctx := context.Background()
			for i := 0; i < 2; i++ {
				if d := l.Admit(ctx, p); !d.Allowed {
					t.Fatalf("request %d rejected: %+v", i, d)
				}
			}
			d := l.Admit(ctx, p)
			if d.Allowed || d.Code != CodeRateLimited || d.RetryAfter != 30*time.Second {
				t.Fatalf("third request = %+v", d)
			}
			if d.Requests.Limit != 2 || d.Requests.Remaining != 0 {
				t.Fatalf("window = %+v", d.Requests)
			}
		})
	}
}

func TestTokensPerMinute(t *testing.T) {
	for name, b := range backendsFor(t) {
		t.Run(name, func(t *testing.T) {
			l := New(b, true, false)
			fixedClock(l)
			p := principal(policy.Policy{}, policy.Policy{TPM: 100}, policy.Policy{})
			ctx := context.Background()
			if d := l.Admit(ctx, p); !d.Allowed || d.Tokens.Remaining != 100 {
				t.Fatalf("first = %+v", d)
			}
			if err := l.Charge(ctx, p, 100, 0); err != nil {
				t.Fatal(err)
			}
			if d := l.Admit(ctx, p); d.Allowed || d.Code != CodeRateLimited {
				t.Fatalf("after 100 tokens = %+v", d)
			}
			// A new minute starts fresh.
			l.now = func() time.Time { return time.Date(2026, 10, 22, 12, 1, 5, 0, time.UTC) }
			if d := l.Admit(ctx, p); !d.Allowed {
				t.Fatalf("next minute = %+v", d)
			}
		})
	}
}

func TestBudgets(t *testing.T) {
	for name, b := range backendsFor(t) {
		t.Run(name, func(t *testing.T) {
			l := New(b, true, false)
			fixedClock(l)
			p := principal(policy.Policy{Budget: &policy.Budget{AmountUSD: 1, Period: policy.PeriodMonthly}}, policy.Policy{}, policy.Policy{})
			ctx := context.Background()
			if d := l.Admit(ctx, p); !d.Allowed {
				t.Fatal("rejected before spending")
			}
			_ = l.Charge(ctx, p, 10, 0.6)
			if d := l.Admit(ctx, p); !d.Allowed {
				t.Fatal("rejected under budget")
			}
			_ = l.Charge(ctx, p, 10, 0.6)
			d := l.Admit(ctx, p)
			if d.Allowed || d.Code != CodeBudget {
				t.Fatalf("over budget = %+v", d)
			}
			// Next month resets.
			l.now = func() time.Time { return time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC) }
			if d := l.Admit(ctx, p); !d.Allowed {
				t.Fatal("budget did not reset in a new month")
			}
		})
	}
}

func TestSeedNeverLowers(t *testing.T) {
	for name, b := range backendsFor(t) {
		t.Run(name, func(t *testing.T) {
			l := New(b, true, false)
			fixedClock(l)
			p := principal(policy.Policy{}, policy.Policy{}, policy.Policy{Budget: &policy.Budget{AmountUSD: 5, Period: policy.PeriodDaily}})
			ctx := context.Background()
			if err := l.Seed(ctx, policy.PeriodDaily, map[string]float64{"key_a": 5}); err != nil {
				t.Fatal(err)
			}
			if d := l.Admit(ctx, p); d.Allowed {
				t.Fatal("seeded spend ignored")
			}
			_ = l.Seed(ctx, policy.PeriodDaily, map[string]float64{"key_a": 1})
			if d := l.Admit(ctx, p); d.Allowed {
				t.Fatal("seed lowered the counter")
			}
		})
	}
}

func TestConcurrentRequestsNeverExceedRPM(t *testing.T) {
	for name, b := range backendsFor(t) {
		t.Run(name, func(t *testing.T) {
			l := New(b, true, false)
			fixedClock(l)
			p := principal(policy.Policy{}, policy.Policy{}, policy.Policy{RPM: 50})
			var mu sync.Mutex
			allowed := 0
			var wg sync.WaitGroup
			for i := 0; i < 200; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if l.Admit(context.Background(), p).Allowed {
						mu.Lock()
						allowed++
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			if allowed != 50 {
				t.Fatalf("allowed %d, want exactly 50", allowed)
			}
		})
	}
}

type broken struct{}

func (broken) Add(context.Context, string, float64, time.Duration) (float64, error) {
	return 0, errors.New("down")
}
func (broken) Get(context.Context, ...string) ([]float64, error) { return nil, errors.New("down") }
func (broken) Raise(context.Context, string, float64, time.Duration) error {
	return errors.New("down")
}

func TestFailModes(t *testing.T) {
	rate := principal(policy.Policy{RPM: 1}, policy.Policy{}, policy.Policy{})
	budget := principal(policy.Policy{Budget: &policy.Budget{AmountUSD: 1, Period: policy.PeriodTotal}}, policy.Policy{}, policy.Policy{})
	ctx := context.Background()
	if d := New(broken{}, true, false).Admit(ctx, rate); !d.Allowed {
		t.Fatal("rate limits should fail open")
	}
	if d := New(broken{}, true, false).Admit(ctx, budget); d.Allowed || d.Code != CodeUnavailable {
		t.Fatalf("budgets should fail closed: %+v", d)
	}
	if d := New(broken{}, false, true).Admit(ctx, rate); d.Allowed {
		t.Fatal("rate limits configured to fail closed")
	}
	if d := New(broken{}, false, true).Admit(ctx, budget); !d.Allowed {
		t.Fatal("budgets configured to fail open")
	}
}

func TestNoPoliciesTouchNothing(t *testing.T) {
	if d := New(broken{}, false, false).Admit(context.Background(), principal(policy.Policy{}, policy.Policy{}, policy.Policy{})); !d.Allowed {
		t.Fatal("a principal with no limits must not depend on the backend")
	}
}

func TestMemoryExpiry(t *testing.T) {
	m := NewMemory(context.Background())
	now := time.Now()
	m.now = func() time.Time { return now }
	_, _ = m.Add(context.Background(), "k", 1, time.Second)
	now = now.Add(2 * time.Second)
	if v, _ := m.Get(context.Background(), "k"); v[0] != 0 {
		t.Fatalf("expired key read %v", v[0])
	}
	m.sweep()
	if len(m.vals) != 0 {
		t.Fatal("sweep kept expired key")
	}
}
