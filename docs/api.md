# API specification

The gateway exposes three surfaces: the **model API** for applications, the **admin API** for operators, and **operational endpoints** for health and metrics.

## Model API

Request and response bodies follow the [OpenAI API reference](https://platform.openai.com/docs/api-reference) for OpenAI-style endpoints and the [Anthropic Messages API](https://docs.anthropic.com/en/api/messages) for `/v1/messages`. This document only specifies what the gateway adds or changes.

| Method and path | Contract | Since |
| --- | --- | --- |
| `GET /v1/models` | OpenAI list models; returns the virtual models the key may use | v0.1 |
| `POST /v1/chat/completions` | OpenAI chat completions, streaming and non-streaming | v0.1 |
| `POST /v1/embeddings` | OpenAI embeddings | v0.1 |
| `POST /v1/responses` | OpenAI Responses; served only by `openai` providers | v0.1 |
| `POST /v1/messages` | Anthropic Messages passthrough; served only by `anthropic` providers | v0.1 |
| `/v1/audio/*`, `/v1/images/*`, `/v1/realtime`, `/v1/videos` | OpenAI contracts | v0.8 |

### Authentication

Send a gateway key as `Authorization: Bearer <key>`. The `x-api-key: <key>` header is also accepted so the Anthropic SDK works unchanged. Upstream provider keys are never accepted from clients and never returned.

### The `model` field

`model` names a **virtual model** from the gateway config, not an upstream model. The gateway rewrites it to the upstream model of whichever target serves the request. Response bodies are not rewritten, so `model` in a response is whatever the upstream reports; the `x-gateway-target` header names the provider and upstream model.

For streaming chat completions the gateway sets `stream_options.include_usage` to `true` unless the client set it, so usage can be metered. Clients then receive one extra final chunk with an empty `choices` array and a `usage` object, which the OpenAI SDKs handle.

### Routing and fallback

Targets of a virtual model are tried in order. The gateway moves to the next target when the current one:

- fails to connect or times out before sending response headers, or
- returns 429 or any 5xx except 501.

It never falls back on other 4xx responses, which indicate a problem with the request. For streaming requests, fallback is only possible before the first byte is sent to the client. When every target fails, the client receives the last upstream error if there was one, otherwise `502 all_targets_failed`.

### Response headers

| Header | Meaning |
| --- | --- |
| `x-request-id` | Gateway request id; echoes the client's `x-request-id` if it is a safe token of up to 128 characters |
| `x-gateway-target` | `<provider>/<upstream model>` that served the request |
| `x-ratelimit-limit-requests`, `x-ratelimit-remaining-requests`, `x-ratelimit-reset-requests` | Tightest request limit that applies to the key, as in the OpenAI API |
| `x-ratelimit-limit-tokens`, `x-ratelimit-remaining-tokens`, `x-ratelimit-reset-tokens` | Same for tokens per minute |

### Errors

OpenAI-style endpoints return `{"error": {"message": "...", "type": "<code>", "code": "<code>"}}`. `/v1/messages` returns Anthropic's `{"type": "error", "error": {"type": "<anthropic type>", "message": "..."}}`. Upstream errors are passed through unchanged when they come from the last target tried.

| Status | Code | When |
| --- | --- | --- |
| 400 | `invalid_request` | Body is not valid JSON, `model` missing, or a field the target adapter cannot translate |
| 401 | `invalid_api_key` | Key missing, unknown, revoked or expired |
| 403 | `model_not_allowed` | Key, team or organization may not use this model |
| 404 | `model_not_found` | Virtual model does not exist, or does not support this endpoint |
| 413 | `request_too_large` | Body exceeds `limits.max_request_bytes` |
| 429 | `rate_limit_exceeded` | A requests or tokens per minute limit was hit; `retry-after` is set |
| 429 | `budget_exceeded` | A budget for the current period is spent |
| 502 | `all_targets_failed` | No target produced a response |
| 503 | `limiter_unavailable` | Budget state could not be checked and budgets fail closed |

## Admin API

Base path `/admin/v1`. JSON in and out. Authenticate with `Authorization: Bearer <admin token>`, where the token is set in config (`admin.token`). The admin API is disabled when no token is set or no store is configured. SSO and per-user roles replace the single token in v0.5.

### Resources

| Method and path | Purpose |
| --- | --- |
| `POST /admin/v1/orgs` | Create organization |
| `GET /admin/v1/orgs` | List organizations |
| `GET`, `PATCH`, `DELETE /admin/v1/orgs/{org_id}` | Read, update, delete (only when it has no teams) |
| `POST /admin/v1/orgs/{org_id}/teams` | Create team |
| `GET /admin/v1/orgs/{org_id}/teams` | List teams |
| `GET`, `PATCH`, `DELETE /admin/v1/teams/{team_id}` | Read, update, delete (only when it has no active keys) |
| `POST /admin/v1/teams/{team_id}/keys` | Create key; the secret is returned once, in this response only |
| `GET /admin/v1/teams/{team_id}/keys` | List keys (no secrets) |
| `GET`, `PATCH /admin/v1/keys/{key_id}` | Read, update limits or name |
| `POST /admin/v1/keys/{key_id}/revoke` | Revoke; takes effect on every replica within the refresh interval |
| `GET /admin/v1/usage` | Aggregated usage and cost |
| `GET /admin/v1/audit` | Admin audit log |

Organizations, teams and keys share the same policy fields, all optional:

```json
{
  "name": "search-team",
  "allowed_models": ["chat-default", "embed-default"],
  "rpm": 600,
  "tpm": 200000,
  "budget": {"amount_usd": 500, "period": "monthly"}
}
```

`allowed_models` empty or absent means all models. `period` is `daily`, `monthly` (calendar month, UTC) or `total`. Keys also accept `expires_at` (RFC 3339).

Key create response:

```json
{"id": "key_01J...", "name": "ci", "prefix": "sk-gog-AbCd", "key": "sk-gog-AbCd...", "team_id": "team_01J...", "created_at": "2026-10-22T21:00:00Z"}
```

### Usage query

`GET /admin/v1/usage?org_id=&team_id=&key_id=&from=&to=&group_by=` where `group_by` is one of `model`, `team`, `key`, `day`. `from` and `to` are RFC 3339 and default to the current month. The response lists rows with `requests`, `input_tokens`, `output_tokens`, `cached_input_tokens`, `reasoning_tokens` and `cost_usd`.

### Lists and pagination

List endpoints accept `limit` (default 50, max 500) and `after` (an id from the previous page) and return `{"data": [...], "has_more": true}`.

### Admin errors

`{"error": {"message": "...", "code": "..."}}` with codes `unauthorized` (401), `not_found` (404), `conflict` (409), `invalid_request` (400).

## Operational endpoints

| Path | Purpose |
| --- | --- |
| `GET /healthz` | Liveness; 200 while the process is running |
| `GET /readyz` | Readiness; 200 once config is loaded and, if a store is configured, the first snapshot is loaded |
| `GET /metrics` | Prometheus metrics; served on `metrics.listen` if set, otherwise on the main listener |
