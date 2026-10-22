# Observability

## Logs

Structured JSON to stdout, one line per request at `info`:

`time`, `level`, `msg="request"`, `request_id`, `trace_id` (when tracing is on), `org_id`, `team_id`, `key_id`, `key_prefix`, `endpoint`, `model`, `target`, `status`, `latency_ms`, `upstream_latency_ms`, `input_tokens`, `output_tokens`, `cost_usd`, `fallbacks`, `error_code`.

Request and response bodies are never logged in v0.1. Secrets are never logged.

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
| `gog_usage_write_errors_total` | counter | none |
| `gog_snapshot_age_seconds` | gauge | none |

Labels never include org, team or key ids, to keep cardinality bounded. Per-tenant numbers come from the usage API.

## Traces

OpenTelemetry, exported over OTLP/HTTP (`http/protobuf`) when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set. The standard OTel environment variables apply, including `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_SERVICE_NAME` (default `genaiopengateway`), `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_TRACES_SAMPLER` and `OTEL_SDK_DISABLED`. Incoming W3C `traceparent` is honoured. For gRPC-only backends, run an OpenTelemetry Collector in between.

Spans: `gateway.request` (server), with one `gateway.upstream` (client) child per target tried. An upstream span ends when response headers arrive; the request span ends when the response has been streamed to the client. Attributes follow the [OpenTelemetry GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/): `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, plus `gog.virtual_model`, `gog.org_id`, `gog.team_id`.

## Usage records

One record per request, written to the store asynchronously in batches of up to 500 or every second. If the store is down, records queue in memory up to 100,000 and the oldest are dropped beyond that, counted in `gog_usage_write_errors_total`. See the data model in [config.md](config.md).
