# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow [semantic versioning](docs/releasing.md).

## [Unreleased]

### Security

- Sign-in roles resolve safely (ADR 0029), from the October 2026 pentest. Organization names are unique without case (`409` when taken) and only owners rename an organization, so an SSO role mapping's `org` can no longer resolve to another organization of the same name, or to one an organization admin renamed. Migration 0015 appends the id to existing duplicate names, keeping the oldest. Role mappings by `email` are skipped when the ID token has `email_verified: false`. Names of organizations, teams and keys are trimmed, at most 200 characters and without control characters.
- Admin UI roles are resolved on every request against the current `sso.roles`, from the email and groups stored with the session (migration 0016), so removing a mapping takes effect at once instead of up to `session_ttl` later. Owners can sign everyone else out from the Settings page or `DELETE /admin/v1/sessions` (`?subject=` for one user), audited as `operator.sessions.delete`.

### Added

- Team provider credentials ([docs/team-credentials.md](docs/team-credentials.md)): a team can store its own API key for an OpenAI-compatible, Anthropic, Gemini or Bedrock provider, used for its requests, realtime sessions and batches instead of the shared key, so they are billed to the team's own account while applications keep their virtual keys. The shared key of that provider is never used as a fallback for the team. Set and removed on the team page or with `PUT`/`DELETE /admin/v1/teams/{id}/credentials/{provider}`; encrypted with `secrets.encryption_key` and audited.
- Objects from the config file can be edited, deleted and reset in the admin UI ([docs/admin-ui-config-editing.md](docs/admin-ui-config-editing.md), ADR 0028). A change is stored in the database and wins over the file; a delete stays deleted after a restart; Reset (`POST /admin/v1/config/{kind}/{name}/reset`) brings the file's version back. Secrets left as set keep the file's value by reference, so keys from `${VAR}` are never copied into the store. The Configuration page labels each object From config file, Changed here, Deleted here or Stored, and writes to file names no longer return `409`.
- Virtual keys ([docs/virtual-keys.md](docs/virtual-keys.md)): a Virtual keys page lists every key across organizations and teams, with search by name, prefix or owner and filters by organization and status, and creates, edits, rotates and revokes keys; each key's Access view shows its organization, team and key policies side by side. `POST /admin/v1/keys/{id}/rotate` issues a new secret while the previous one keeps working for a grace period (default 24 hours, up to 30 days), so applications switch over without downtime. Keys have an optional `owner`, shown in `key.expiring` alerts. New `GET /admin/v1/keys`.
- Every runtime setting is changed from the admin UI ([docs/admin-ui-settings.md](docs/admin-ui-settings.md), ADR 0027). The Settings page shows each setting's value, its default and where it comes from (default, config file or set here), with Reset; changes are stored as `settings/gateway`, win over the config file, apply on every replica without a restart and are audited with the fields that changed. Only startup settings (listen addresses, TLS, store, limiter connection, encryption keys, admin token, `ui.enabled`, static keys) stay in the environment and config file, shown read-only. Guardrails, MCP servers, virtual MCP servers and alert webhooks get forms, and a test fails when a new setting has no control in the UI.
- Guardrails ([docs/guardrails.md](docs/guardrails.md)): checks on the text of model requests (`input`), model responses (`output`), MCP tool arguments (`tool_input`) and MCP tool results (`tool_output`). Four types: `pii` (the body logging detectors), `regex` (RE2), `model` (a classifier such as Llama Guard, called through the gateway's own routing and metered under the requesting key) and `webhook` (a signed call to an operator's service, which answers allow, block or redact). Each `block`s with `400 guardrail_blocked`, `redact`s, or only `log`s; external checks have a timeout and `on_error`. Guardrails are attached by a new `guardrails` policy field, whose lists add up across organization, team and key, and by virtual MCP servers. Streamed responses are buffered when an `output` guardrail applies and re-sent as a valid stream when redacted. Built-in checks add about 2.5 ms at p50 on a 32 KiB request, and a 10 ms gate runs in CI.
- Guardrail decisions are recorded without the matched text: the `X-Gog-Guardrails` response header, a `guardrail_events` table pruned with `usage_retention`, `GET /admin/v1/guardrail-events`, metrics `gog_guardrail_checks_total` and `gog_guardrail_duration_seconds`, and a Guardrails page in the admin UI. The policy form has a guardrails field.

- Gemini and Vertex AI providers ([docs/cloud-providers.md](docs/cloud-providers.md)). `gemini` calls the Gemini API with an API key; `vertex` calls Vertex AI with Application Default Credentials or a `credentials_file`, picking the global, multi-region or regional endpoint from `location`. Chat completions are translated natively to generateContent, with streaming, tools (thought signatures kept in `extra_content`), data URL images, JSON schemas, `reasoning_effort` and usage that counts thinking tokens as output. Claude models on Vertex AI (`anthropic/claude-…`) reuse the Anthropic adapter for chat completions and `/v1/messages`.
- Amazon Bedrock provider (`bedrock`): chat completions for every Bedrock model through the Converse API, with streaming, tools and images; `/v1/messages` for Claude through InvokeModel, streamed as Anthropic events; and embeddings for Titan Text Embeddings V2 and Cohere Embed. Requests use a Bedrock API key, or SigV4 with the AWS default credential chain (environment, shared config and SSO, EKS web identity, ECS and EC2 roles). Throttling and quota errors become `429` and capacity errors `5xx`, so fallback works.
- Helm chart ([docs/deploy-kubernetes.md](docs/deploy-kubernetes.md)), published with each release to `oci://ghcr.io/rnikadi/charts/genaiopengateway` and signed with cosign. It runs 2 replicas by default as non-root with a read-only root filesystem, no capabilities and no service account token, spreads them across nodes and zones, and adds a PodDisruptionBudget, with optional Ingress, HorizontalPodAutoscaler, ServiceMonitor and NetworkPolicy. It refuses to render a config that would run several replicas on SQLite or the in-memory limiter. Postgres, Valkey and secrets stay outside the chart.
- `POST /v1beta/models/{model}:generateContent` and `:streamGenerateContent`, so the Google GenAI SDK and Gemini CLI can use the gateway; keys are also accepted in `x-goog-api-key`.
- The admin UI shows everything the gateway is configured with ([docs/admin-ui-config.md](docs/admin-ui-config.md)): a Providers page (type, endpoint, auth, models, last-hour health), endpoints and per-target health on the Models page, guardrail definitions, an Alerts page with webhooks and a test button, and a Settings page for owners. New read-only admin endpoints `providers`, `guardrails`, `alerts/webhooks` and `settings` never return a secret; usage can be grouped by provider and endpoint.
- A playground in the admin UI ([docs/playground.md](docs/playground.md)) calls a model as a team, with streaming: the team's allowed models, limits, budget and guardrails apply, usage is metered under the team and each call is audited. The Requests page can delete an organization's logged bodies before a date, and the Overview shows providers with errors in the last hour.
- Configuration objects stored through the admin API ([docs/admin-ui-config-editing.md](docs/admin-ui-config-editing.md), ADR 0023): owners add and change providers, models, guardrails, MCP servers, virtual MCP servers and alert webhooks from a Configuration page or `PUT /admin/v1/config/{kind}/{name}`. They are merged into the config file, which wins on a name clash, so they get the same defaults and validation; a write that would break the config is refused. Secrets are encrypted with `secrets.encryption_key` and never returned. Every replica applies a change within `store.refresh_interval`. Writes are audited.

- Playground tabs for images, text to speech and transcription ([docs/playground.md](docs/playground.md)), on the same team-scoped, metered and audited path as chat. Cookie requests may send multipart from the gateway's own origin, for audio uploads, and the UI's content security policy allows generated images and audio.
- Native images and speech on cloud providers ([docs/cloud-providers.md](docs/cloud-providers.md)): Imagen on `vertex` and `gemini`, Nova Canvas and Titan Image on `bedrock` for `/v1/images/generations`, and Gemini TTS models on `vertex` and `gemini` for `/v1/audio/speech` (WAV or PCM), chosen by model id and answered in OpenAI's shape.
- Images, text to speech, speech to text, moderation and rerank ([docs/modalities.md](docs/modalities.md), ADR 0024): `POST /v1/images/generations`, `/v1/images/edits`, `/v1/audio/speech`, `/v1/audio/transcriptions`, `/v1/audio/translations`, `/v1/moderations` and `/v1/rerank` on `openai` providers, with keys, policies, fallback, limits, budgets and body logging as for chat. Multipart uploads are supported. Usage records count images, speech characters, audio seconds and video seconds, and prices gain `per_image`, `per_mchars`, `per_audio_minute`, `per_video_second` and `per_request`; the Usage page, CSV export and model form show them. Guardrails check prompts, speech input, rerank queries and transcripts.
- Video jobs ([docs/modalities.md](docs/modalities.md)): `POST /v1/videos`, remix, retrieve, content, delete and list, on `openai` providers. The gateway records each job (migration 0010) so later calls reach the target that made it and only the owning team; other teams get `404`. A job is charged by its `seconds` with `per_video_second` when it is created. Video jobs need a database.
- Batch and files ([docs/batch.md](docs/batch.md), ADR 0025): `/v1/files` for batch input and `/v1/batches` on `openai` providers. Uploads are checked line by line (one model and endpoint, the caller's allowed models, input guardrails with redaction), routed by their model with fallback, and rewritten to the target's model. Files and batches are recorded (migration 0011) so only the owning team reaches them and their output. A batch is charged once when it finishes, from the usage it reports, less the new `batch_discount` price field; a background refresh charges batches no client polls. `limits.max_batch_file_bytes` bounds uploads (100 MiB).
- A Jobs page in the admin UI lists batches and video jobs with status, request counts and cost, and lets owners and organization admins cancel a batch (audited as `batch.cancel`). `GET /admin/v1/batches`, `POST /admin/v1/batches/{id}/cancel` and `GET /admin/v1/videos` back it.
- Realtime sessions ([docs/modalities.md](docs/modalities.md)): `GET /v1/realtime?model=` relays OpenAI's realtime WebSocket to `openai` providers, with fallback while dialing, the key in `Authorization` or the browser subprotocol, and the virtual model kept in `session.update`. Each `response.done` is charged and recorded, with new `audio_input_per_mtok` and `audio_output_per_mtok` prices; a spent budget or `limits.max_realtime_session` (default 30m) ends the session. Keys with guardrails are refused realtime.
- Response cache ([docs/cache.md](docs/cache.md), ADR 0026): a `cache` policy field (`off`, `exact` or `semantic`, with a `ttl`) on organizations, teams and keys answers repeated chat completions, Messages and embeddings requests from the cache, streamed or not, without calling the provider. Exact matches key on the team, endpoint, model, guardrails and the request body; semantic matches also accept a reworded last user message, compared with `cache.semantic.model` embeddings. Answers live in Valkey with the `valkey` limiter, shared by replicas, or in memory. Hits answer `X-Gog-Cache: hit` and are recorded with no tokens or cost, with what they saved (migration 0012); `Cache-Control: no-cache` and `no-store` skip it. The policy form has a cache setting, the Usage page shows hits and savings, the Requests page marks cached answers, the Settings page shows the cache configuration, and organization and team pages clear it through `DELETE /admin/v1/cache` (audited as `cache.clear`). Metrics `gog_cache_lookups_total` and `gog_cache_saved_usd_total`.

### Changed

- On Postgres, migrations run under an advisory lock, so replicas that start together no longer race on the same migration.
- On shutdown, `/readyz` fails before the listener closes, so load balancers stop sending requests to a stopping gateway. The shutdown grace period is 50 seconds.
- When an output guardrail applies to a stream the gateway cannot read, the response is blocked instead of passed on unchecked.
- The redaction detectors run their regular expressions only around the characters a match must contain, which makes body logging faster on long text.

## [0.3.0] - 2026-10-23

Running it in production: an admin UI with single sign-on, alerts, per-user MCP credentials and audited body logging.

### Added

- Admin UI at `/ui/`, embedded in the binary: an overview of spend, organizations, teams and keys with their policies (a new key is shown once), usage by model, team, key or day with CSV download, MCP servers, tool review and approval and call records, models, and the audit log.
- Operator sign-in with OIDC (authorization code with PKCE) and roles mapped from identity provider groups or emails: `owner`, `admin` of one organization, and `viewer` of one or every organization. Roles apply to the admin API too. Sessions are stored server side; cookie requests that change something must be same-origin JSON. The admin token still works and acts as owner.
- Alerts ([docs/alerts.md](docs/alerts.md)): when spend reaches a share of a budget (50%, 80% and 100% by default, or per budget with `alert_at`), when a pinned MCP tool awaits approval, and a week before a key expires. Each alert fires once across replicas, is listed at `GET /admin/v1/alerts` and in the UI overview, and is sent to webhooks from config as signed JSON or Slack messages, with retries. `POST /admin/v1/alerts/test` checks a webhook.
- Per-user upstream MCP credentials ([docs/mcp-user-credentials.md](docs/mcp-user-credentials.md)): a server with `user_auth` is called with each user's own OAuth token. A user who has not connected gets a connect link (URL elicitation, or a tool error for older clients), signs in to the upstream with PKCE, and the gateway stores their tokens encrypted, refreshes them, and keeps one upstream session per user. With SSO on the same identity provider, only the user who asked can complete the link. Owners list and revoke connections in the admin API and the UI.
- Request body logging ([docs/body-logging.md](docs/body-logging.md)), off by default: a `log_bodies` policy on an organization, team or key stores the request and response of model calls (streams assembled into the final result) and MCP calls. Bodies are redacted before storage (emails, phone numbers, card numbers with Luhn, IBANs with mod-97, US SSNs, well-known secrets, custom patterns), truncated to `max_body_bytes`, encrypted with AES-256-GCM under `secrets.encryption_key`, and pruned after `retention`. Owners and organization admins read them at `GET /admin/v1/requests/{id}` or on the UI's Requests page, and each read is audited.
- Admin API: `GET /admin/v1/me`, `/admin/v1/models` and `/admin/v1/mcp/virtual-servers`.
- `compose.sso.yaml` adds Dex to the Compose setup, with owner, organization admin and viewer users, and a browser test of the UI runs against it in CI.
- A Docker Compose setup for local use in `deploy/compose`: the gateway with Postgres, Valkey and the MCP reference server, plus optional profiles for a local model through Ollama and for Jaeger and Prometheus.

## [0.2.0] - 2026-10-23

The MCP gateway: one governed endpoint for the MCP servers a company uses.

### Added

- MCP gateway: virtual MCP servers at `/mcp/{name}` that expose chosen tools, prompts and resources from upstream MCP servers over Streamable HTTP, with per-upstream name prefixes, tool allow lists, static upstream credentials, `allowed_mcp_servers` policies, rate limits on calls, and MCP metrics, logs and traces. Clients on MCP 2024-11-05 through 2026-07-28 are supported.
- MCP tool pinning against silent definition changes (`pin_tools: true | strict | false`), an audit record per MCP call, and admin endpoints for upstream status, tool approval and call records.
- OAuth 2.1 on MCP endpoints: the gateway validates JWT access tokens from the company's identity provider (JWKS discovery, audience per virtual server, required scopes), maps them to an organization and team, records the token subject, and publishes RFC 9728 protected resource metadata.

## [0.1.0] - 2026-10-22

The first release: an OpenAI-compatible gateway with tenants, keys, limits, budgets, usage and observability.

### Model API

- `/v1/chat/completions`, `/v1/embeddings`, `/v1/responses` and `/v1/models`, compatible with the OpenAI SDKs, streaming included.
- `/v1/messages` passthrough for the Anthropic SDKs.
- Virtual models with ordered fallback across targets on 429, 5xx and connection errors.
- Providers: OpenAI or any OpenAI-compatible API (such as vLLM or Ollama), and Anthropic, which also serves chat completions by translation, tool calls and streaming included.

### Tenancy and control

- Organizations, teams and keys over an admin REST API, with an audit log of every change.
- Model allow lists, requests and tokens per minute, and daily, monthly or total USD budgets at each level.
- Limits held in memory for one replica or in Valkey across replicas. Rate limits fail open and budgets fail closed by default.
- Keys are stored as SHA-256 hashes and served from memory, so the database is off the request path.
- Static keys in config for setups without a database.

### Usage and observability

- Usage and cost per request in SQLite or Postgres, queryable by org, team, key and model, with a retention period.
- Prometheus metrics, OpenTelemetry traces over OTLP with GenAI semantic conventions, and one JSON log line per request.

### Operations

- One static binary or a distroless container image for linux, darwin and windows on amd64 and arm64.
- Config reload on `SIGHUP` and graceful shutdown.
- Signed binaries and images with SBOMs.

[Unreleased]: https://github.com/rnikadi/genaiopengateway/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/rnikadi/genaiopengateway/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/rnikadi/genaiopengateway/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/rnikadi/genaiopengateway/releases/tag/v0.1.0
