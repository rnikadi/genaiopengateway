// Package telemetry holds the gateway's Prometheus metrics and
// OpenTelemetry tracing setup. docs/observability.md lists every signal.
package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// Metrics are the gateway's Prometheus metrics. Labels never carry
// tenant ids, so cardinality is bounded by config.
type Metrics struct {
	reg *prometheus.Registry

	requests       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	ttfb           *prometheus.HistogramVec
	tokens         *prometheus.CounterVec
	cost           *prometheus.CounterVec
	fallbacks      *prometheus.CounterVec
	limitRejection *prometheus.CounterVec
	unpriced       *prometheus.CounterVec

	mcpRequests  *prometheus.CounterVec
	mcpDuration  *prometheus.HistogramVec
	mcpToolCalls *prometheus.CounterVec
	mcpUp        *prometheus.GaugeVec
	mcpPending   *prometheus.GaugeVec

	alerts          *prometheus.CounterVec
	alertDeliveries *prometheus.CounterVec

	bodiesWritten prometheus.Counter
	redactions    *prometheus.CounterVec

	mcpConnections *prometheus.GaugeVec
	connectFlows   *prometheus.CounterVec
	tokenRefreshes *prometheus.CounterVec

	guardrailChecks   *prometheus.CounterVec
	guardrailDuration *prometheus.HistogramVec
}

// Model request latencies span milliseconds (embeddings) to minutes
// (long generations).
var latencyBuckets = []float64{.025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20, 40, 80, 160, 320}

// NewMetrics registers all metrics on a new registry. usageErrors,
// snapshotAge and bodiesDropped are read at scrape time.
func NewMetrics(usageErrors, snapshotAge, bodiesDropped func() float64) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_requests_total", Help: "Model requests served.",
		}, []string{"endpoint", "model", "target", "status_class", "error_code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gog_request_duration_seconds", Help: "Time to serve a model request, to the last byte.", Buckets: latencyBuckets,
		}, []string{"endpoint", "model", "target"}),
		ttfb: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gog_upstream_ttfb_seconds", Help: "Time from sending a request upstream to its response headers.", Buckets: latencyBuckets,
		}, []string{"target"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_tokens_total", Help: "Tokens reported by upstreams.",
		}, []string{"model", "target", "kind"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_cost_usd_total", Help: "Cost of model requests in US dollars, from configured prices.",
		}, []string{"model", "target"}),
		fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_fallbacks_total", Help: "Targets that failed and were skipped for the next one.",
		}, []string{"model", "from_target", "reason"}),
		limitRejection: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_limit_rejections_total", Help: "Requests rejected by rate limits or budgets.",
		}, []string{"scope", "kind"}),
		unpriced: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_unpriced_requests_total", Help: "Successful requests to targets with no price configured.",
		}, []string{"target"}),
		mcpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_mcp_requests_total", Help: "MCP requests served.",
		}, []string{"virtual_server", "method", "status"}),
		mcpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gog_mcp_request_duration_seconds", Help: "Time to serve an MCP request.", Buckets: latencyBuckets,
		}, []string{"virtual_server", "method"}),
		mcpToolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_mcp_tool_calls_total", Help: "Tool calls forwarded to upstream MCP servers.",
		}, []string{"server", "tool", "status"}),
		mcpUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gog_mcp_upstream_up", Help: "Whether the last catalog refresh of an upstream MCP server succeeded.",
		}, []string{"server"}),
		mcpPending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gog_mcp_tools_pending_approval", Help: "Upstream MCP tools hidden until an admin approves their definition.",
		}, []string{"server"}),
		alerts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_alerts_total", Help: "Alerts raised.",
		}, []string{"event"}),
		alertDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_alert_deliveries_total", Help: "Alert delivery attempts to webhooks, by result.",
		}, []string{"webhook", "result"}),
		bodiesWritten: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gog_request_bodies_written_total", Help: "Request and response bodies written to the store.",
		}),
		redactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_redactions_total", Help: "Values redacted from logged bodies, by type.",
		}, []string{"type"}),
		guardrailChecks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_guardrail_checks_total", Help: "Guardrail checks, by result: pass, match or error.",
		}, []string{"guardrail", "stage", "result"}),
		guardrailDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gog_guardrail_duration_seconds", Help: "Time to run one guardrail check.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"guardrail", "stage"}),
		mcpConnections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gog_mcp_connections", Help: "Users connected to each user_auth MCP server.",
		}, []string{"server"}),
		connectFlows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_mcp_connect_flows_total", Help: "MCP connect flows finished, by result.",
		}, []string{"server", "result"}),
		tokenRefreshes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gog_mcp_token_refreshes_total", Help: "Refreshes of users' upstream MCP tokens, by result.",
		}, []string{"server", "result"}),
	}
	m.reg.MustRegister(
		m.requests, m.duration, m.ttfb, m.tokens, m.cost, m.fallbacks, m.limitRejection, m.unpriced,
		m.mcpRequests, m.mcpDuration, m.mcpToolCalls, m.mcpUp, m.mcpPending,
		m.alerts, m.alertDeliveries, m.bodiesWritten, m.redactions,
		m.mcpConnections, m.connectFlows, m.tokenRefreshes, m.guardrailChecks, m.guardrailDuration,
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "gog_request_bodies_dropped_total", Help: "Bodies not logged because the queue was full or the write failed.",
		}, bodiesDropped),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "gog_usage_write_errors_total", Help: "Usage records that could not be written to the store.",
		}, usageErrors),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "gog_snapshot_age_seconds", Help: "Seconds since keys and tenants were last loaded from the store.",
		}, snapshotAge),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Request is what one model request contributes to metrics.
type Request struct {
	Endpoint, Model, Target string
	Status                  int
	ErrorCode               string
	Duration                time.Duration
	Tokens                  usage.Tokens
	CostUSD                 float64
	// Unpriced is set when the target has no price and the request
	// used tokens.
	Unpriced bool
}

// ObserveRequest records a finished model request.
func (m *Metrics) ObserveRequest(r Request) {
	m.requests.WithLabelValues(r.Endpoint, r.Model, r.Target, statusClass(r.Status), r.ErrorCode).Inc()
	if r.Target == "" {
		return
	}
	m.duration.WithLabelValues(r.Endpoint, r.Model, r.Target).Observe(r.Duration.Seconds())
	for kind, n := range map[string]int64{
		"input": r.Tokens.InputTokens, "output": r.Tokens.OutputTokens,
		"cached_input": r.Tokens.CachedInputTokens, "reasoning": r.Tokens.ReasoningTokens,
	} {
		if n > 0 {
			m.tokens.WithLabelValues(r.Model, r.Target, kind).Add(float64(n))
		}
	}
	if r.CostUSD > 0 {
		m.cost.WithLabelValues(r.Model, r.Target).Add(r.CostUSD)
	}
	if r.Unpriced {
		m.unpriced.WithLabelValues(r.Target).Inc()
	}
}

// ObserveUpstream records the time a target took to answer.
func (m *Metrics) ObserveUpstream(target string, ttfb time.Duration) {
	m.ttfb.WithLabelValues(target).Observe(ttfb.Seconds())
}

// ObserveFallback records a target that failed and was skipped.
func (m *Metrics) ObserveFallback(model, target string, status int) {
	reason := "connection_error"
	switch {
	case status == http.StatusTooManyRequests:
		reason = "rate_limited"
	case status >= 500:
		reason = "server_error"
	}
	m.fallbacks.WithLabelValues(model, target, reason).Inc()
}

// ObserveLimitRejection records a request rejected by a limit.
func (m *Metrics) ObserveLimitRejection(scope, kind string) {
	m.limitRejection.WithLabelValues(scope, kind).Inc()
}

// MCP outcomes, used as the status label and in audit records.
const (
	MCPOk          = "ok"
	MCPToolError   = "tool_error"
	MCPError       = "error"
	MCPDenied      = "denied"
	MCPRateLimited = "rate_limited"
)

// ObserveMCPRequest records a finished MCP request.
func (m *Metrics) ObserveMCPRequest(virtualServer, method, status string, d time.Duration) {
	m.mcpRequests.WithLabelValues(virtualServer, method, status).Inc()
	m.mcpDuration.WithLabelValues(virtualServer, method).Observe(d.Seconds())
}

// ObserveMCPToolCall records a tool call forwarded upstream.
func (m *Metrics) ObserveMCPToolCall(server, tool, status string) {
	m.mcpToolCalls.WithLabelValues(server, tool, status).Inc()
}

// SetMCPUpstreamUp records whether an upstream server is reachable.
func (m *Metrics) SetMCPUpstreamUp(server string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	m.mcpUp.WithLabelValues(server).Set(v)
}

// SetMCPPendingTools records how many of a server's tools await approval.
func (m *Metrics) SetMCPPendingTools(server string, n int) {
	m.mcpPending.WithLabelValues(server).Set(float64(n))
}

// ForgetMCPUpstream removes a server that is no longer configured.
func (m *Metrics) ForgetMCPUpstream(server string) {
	m.mcpUp.DeleteLabelValues(server)
	m.mcpPending.DeleteLabelValues(server)
}

// ObserveAlert counts a raised alert.
func (m *Metrics) ObserveAlert(event string) { m.alerts.WithLabelValues(event).Inc() }

// ObserveAlertDelivery counts a delivery result: ok, retried, failed or
// dropped.
func (m *Metrics) ObserveAlertDelivery(webhook, result string) {
	m.alertDeliveries.WithLabelValues(webhook, result).Inc()
}

// ObserveBodies counts body records written to the store.
func (m *Metrics) ObserveBodies(n int) { m.bodiesWritten.Add(float64(n)) }

// ObserveRedactions counts redacted values by type.
func (m *Metrics) ObserveRedactions(counts map[string]int) {
	for typ, n := range counts {
		m.redactions.WithLabelValues(typ).Add(float64(n))
	}
}

// ObserveGuardrail records one guardrail check.
func (m *Metrics) ObserveGuardrail(guardrail, stage, result string, d time.Duration) {
	m.guardrailChecks.WithLabelValues(guardrail, stage, result).Inc()
	m.guardrailDuration.WithLabelValues(guardrail, stage).Observe(d.Seconds())
}

// SetMCPConnections sets how many users connected a server.
func (m *Metrics) SetMCPConnections(server string, n int) {
	m.mcpConnections.WithLabelValues(server).Set(float64(n))
}

// ObserveConnectFlow counts a finished connect flow: completed, denied,
// expired or error.
func (m *Metrics) ObserveConnectFlow(server, result string) {
	m.connectFlows.WithLabelValues(server, result).Inc()
}

// ObserveTokenRefresh counts a token refresh: ok, invalid_grant or error.
func (m *Metrics) ObserveTokenRefresh(server, result string) {
	m.tokenRefreshes.WithLabelValues(server, result).Inc()
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return strconv.Itoa(status/100) + "xx"
}
