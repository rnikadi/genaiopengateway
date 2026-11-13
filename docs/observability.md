# Observability

## Logs

Structured JSON to stdout, one line per request at `info`:

`time`, `level`, `msg="request"`, `request_id`, `trace_id` (when tracing is on), `org_id`, `team_id`, `key_id`, `key_prefix`, `endpoint`, `model`, `target`, `status`, `latency_ms`, `upstream_latency_ms`, `input_tokens`, `output_tokens`, `cost_usd`, `fallbacks`, `error_code`.

MCP calls (`tools/call`, `prompts/get`, `resources/read`) log `msg="mcp request"` with `request_id`, `trace_id`, `org_id`, `team_id`, `key_id`, `virtual_server`, `method`, `server`, `name`, `status`, `latency_ms` and `error_code`.

Request and response bodies, tool arguments and tool results are never logged. Secrets are never logged.

## Metrics

Prometheus, at `/metrics`. All names start with `gog_`. Go runtime and process metrics are included.

`/metrics` needs no key. It is served on `metrics.listen` when set, which is the recommended setup: keep that port off the public network. Without it, `/metrics` is on the main listener.

| Metric | Type | Labels |
| --- | --- | --- |
| `gog_requests_total` | counter | `endpoint`, `model`, `target`, `status_class`, `error_code` |
| `gog_request_duration_seconds` | histogram | `endpoint`, `model`, `target` |
| `gog_upstream_ttfb_seconds` | histogram | `target` |
| `gog_tokens_total` | counter | `model`, `target`, `kind` (input, output, cached_input, reasoning) |
| `gog_cost_usd_total` | counter | `model`, `target` |
| `gog_fallbacks_total` | counter | `model`, `from_target`, `reason` |
| `gog_limit_rejections_total` | counter | `scope` (org, team, key), `kind` (rpm, tpm, budget) |
| `gog_unpriced_requests_total` | counter | `target` |
| `gog_usage_write_errors_total` | counter | none (usage, MCP call and guardrail event records) |
| `gog_snapshot_age_seconds` | gauge | none |
| `gen_ai_client_operation_duration_seconds` | histogram | `gen_ai_operation_name`, `gen_ai_provider_name`, `gen_ai_request_model`, `error_type` (OpenTelemetry `gen_ai.client.operation.duration`) |
| `gen_ai_client_token_usage` | histogram | `gen_ai_operation_name`, `gen_ai_provider_name`, `gen_ai_request_model`, `gen_ai_token_type` (input, output; OpenTelemetry `gen_ai.client.token.usage`) |
| `gog_stream_records_total` | counter | `stream`, `result` (delivered, dropped); see [streams](streams.md) |
| `gog_target_circuit_state` | gauge | `target`; 0 closed, 1 open, 2 half open ([routing](routing.md#circuit-breaker)) |
| `gog_mcp_requests_total` | counter | `virtual_server`, `method`, `status` (ok, tool_error, error, denied, rate_limited) |
| `gog_mcp_request_duration_seconds` | histogram | `virtual_server`, `method` |
| `gog_mcp_tool_calls_total` | counter | `server`, `tool`, `status` |
| `gog_a2a_requests_total` | counter | `agent`, `method`, `status` (ok, agent_error, upstream_error, invalid, not_found, guardrail_blocked, denied, rate_limited); see [A2A agents](a2a.md) |
| `gog_a2a_request_duration_seconds` | histogram | `agent`, `method`; to the end of a stream |
| `gog_mcp_upstream_up` | gauge | `server` |
| `gog_mcp_tools_pending_approval` | gauge | `server` |
| `gog_alerts_total` | counter | `event` |
| `gog_alert_deliveries_total` | counter | `webhook`, `result` (ok, retried, failed, dropped) |
| `gog_mcp_connections` | gauge | `server` |
| `gog_mcp_connect_flows_total` | counter | `server`, `result` (completed, denied, expired, error) |
| `gog_mcp_token_refreshes_total` | counter | `server`, `result` (ok, invalid_grant, error) |
| `gog_request_bodies_written_total` | counter | none |
| `gog_request_bodies_dropped_total` | counter | none (queue full or write failed) |
| `gog_guardrail_checks_total` | counter | `guardrail`, `stage`, `result` (pass, match, error) |
| `gog_guardrail_duration_seconds` | histogram | `guardrail`, `stage` |
| `gog_cache_lookups_total` | counter | `result` (hit, semantic, miss) |
| `gog_cache_saved_usd_total` | counter | none (what cached answers cost when first made) |
| `gog_redactions_total` | counter | `type` (email, phone, credit_card, iban, us_ssn, secret, custom) |

Labels never include org, team or key ids, to keep cardinality bounded. Per-tenant numbers come from the usage API.

## Dashboard and alerts

A Grafana dashboard and Prometheus alerting rules ship in the Helm chart's `files/` directory. The [runbook](runbook.md) explains how to install them and what to do for each alert.

## Traces

OpenTelemetry, exported over OTLP/HTTP (`http/protobuf`) when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set. The standard OTel environment variables apply, including `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_SERVICE_NAME` (default `genaiopengateway`), `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER` and `OTEL_SDK_DISABLED`. Incoming W3C `traceparent` is honoured. For gRPC-only backends, run an OpenTelemetry Collector in between.

Spans: `gateway.request` (server), with one client child per target tried. A client span is named for the operation and the upstream model, such as `chat gpt-4.1-mini`, as the [OpenTelemetry GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/) say. It ends when response headers arrive. The request span ends when the response has been streamed to the client.

Attributes follow the same conventions:

- `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model` and `gen_ai.response.model`.
- `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` and `gen_ai.usage.cache_read.input_tokens` on the request span.
- The request's own parameters, when it sets them: `gen_ai.request.max_tokens`, `temperature`, `top_p`, `top_k`, `seed`, `stop_sequences` and `choice.count`.
- `error.type` on a failed client span: the upstream's HTTP status, `timeout` or `connection_error`. On the request span it is the gateway's error code.
- `gog.virtual_model`, `gog.target`, `gog.org_id`, `gog.team_id`, `gog.fallbacks` and `gog.cache`.

Prompts and completions are never put on spans. Use [body logging](body-logging.md) for content, which is redacted and encrypted.

Upgrading from 0.8: client spans were named `gateway.upstream`.

MCP requests have an `mcp.request` (server) span, with an `mcp.upstream` (client) child when the request goes to an upstream. They carry `mcp.method.name`, `mcp.protocol.version` (upstream span), `gen_ai.tool.name`, `gog.virtual_server`, `gog.mcp_server`, `gog.org_id` and `gog.team_id`. Trace context is read from the request's `_meta` or its HTTP headers, and passed upstream in `_meta`.

## Usage records

One record per request, written to the store asynchronously in batches of up to 500 or every second. If the store is down, records queue in memory up to 100,000 and the oldest are dropped beyond that, counted in `gog_usage_write_errors_total`. See the data model in [config.md](config.md).
