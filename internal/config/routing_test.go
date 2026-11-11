package config

import (
	"strings"
	"testing"
	"time"
)

func TestRoutingValidation(t *testing.T) {
	base := "providers: {p: {base_url: \"http://x\"}}\nmodels:\n  m:\n"
	cases := []struct{ name, model, err string }{
		{"defaults", "    targets: [{provider: p, model: a}]\n", ""},
		{"weighted sticky", "    strategy: weighted\n    sticky: user\n    targets: [{provider: p, model: a, weight: 3}]\n", ""},
		{"unknown strategy", "    strategy: fastest\n    targets: [{provider: p, model: a}]\n", "strategy must be one of"},
		{"sticky needs strategy", "    sticky: user\n    targets: [{provider: p, model: a}]\n", "sticky routing needs"},
		{"too many retries", "    retries: {attempts: 9}\n    targets: [{provider: p, model: a}]\n", "retries.attempts"},
		{"negative weight", "    targets: [{provider: p, model: a, weight: -1}]\n", "weight and timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Parse([]byte(base + c.model))
			if c.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				m := cfg.Models["m"]
				if m.Retries.Backoff.Std() != DefaultRetryBackoff || m.Retries.MaxBackoff.Std() != DefaultRetryMaxBackoff {
					t.Fatalf("retry defaults not applied: %+v", m.Retries)
				}
				if cfg.Routing.CircuitBreaker.Failures != DefaultBreakerFailures || cfg.Routing.CircuitBreaker.Cooldown.Std() != 30*time.Second {
					t.Fatalf("breaker defaults: %+v", cfg.Routing.CircuitBreaker)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("err = %v, want %q", err, c.err)
			}
		})
	}
}
