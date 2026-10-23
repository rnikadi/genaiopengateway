# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow [semantic versioning](docs/releasing.md).

## [Unreleased]

### Added

- Guardrails ([docs/guardrails.md](docs/guardrails.md)): checks on the text of model requests (`input`), model responses (`output`), MCP tool arguments (`tool_input`) and MCP tool results (`tool_output`). Four types: `pii` (the body logging detectors), `regex` (RE2), `model` (a classifier such as Llama Guard, called through the gateway's own routing and metered under the requesting key) and `webhook` (a signed call to an operator's service, which answers allow, block or redact). Each `block`s with `400 guardrail_blocked`, `redact`s, or only `log`s; external checks have a timeout and `on_error`. Guardrails are attached by a new `guardrails` policy field, whose lists add up across organization, team and key, and by virtual MCP servers. Streamed responses are buffered when an `output` guardrail applies and re-sent as a valid stream when redacted. Built-in checks add about 2.5 ms at p50 on a 32 KiB request, and a 10 ms gate runs in CI.
- Guardrail decisions are recorded without the matched text: the `X-Gog-Guardrails` response header, a `guardrail_events` table pruned with `usage_retention`, `GET /admin/v1/guardrail-events`, metrics `gog_guardrail_checks_total` and `gog_guardrail_duration_seconds`, and a Guardrails page in the admin UI. The policy form has a guardrails field.

- Gemini and Vertex AI providers ([docs/cloud-providers.md](docs/cloud-providers.md)). `gemini` calls the Gemini API with an API key; `vertex` calls Vertex AI with Application Default Credentials or a `credentials_file`, picking the global, multi-region or regional endpoint from `location`. Chat completions are translated natively to generateContent, with streaming, tools (thought signatures kept in `extra_content`), data URL images, JSON schemas, `reasoning_effort` and usage that counts thinking tokens as output. Claude models on Vertex AI (`anthropic/claude-…`) reuse the Anthropic adapter for chat completions and `/v1/messages`.
- Amazon Bedrock provider (`bedrock`): chat completions for every Bedrock model through the Converse API, with streaming, tools and images; `/v1/messages` for Claude through InvokeModel, streamed as Anthropic events; and embeddings for Titan Text Embeddings V2 and Cohere Embed. Requests use a Bedrock API key, or SigV4 with the AWS default credential chain (environment, shared config and SSO, EKS web identity, ECS and EC2 roles). Throttling and quota errors become `429` and capacity errors `5xx`, so fallback works.
- Helm chart ([docs/deploy-kubernetes.md](docs/deploy-kubernetes.md)), published with each release to `oci://ghcr.io/rnikadi/charts/genaiopengateway` and signed with cosign. It runs 2 replicas by default as non-root with a read-only root filesystem, no capabilities and no service account token, spreads them across nodes and zones, and adds a PodDisruptionBudget, with optional Ingress, HorizontalPodAutoscaler, ServiceMonitor and NetworkPolicy. It refuses to render a config that would run several replicas on SQLite or the in-memory limiter. Postgres, Valkey and secrets stay outside the chart.
- `POST /v1beta/models/{model}:generateContent` and `:streamGenerateContent`, so the Google GenAI SDK and Gemini CLI can use the gateway; keys are also accepted in `x-goog-api-key`.
- The admin UI shows everything the gateway is configured with ([docs/admin-ui-config.md](docs/admin-ui-config.md)): a Providers page (type, endpoint, auth, models, last-hour health), endpoints and per-target health on the Models page, guardrail definitions, an Alerts page with webhooks and a test button, and a Settings page for owners. New read-only admin endpoints `providers`, `guardrails`, `alerts/webhooks` and `settings` never return a secret; usage can be grouped by provider and endpoint.
- A playground in the admin UI ([docs/playground.md](docs/playground.md)) calls a model as a team, with streaming: the team's allowed models, limits, budget and guardrails apply, usage is metered under the team and each call is audited. The Requests page can delete an organization's logged bodies before a date, and the Overview shows providers with errors in the last hour.
- Configuration objects stored through the admin API ([docs/admin-ui-config-editing.md](docs/admin-ui-config-editing.md), ADR 0023): owners add and change providers, models, guardrails, MCP servers, virtual MCP servers and alert webhooks from a Configuration page or `PUT /admin/v1/config/{kind}/{name}`. They are merged into the config file, which wins on a name clash, so they get the same defaults and validation; a write that would break the config is refused. Secrets are encrypted with `secrets.encryption_key` and never returned. Every replica applies a change within `store.refresh_interval`. Writes are audited.

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
