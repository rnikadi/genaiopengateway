# Architecture

The gateway is split into a stateless **data plane** that serves every request and a **control plane** that manages tenants, keys, policies and config. In v0.1 both run in one binary (ADR 0010).

Model calls (and MCP calls from v0.2, see [mcp.md](mcp.md)) pass through the same chain:

1. **Ingress:** HTTP and SSE today; WebSocket and WebRTC with realtime audio in v0.8.
2. **Authentication:** gateway key to org, team and key policy, from the in-memory snapshot.
3. **Policy and limits:** model allow lists, RPM, TPM and budgets at every level.
4. **Guardrails:** request size limits in v0.1; PII, patterns, classifier models and webhooks in v0.5 ([guardrails.md](guardrails.md)).
5. **Router:** virtual model to ordered targets, fallback on 429, 5xx and connection errors.
6. **Adapters:** passthrough or translation to the provider API.
7. **Metering:** usage extracted from the response or stream, cost computed, record queued.

## Packages

| Package | Purpose |
| --- | --- |
| `cmd/gateway` | Binary: flags, config load, signal handling, graceful shutdown |
| `internal/config` | YAML config, `${ENV}` expansion, validation |
| `internal/server` | HTTP handlers for model, admin and operational endpoints; middleware |
| `internal/auth` | Key format, hashing, snapshot lookup |
| `internal/router` | Virtual model resolution and fallback |
| `internal/provider/openai`, `internal/provider/anthropic` | Adapters |
| `internal/limits` | Rate limits and budgets; memory and Valkey backends |
| `internal/store` | Store interface; SQLite and Postgres; migrations |
| `internal/usage` | Usage records, cost calculation, async writer |
| `internal/telemetry` | Metrics and tracing setup |
| `internal/mcpgw` | MCP gateway: upstream sessions, catalogs, virtual servers, pinning |
| `internal/oauth` | JWT access token validation and protected resource metadata |

Dependencies point inward: `server` uses everything; adapters, store and limits know nothing about HTTP handlers.

## Request lifecycle

1. Read and size-check the body; assign a request id.
2. Look up the key hash in the snapshot; load the org, team and key policies.
3. Check the model allow lists at all three levels.
4. Check RPM and remaining budget at all three levels (and TPM against tokens used so far this minute).
5. Route to the first target; on a retryable failure, try the next.
6. Stream or copy the response to the client while extracting usage.
7. After the response: add tokens to TPM counters, add cost to budget counters, queue the usage record, record metrics and finish the trace.

Budgets are checked before the call and charged after it, so one in-flight request can overshoot a budget by at most its own cost. This is the usual trade-off in gateways and is documented for users.
