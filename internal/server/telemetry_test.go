package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := do(h, "GET", "/metrics", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status %d", rec.Code)
	}
	return rec.Body.String()
}

func TestMetrics(t *testing.T) {
	var badHits, goodHits int
	bad := upstream(t, http.StatusServiceUnavailable, &badHits)
	good := upstream(t, http.StatusOK, &goodHits)
	h := gateway(t, `
providers:
  bad: {base_url: "`+bad.URL+`", api_key: upstream-key}
  good: {base_url: "`+good.URL+`", api_key: upstream-key}
models:
  chat:
    targets:
      - {provider: bad, model: primary}
      - {provider: good, model: backup, price: {input_per_mtok: 1000000, output_per_mtok: 1000000}}
  free: {targets: [{provider: good, model: free}]}
keys:
  - {name: dev, key: `+devKey+`}
  - {name: slow, key: slow-key-0123456789, rpm: 1}
`, nil)

	do(h, "POST", "/v1/chat/completions", devKey, `{"model":"chat","messages":[]}`)
	do(h, "POST", "/v1/chat/completions", devKey, `{"model":"free","messages":[]}`)
	do(h, "POST", "/v1/chat/completions", devKey, `{"model":"made-up-by-client","messages":[]}`)
	do(h, "POST", "/v1/chat/completions", "slow-key-0123456789", `{"model":"free","messages":[]}`)
	do(h, "POST", "/v1/chat/completions", "slow-key-0123456789", `{"model":"free","messages":[]}`)

	body := scrape(t, h)
	for _, want := range []string{
		`gog_requests_total{endpoint="chat.completions",error_code="",model="chat",status_class="2xx",target="good/backup"} 1`,
		`gog_requests_total{endpoint="chat.completions",error_code="model_not_found",model="",status_class="4xx",target=""} 1`,
		`gog_requests_total{endpoint="chat.completions",error_code="rate_limit_exceeded",model="free",status_class="4xx",target=""} 1`,
		`gog_fallbacks_total{from_target="bad/primary",model="chat",reason="server_error"} 1`,
		`gog_tokens_total{kind="input",model="chat",target="good/backup"} 10`,
		`gog_tokens_total{kind="output",model="chat",target="good/backup"} 5`,
		`gog_cost_usd_total{model="chat",target="good/backup"} 15`,
		`gog_unpriced_requests_total{target="good/free"} 2`,
		`gog_limit_rejections_total{kind="rpm",scope="key"} 1`,
		`gog_upstream_ttfb_seconds_count{target="bad/primary"} 1`,
		`gog_request_duration_seconds_count{endpoint="chat.completions",model="chat",target="good/backup"} 1`,
		`gog_usage_write_errors_total 0`,
		`gog_snapshot_age_seconds 0`,
		`go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
	if strings.Contains(body, "made-up-by-client") {
		t.Error("a client-chosen model name became a metric label")
	}
}

func TestMetricsOnOwnListener(t *testing.T) {
	h := gateway(t, basicConfig("http://127.0.0.1:1")+"metrics: {listen: \":0\"}\n", nil)
	if rec := do(h, "GET", "/metrics", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("/metrics on the main listener = %d, want 404 when metrics.listen is set", rec.Code)
	}
}

func TestTraces(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev); _ = tp.Shutdown(context.Background()) })

	var badHits, goodHits int
	bad := upstream(t, http.StatusServiceUnavailable, &badHits)
	good := upstream(t, http.StatusOK, &goodHits)
	h := gateway(t, `
providers:
  bad: {base_url: "`+bad.URL+`", api_key: upstream-key}
  good: {base_url: "`+good.URL+`", api_key: upstream-key}
models:
  chat: {targets: [{provider: bad, model: primary}, {provider: good, model: backup}]}
keys: [{name: dev, key: `+devKey+`}]
`, nil)

	req := strings.NewReader(`{"model":"chat","messages":[]}`)
	r, _ := http.NewRequest("POST", "/v1/chat/completions", req)
	r.Header.Set("Authorization", "Bearer "+devKey)
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	r.Header.Set("Traceparent", parent)
	rec := serve(h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	spans := sr.Ended()
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 2 upstream and 1 request", len(spans))
	}
	root := spans[2]
	if root.Name() != "gateway.request" || root.Parent().SpanID().String() != "00f067aa0ba902b7" ||
		root.SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("request span %s does not continue the caller's trace", root.Name())
	}
	attrs := map[string]string{}
	for _, kv := range root.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	for k, v := range map[string]string{
		"gen_ai.operation.name": "chat", "gen_ai.provider.name": "openai", "gen_ai.request.model": "backup",
		"gen_ai.usage.input_tokens": "10", "gen_ai.usage.output_tokens": "5", "gog.virtual_model": "chat",
		"gog.org_id": "org_static",
	} {
		if attrs[k] != v {
			t.Errorf("request span %s = %q, want %q", k, attrs[k], v)
		}
	}
	for i, model := range []string{"primary", "backup"} {
		s := spans[i]
		if s.Name() != "gateway.upstream" || s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatalf("span %d is %s, not an upstream child of the request", i, s.Name())
		}
		for _, kv := range s.Attributes() {
			if kv.Key == "gen_ai.request.model" && kv.Value.AsString() != model {
				t.Errorf("upstream span %d model %s, want %s", i, kv.Value.AsString(), model)
			}
		}
	}
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
