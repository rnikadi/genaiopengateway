package server

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// TestCircuitBreakerSurvivesReload checks that an open breaker skips its
// target, shows in metrics, and is still open after a config reload.
func TestCircuitBreakerSurvivesReload(t *testing.T) {
	var badHits, goodHits int
	bad := upstream(t, http.StatusServiceUnavailable, &badHits)
	good := upstream(t, http.StatusOK, &goodHits)
	cfg, err := config.Parse([]byte(`
providers:
  bad: {base_url: "` + bad.URL + `", api_key: upstream-key}
  good: {base_url: "` + good.URL + `", api_key: upstream-key}
routing: {circuit_breaker: {failures: 2, cooldown: 1h}}
models:
  chat: {targets: [{provider: bad, model: primary}, {provider: good, model: backup}]}
keys:
  - {name: dev, key: ` + devKey + `}
`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for range 4 {
		if rec := do(h, "POST", "/v1/chat/completions", devKey, `{"model":"chat","messages":[]}`); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if badHits != 2 || goodHits != 4 {
		t.Fatalf("bad got %d, good %d; want 2 and 4", badHits, goodHits)
	}
	if body := scrape(t, h); !strings.Contains(body, `gog_target_circuit_state{target="bad/primary"} 1`) {
		t.Fatal("open breaker not in metrics")
	}
	if err := s.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	do(h, "POST", "/v1/chat/completions", devKey, `{"model":"chat","messages":[]}`)
	if badHits != 2 {
		t.Fatalf("reload closed the breaker: bad got %d", badHits)
	}
}

// TestResidencyPinning checks that a pinned key only reaches providers
// with its residency label, and is refused when the model has none.
func TestResidencyPinning(t *testing.T) {
	var usHits, euHits int
	us := upstream(t, http.StatusOK, &usHits)
	eu := upstream(t, http.StatusOK, &euHits)
	h := gateway(t, `
providers:
  us: {base_url: "`+us.URL+`", api_key: upstream-key, residency: us}
  eu: {base_url: "`+eu.URL+`", api_key: upstream-key, residency: eu}
models:
  chat: {targets: [{provider: us, model: a}, {provider: eu, model: b}]}
  us-only: {targets: [{provider: us, model: a}]}
keys:
  - {name: any, key: `+devKey+`}
  - {name: eu, key: eu-key-0123456789, residency: [eu]}
`, nil)
	if rec := do(h, "POST", "/v1/chat/completions", devKey, `{"model":"chat","messages":[]}`); rec.Code != http.StatusOK || usHits != 1 {
		t.Fatalf("unpinned: status %d, us hits %d", rec.Code, usHits)
	}
	if rec := do(h, "POST", "/v1/chat/completions", "eu-key-0123456789", `{"model":"chat","messages":[]}`); rec.Code != http.StatusOK || euHits != 1 || usHits != 1 {
		t.Fatalf("pinned: status %d, eu hits %d, us hits %d", rec.Code, euHits, usHits)
	}
	rec := do(h, "POST", "/v1/chat/completions", "eu-key-0123456789", `{"model":"us-only","messages":[]}`)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != codeResidency || usHits != 1 {
		t.Fatalf("no eu target: status %d, code %s, us hits %d", rec.Code, errorCode(t, rec), usHits)
	}
}
