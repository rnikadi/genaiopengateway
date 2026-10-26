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
| `POST /v1/messages` | Anthropic Messages passthrough; served by `anthropic` providers and by Claude models on `vertex` and `bedrock` | v0.1 |
| `POST /v1beta/models/{model}:generateContent`, `:streamGenerateContent` | Google's generateContent, for the Google GenAI SDK and Gemini CLI; served by `gemini` and `vertex` providers. `{model}` is a virtual model, the key may come in `x-goog-api-key`, streams are always server-sent events, and errors use Google's shape ([cloud-providers.md](cloud-providers.md)) | v0.5 |
| `POST /v1/images/generations`, `POST /v1/images/edits` | OpenAI images; edits take multipart; `stream` gives server-sent events. Served by `openai` providers ([modalities.md](modalities.md)) | v0.8 |
| `POST /v1/audio/speech` | OpenAI text to speech; returns audio, or events with `stream_format: sse` | v0.8 |
| `POST /v1/audio/transcriptions`, `POST /v1/audio/translations` | OpenAI speech to text, multipart. Streamed transcripts are refused while `output` guardrails apply to the key | v0.8 |
| `POST /v1/moderations` | OpenAI moderations | v0.8 |
| `POST /v1/rerank` | Rerank with `query` and `documents`, the shape of Cohere, Jina and vLLM | v0.8 |
| `POST /v1/videos`, `POST /v1/videos/{id}/remix` | OpenAI video jobs, JSON or multipart with `input_reference`; charged by the job's `seconds` when it is created | v0.8 |
| `GET /v1/videos`, `GET`/`DELETE /v1/videos/{id}`, `GET /v1/videos/{id}/content` | The caller's video jobs, at the target that made them. Listing returns each job as last retrieved. Other teams get `404`; these calls are not charged and do not count toward limits | v0.8 |
| `POST /v1/files` (`purpose=batch`), `GET /v1/files`, `GET`/`DELETE /v1/files/{id}`, `GET /v1/files/{id}/content` | Batch input files ([batch.md](batch.md)). An upload is checked line by line, routed by the model its lines name, rewritten to the target's model and kept on that target. Output and error files of the caller's batches download here too. Other teams get `404` | v0.8 |
| `POST /v1/batches`, `GET /v1/batches`, `GET /v1/batches/{id}`, `POST /v1/batches/{id}/cancel` | OpenAI's Batch API on the input file's target. A batch is charged once, from the usage it reports when it finishes, less the target's `batch_discount` | v0.8 |
| `GET /v1/realtime?model=` | OpenAI realtime over WebSocket ([modalities.md](modalities.md)). The key comes in `Authorization` or, from browsers, the `openai-insecure-api-key.<key>` subprotocol. Each `response.done` is charged; a spent budget or `limits.max_realtime_session` ends the session with an `error` event. Refused for keys with guardrails | v0.8 |

### Authentication

Send a gateway key as `Authorization: Bearer <key>`. The `x-api-key: <key>` header is also accepted so the Anthropic SDK works unchanged. Upstream provider keys are never accepted from clients and never returned.

### The `model` field

`model` names a **virtual model** from the gateway config, not an upstream model. The gateway rewrites it to the upstream model of whichever target serves the request. Response bodies are not rewritten, so `model` in a response is whatever the upstream reports; the `x-gateway-target` header names the provider and upstream model.

For streaming chat completions the gateway sets `stream_options.include_usage` to `true` unless the client set it, so usage can be metered. The gateway reads usage from the extra final chunk and removes it from the stream, so clients see exactly what they asked for. Clients that set `include_usage` themselves receive the chunk as usual.

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
| `x-gog-cache` | `hit`, `semantic` or `miss` when the caller's policy turns on the response cache ([cache.md](cache.md)). Send `Cache-Control: no-cache` to skip the lookup, or `no-store` to skip the cache entirely |
| `x-gog-guardrails` | Guardrails that acted on the request, as `name=action` pairs, for example `no-pii=redact` ([guardrails.md](guardrails.md)) |
| `x-ratelimit-limit-tokens`, `x-ratelimit-remaining-tokens`, `x-ratelimit-reset-tokens` | Same for tokens per minute |

### Errors

OpenAI-style endpoints return `{"error": {"message": "...", "type": "<code>", "code": "<code>"}}`. `/v1/messages` returns Anthropic's `{"type": "error", "error": {"type": "<anthropic type>", "message": "..."}}`. Upstream errors are passed through unchanged when they come from the last target tried.

| Status | Code | When |
| --- | --- | --- |
| 400 | `invalid_request` | Body is not valid JSON, `model` missing, or a field the target adapter cannot translate |
| 400 | `guardrail_blocked` | A guardrail blocked the request or the model's response; the message names the guardrail |
| 401 | `invalid_api_key` | Key missing, unknown, revoked or expired |
| 403 | `model_not_allowed` | Key, team or organization may not use this model |
| 404 | `model_not_found` | Virtual model does not exist, or does not support this endpoint |
| 404 | `not_found` | The video job, file or batch does not exist or belongs to another team |
| 413 | `request_too_large` | Body exceeds `limits.max_request_bytes`, or a batch file `limits.max_batch_file_bytes` |
| 429 | `rate_limit_exceeded` | A requests or tokens per minute limit was hit; `retry-after` is set |
| 429 | `budget_exceeded` | A budget for the current period is spent |
| 501 | `database_required` | Video jobs, files and batches need `store` configured |
| 502 | `all_targets_failed` | No target produced a response |
| 503 | `limiter_unavailable` | Budget state could not be checked and budgets fail closed |

## Admin API

Base path `/admin/v1`. JSON in and out. Authenticate with `Authorization: Bearer <admin token>`, where the token is set in config (`admin.token`). The admin API is disabled when no token is set or no store is configured. From v0.3 the admin API also accepts a session cookie from the admin UI, with roles; see [admin-ui.md](admin-ui.md#admin-api-changes).

### Resources

| Method and path | Purpose |
| --- | --- |
| `POST /admin/v1/orgs` | Create organization. Names are trimmed, 1 to 200 characters, and unique without case (`409` when taken) |
| `GET /admin/v1/orgs` | List organizations |
| `GET`, `PATCH`, `DELETE /admin/v1/orgs/{org_id}` | Read, update, delete (only when it has no teams). Only owners update an organization (ADR 0030) |
| `POST /admin/v1/orgs/{org_id}/teams` | Create team |
| `GET /admin/v1/orgs/{org_id}/teams` | List teams |
| `GET`, `PATCH`, `DELETE /admin/v1/teams/{team_id}` | Read, update, delete (only when it has no active keys) |
| `GET /admin/v1/teams/{team_id}/credentials` | Each provider with whether the team has its own credential for it (`set`, `supported`, `updated_at`, `updated_by`); never the secret ([team-credentials.md](team-credentials.md)) |
| `PUT`, `DELETE /admin/v1/teams/{team_id}/credentials/{provider}` | Set the team's own API key for a provider with `{"api_key": "..."}`, or remove it; needs `secrets.encryption_key`; audited as `team.credential.set` and `team.credential.delete` |
| `POST /admin/v1/teams/{team_id}/keys` | Create key; the secret is returned once, in this response only |
| `GET /admin/v1/teams/{team_id}/keys` | List keys (no secrets) |
| `GET /admin/v1/keys` | List keys across teams, filtered by `org_id`, `team_id`, `status` (`active`, `expiring`, `expired`, `revoked`, `rotating`) and `q` (name, prefix or owner); limited to the organizations the operator can read ([virtual-keys.md](virtual-keys.md)) |
| `GET`, `PATCH /admin/v1/keys/{key_id}` | Read, update limits, name or `owner` |
| `POST /admin/v1/keys/{key_id}/revoke` | Revoke; takes effect on every replica within the refresh interval |
| `POST /admin/v1/keys/{key_id}/rotate` | New secret, returned once; the previous one works for `grace` (default `24h`, `0` to `30d`). Audited as `key.rotate` |
| `GET /admin/v1/usage` | Aggregated usage and cost |
| `GET /admin/v1/audit` | Admin audit log |
| `GET /admin/v1/alerts` | Alerts raised, newest first; filter with `org_id` and `event` ([alerts.md](alerts.md)) |
| `POST /admin/v1/alerts/test` | Send a test alert to a configured webhook: `{"webhook": "name"}`. Returns `{"ok", "status"}` (owners only) |
| `GET /admin/v1/mcp/connections`, `DELETE /admin/v1/mcp/connections/{server}/{subject}` | Users' connections to per-user MCP servers; list and revoke (owners only, see [mcp.md](mcp.md#api-additions)) |
| `GET /admin/v1/requests` | Model requests from usage records, newest first, with `has_body`; filter with `org_id`, `team_id`, `key_id`, `from`, `to`. `after` is a `request_id` ([body-logging.md](body-logging.md)) |
| `GET /admin/v1/requests/{request_id}` | A request's logged body, decrypted: `request`, `response`, `truncated`, `redactions`. Owners and the organization's admins only; each read is audited as `request_body.read` |
| `GET /admin/v1/guardrail-events` | Guardrails that matched or failed, newest first; filter with `org_id`, `team_id`, `key_id`, `guardrail`, `action`, `request_id`, `from`, `to`. Never the matched text ([guardrails.md](guardrails.md)) |
| `DELETE /admin/v1/sessions?subject=` | Sign out every admin UI session but the caller's, or one user's. Returns `{"deleted": n}` (owners only, audited as `operator.sessions.delete`) |
| `DELETE /admin/v1/requests?org_id=&before=` | Delete an organization's bodies logged before a time, for erasure requests. Returns `{"deleted": n}` (owners only, audited) |
| `GET /admin/v1/batches` | Batches newest first, with owner, model, target, `status`, the last batch `object`, `cost_usd` and `charged_at`; filter with `org_id`, `team_id`, `status` ([batch.md](batch.md)) |
| `POST /admin/v1/batches/{id}/cancel` | Cancel a batch at its target. Owners and the organization's admins; audited as `batch.cancel` |
| `DELETE /admin/v1/cache?org_id=&team_id=` | Remove an organization's cached answers, or one team's, and report how many as `cleared`. Owners and the organization's admins; audited as `cache.clear` ([cache.md](cache.md)) |
| `GET /admin/v1/videos` | Video jobs newest first, with owner, model, target and the last job `object`; filter with `org_id`, `team_id` |
| `GET /admin/v1/models` | Virtual models with the endpoints they serve; each target with `provider_type`, `endpoints`, price and last-hour `health` |
| `GET /admin/v1/providers` | Providers with `type`, `base_url`, `region`, `project`, `location`, `auth`, `timeout`, `header_names`, the `models` using them and last-hour `health`. Never credentials |
| `GET /admin/v1/guardrails` | Guardrail definitions: `type`, `action`, `stages`, `detect`, `patterns`, `model`, `unsafe_if`, `webhook_host`, `timeout`, `on_error`, and the `virtual_servers` that attach them |
| `GET /admin/v1/alerts/webhooks` | Alert webhooks: `name`, `format`, `events`, `org`, `host` (no path), `signed`; and `budget_thresholds` (owners only) |
| `POST /admin/v1/playground/chat/completions?team_id=` | Call a model as a team with an OpenAI chat completions body; the team's organization and team policies apply, usage is recorded under the team, and each call is audited as `playground.chat` (owners and the organization's admins, [playground.md](playground.md)) |
| `GET /admin/v1/config` | Every configuration object with its source (`file`, `store`, `changed` or `deleted`), secrets as `"set"` (owners only, [admin-ui-config-editing.md](admin-ui-config-editing.md)) |
| `PUT /admin/v1/config/{kind}/{name}` | Create or replace a stored provider, model, guardrail, MCP server, virtual MCP server or alert webhook, or the gateway settings (`settings/gateway`, [admin-ui-settings.md](admin-ui-settings.md)); the body is its spec as in `config.yaml`. A name from the config file is replaced by the stored version ([admin-ui-config-editing.md](admin-ui-config-editing.md)). `400` when the merged config would be invalid (owners only, audited) |
| `DELETE /admin/v1/config/{kind}/{name}` | Delete a stored object, or mark one from the config file deleted; refused while something refers to it (owners only, audited) |
| `POST /admin/v1/config/{kind}/{name}/reset` | Remove the UI change to an object from the config file, so the file's version applies again (owners only, audited) |
| `GET /admin/v1/settings` | The gateway's own settings; secrets only as set or not set. `fields` lists each runtime setting with `path`, `value`, `default` and `source` (`default`, `file` or `stored`); `stored` is the stored settings spec, `startup` the settings read only at startup (owners only, [admin-ui-settings.md](admin-ui-settings.md)) |

Organizations, teams and keys share the same policy fields, all optional:

```json
{
  "name": "search-team",
  "allowed_models": ["chat-default", "embed-default"],
  "allowed_mcp_servers": ["engineering"],
  "rpm": 600,
  "tpm": 200000,
  "budget": {"amount_usd": 500, "period": "monthly", "alert_at": [0.8, 1]},
  "guardrails": ["no-pii"],
  "log_bodies": true,
  "cache": {"mode": "exact", "ttl": "1h"}
}
```

`cache` turns on the response cache for that scope: `mode` is `off`, `exact` or `semantic` (which needs `cache.semantic.model` in config), and `ttl` is how long an answer is reused, at most 720h. The most specific level that sets it wins, and unset everywhere means off ([cache.md](cache.md)).

`log_bodies` (true or false) turns body logging on or off for that scope; the most specific level that sets it wins, and unset everywhere means off ([body-logging.md](body-logging.md)).

`guardrails` names guardrails from config. Unlike the other fields, the lists of the organization, team and key add up: a request runs every guardrail named at any level. Unknown names are rejected ([guardrails.md](guardrails.md)).

`alert_at` is optional: it overrides the gateway's alert thresholds for this budget, and `[]` turns its alerts off.

`allowed_models` and `allowed_mcp_servers` empty or absent mean all models and all MCP virtual servers. `period` is `daily`, `monthly` (calendar month, UTC) or `total`. Keys also accept `expires_at` (RFC 3339).

Key create response:

```json
{"id": "key_01J...", "name": "ci", "prefix": "sk-gog-AbCd", "key": "sk-gog-AbCd...", "team_id": "team_01J...", "created_at": "2026-10-22T21:00:00Z"}
```

### Usage query

`GET /admin/v1/usage?org_id=&team_id=&key_id=&from=&to=&group_by=` where `group_by` is one of `model`, `team`, `key`, `day`, `provider`, `endpoint`. `from` and `to` are RFC 3339 and default to the current month. The response lists rows with `requests`, `input_tokens`, `output_tokens`, `cached_input_tokens`, `reasoning_tokens`, `cost_usd`, and `cache_hits` and `saved_usd` for answers served from the response cache.

`health` covers the last hour across all replicas, from usage records: `requests`, `errors` (status 400 or above), `error_rate`, `avg_latency_ms` and `last_error_at`. Only the target that answered a request is counted; failovers show in `gog_fallbacks_total`.

### Lists and pagination

List endpoints accept `limit` (default 50, max 500) and `after` (an id from the previous page) and return `{"data": [...], "has_more": true}`.

### Admin errors

`{"error": {"message": "...", "code": "..."}}` with codes `unauthorized` (401), `forbidden` (403), `not_found` (404), `conflict` (409), `invalid_request` (400).

## MCP

Virtual MCP servers are served at `POST /mcp/{name}` (Streamable HTTP), with admin endpoints for upstream status, tool pins and call audit under `/admin/v1/mcp`. See [mcp.md](mcp.md#api-additions).

## Operational endpoints

| Path | Purpose |
| --- | --- |
| `GET /healthz` | Liveness; 200 while the process is running |
| `GET /readyz` | Readiness; 200 once config is loaded and, if a store is configured, the first snapshot is loaded |
| `GET /metrics` | Prometheus metrics; served on `metrics.listen` if set, otherwise on the main listener |
