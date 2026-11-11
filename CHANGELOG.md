# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow [semantic versioning](docs/releasing.md).

## [Unreleased]

### Added

- Provider types `elevenlabs`, `deepgram` and `assemblyai` ([docs/modalities.md](docs/modalities.md#speech-providers-with-their-own-apis)): ElevenLabs text to speech and Scribe, Deepgram Aura and pre-recorded transcription, and AssemblyAI transcription, served as OpenAI's `audio.speech` and `audio.transcriptions` with the usual metering, fallback and team credentials. AssemblyAI transcripts are deleted upstream once read. Presets and prices for all three are in the catalog, and the conformance run covers them.
- Provider types `stability` and `fal` for image generation ([docs/modalities.md](docs/modalities.md#image-providers-with-their-own-apis)), and `cohere` for chat, embeddings and rerank ([docs/modalities.md](docs/modalities.md#cohere)), with catalog presets. The Mistral preset adds Voxtral transcription.
- Veo video jobs on `gemini` targets ([docs/modalities.md](docs/modalities.md#veo-on-the-gemini-api)): create, retrieve, content and delete through OpenAI's `videos` endpoints, metered by the second.
- Nova Reel video jobs on `bedrock` targets with the new `s3_output_uri` setting ([docs/modalities.md](docs/modalities.md#nova-reel-on-bedrock)), including multi-shot videos up to two minutes. Content is read from S3 with SigV4.
- A Gemini Live relay ([docs/modalities.md](docs/modalities.md#gemini-live)) at Google's BidiGenerateContent path, so Google's SDKs work unchanged: the setup's model is a virtual model, the session goes to a `gemini` target's Live model, and each turn's usage is charged with audio prices. The catalog gains the `live` kind.

## [0.11.0] - 2026-11-11

Enterprise identity: the v0.11 scope of the [roadmap](docs/roadmap.md). Operators sign in with SAML or OIDC and are provisioned by SCIM, roles are sets of permissions down to one team, Cedar policies add attribute-based rules, teams request MCP servers from a catalog, and tenants are managed as code with `gogctl` or Terraform.

### Upgrade notes

- Migrations 0017 to 0020 add the SCIM, SAML assertion, Cedar policy and MCP access request tables. They run at startup, as before.
- Built-in roles are unchanged, but `GET /admin/v1/me` now lists each grant's permissions, and role mappings can name a `team`.
- `gogctl config import` reports `N changes applied` instead of `N objects imported`, and exports gain an `orgs` section. Files without it import as before.
- The SAML implementation uses gosaml2. SHA-1 signatures are refused, so identity providers must sign with SHA-256 or better.

### Added

- Custom and per-team roles ([docs/roles.md](docs/roles.md), ADR 0032). Roles are now sets of permissions, such as `keys.write` or `usage.read`. `sso.custom_roles` defines new ones, and a role mapping with a `team` grants a role for one team: its keys, credentials, usage and playground, but not its policy. Both are editable on the Settings page. `GET /admin/v1/me` lists each grant's permissions.
- SCIM 2.0 provisioning ([docs/scim.md](docs/scim.md)) at `/scim/v2`, for Okta, Entra ID and other identity providers, with a bearer token in `sso.scim.token`. Role mappings match provisioned users' groups from the directory, so group changes apply at once, and deactivating or deleting a user signs them out. A new Directory page shows what was provisioned, and every change is audited. Migration 0017 adds the tables.
- SAML 2.0 sign-in for the admin UI ([docs/saml.md](docs/saml.md)), as an alternative to OIDC, with setup for Okta, Entra ID and Keycloak. The gateway serves its service provider metadata at `/ui/auth/saml/metadata`. Responses must answer a request the browser started, SHA-1 signatures are refused, and each assertion signs in once across replicas (migration 0018). Roles, team grants and SCIM work as with OIDC.
- Cedar policies per organization ([docs/cedar.md](docs/cedar.md), ADR 0009): attribute-based rules on models and MCP tools by team, key, end user, time and client address, evaluated in-process with cedar-go. Owners edit them on the organization page, every save is a version, and anyone who can read the organization can check a request against them. A denied model request gets 403 `policy_denied` naming the policy; a denied tool call gets a tool error. Migration 0019 adds the table.
- An MCP catalog with access requests ([docs/mcp-catalog.md](docs/mcp-catalog.md)). The MCP page lists the virtual servers with a team's access to each; anyone who can read a team can request a server for it, and operators who manage the organization's teams approve, which adds the server to the team's `allowed_mcp_servers`, or deny with a note. Requests and decisions are audited. Migration 0020 adds the table.
- Declarative tenants in `gogctl config export` and `import` ([docs/gogctl.md](docs/gogctl.md#configuration-as-code)): organizations by name with their policies, Cedar policies and teams. Import creates what is missing and updates what differs, and `-prune` deletes what the file's sections leave out. Importing the same file twice changes nothing. Exports now include the `orgs` section, and import reports `N changes applied` instead of `N objects imported`.
- A compliance guide ([docs/compliance.md](docs/compliance.md)) mapping the gateway's controls to SOC 2, ISO 27001, GDPR, the EU AI Act, HIPAA and PCI DSS, with the records it keeps, a hardening checklist and answers to common questionnaire items.
- A Terraform provider ([docs/terraform.md](docs/terraform.md)) in `terraform/`, with resources for organizations, teams, keys, Cedar policies and stored configuration objects, plus an organization data source. Acceptance tests run it against a real gateway in CI, and release archives include its binary.

## [0.10.0] - 2026-11-11

Easy to use and reliable at scale: the v0.9 and v0.10 scope of the [roadmap](docs/roadmap.md). A new user gets a first completion without writing YAML, and a provider that fails mid-load costs no requests when another target exists.

### Upgrade notes

- The circuit breaker is on by default (5 failures in a row, 30s cooldown). Set `routing.circuit_breaker.failures: 0` to keep the old behavior.
- Upstream spans are renamed from `gateway.upstream` to `<operation> <model>`, such as `chat gpt-4.1-mini`; update trace queries and dashboards that match on the old name.
- `GET /admin/v1/models` and `/admin/v1/providers` gain `strategy`, `sticky`, `retries`, per-target `circuit` and `residency` fields.
- `streams` is a new stored configuration kind and `secrets.kms` a new startup setting; neither changes anything until it is set.
- No database migrations.

### Added

- Zero-config start ([docs/quickstart.md](docs/quickstart.md)): without a config file the gateway starts on SQLite with in-memory limits, generates an admin token and an encryption key into `bootstrap.json` in its data directory (`-data`, `GOG_DATA_DIR`, `/var/lib/genaiopengateway` in the image), and prints the admin UI link and the token. `docker run` needs no mounted file.
- A setup wizard in the admin UI: pick a provider preset, paste a key and choose models, create an organization, team and key, send a test request, and copy snippets for curl, Python, Node and MCP clients. The Overview points owners to it while no provider is configured. A browser test runs the whole flow in CI and fails if it takes 5 minutes or more.
- Provider presets and a model price catalog ([docs/catalog.md](docs/catalog.md)) for OpenAI, Anthropic, Gemini, Mistral, Groq, DeepSeek, OpenRouter, Azure OpenAI, Bedrock, Vertex AI, Ollama and vLLM, served at `GET /admin/v1/catalog`.
- `gogctl`, a command line tool over the admin API ([docs/gogctl.md](docs/gogctl.md)): organizations, teams, keys with rotation, usage, the catalog, and `config export` and `config import` of stored configuration objects as YAML for configuration as code. It ships in the release archives.
- Routing strategies ([docs/routing.md](docs/routing.md), ADR 0031): each model chooses `ordered` (the default), `weighted`, `round_robin`, `least_latency` or `lowest_cost`, with optional sticky routing per user for prompt-cache hits.
- Retries on the same target with jittered exponential backoff that honors `Retry-After`, and a per-target timeout to the first response byte that never cuts off a stream.
- A circuit breaker per target (`routing.circuit_breaker`, on by default: 5 failures in a row, 30s cooldown) with a single half-open probe. Its state shows on the Models page, in `GET /admin/v1/models`, and in the `gog_target_circuit_state` metric. Config reloads keep it.
- Data residency pinning ([docs/routing.md](docs/routing.md#data-residency)): label providers with `residency`, and pin organizations, teams and keys to labels. A pinned request only reaches matching providers, and is refused with `403 residency_not_allowed` when none qualifies.
- OpenTelemetry GenAI semantic conventions ([docs/observability.md](docs/observability.md#traces)): request parameters (`gen_ai.request.max_tokens`, `temperature`, `top_p` and others), cached input tokens and `error.type` on spans, and the `gen_ai_client_operation_duration_seconds` and `gen_ai_client_token_usage` histograms.
- A Grafana dashboard and Prometheus alerting rules, with a [runbook](docs/runbook.md) section for each alert. The Helm chart installs them with `grafanaDashboard.enabled` and `prometheusRule.enabled`, and the Compose `observability` profile now starts Grafana with the dashboard.
- A chaos test (`test/chaos`) that runs failover against upstreams that refuse connections, fail, rate limit, stall and recover.
- Streams ([docs/streams.md](docs/streams.md)) copy usage, audit, MCP call and guardrail records to a webhook, as signed NDJSON batches, or to a JSON Lines file for a log shipper, once they are written to the database. They are configured in the file, the admin API or the new Streams tab of the Configuration page, and counted in `gog_stream_records_total`.
- The encryption key can be kept wrapped by AWS KMS, Google Cloud KMS or Vault Transit (`secrets.kms`, [docs/secrets.md](docs/secrets.md)). It is unwrapped at startup with the platform's usual credentials, including Vault's Kubernetes login, and exists only in memory.
- An install script (`scripts/install.sh`) that downloads the latest release for Linux or macOS and checks it against the release checksums.

### Changed

- Upstream spans are named for the operation and model, such as `chat gpt-4.1-mini`, as the OpenTelemetry GenAI conventions say, where they were all `gateway.upstream`.
- The circuit breaker is on by default, so a target that fails 5 times in a row is skipped for 30 seconds where it used to be tried first on every request. Set `routing.circuit_breaker.failures: 0` to keep the old behavior.

### Fixed

- The release workflow signs the Helm chart: cosign now has registry credentials. The 0.8.0 chart was published without a signature.

## [0.8.0] - 2026-11-11

Every model type and enterprise control built since 0.3: guardrails, Gemini, Vertex AI and Bedrock, a Helm chart, images, speech, transcription, video, realtime, batch, a response cache, and the whole configuration in the admin UI. The version follows the [roadmap](docs/roadmap.md), whose v0.5 and v0.8 scope this release covers.

### Upgrade notes

- Migrations 0007 to 0016 run on start; take a database backup first. Migration 0015 appends the id to organization names that clash without case. On Postgres they run under an advisory lock, so replicas can start together.
- Organization admins can no longer change their own organization's policy (ADR 0030); give owners that job.
- Compose now publishes its ports on 127.0.0.1 only.
- Configuration objects changed in the admin UI win over the config file (ADR 0028). Use Reset on the Configuration page to go back to the file's version.

### Docs and tooling

- A documentation site built from `docs/` with MkDocs Material and published to GitHub Pages ([mkdocs.yml](mkdocs.yml)); CI builds it in strict mode so broken links fail the build.
- A route reference generated from the handlers ([docs/reference/routes.md](docs/reference/routes.md)); a test fails when a route is added without regenerating it or without a doc comment on its handler.
- A reproducible benchmark ([docs/benchmarks.md](docs/benchmarks.md)): `scripts/bench.sh` measures the latency the gateway adds and its throughput against a fake upstream, optionally beside LiteLLM. The gateway adds about 2 ms at p50 at 12,800 requests a second on 4 vCPUs. CI fails when it adds more than 10 ms at p50.
- A security response policy with response times and a 90-day disclosure window ([SECURITY.md](SECURITY.md)), a monthly release cadence and the post-1.0 support policy ([docs/releasing.md](docs/releasing.md)), CODEOWNERS and issue templates.

### Security

- Only owners change an organization (ADR 0030), from the October 2026 pentest. Organization admins could raise the budget and limits an owner set, widen allowed models, or remove a required guardrail; they now see the organization's policy read-only and still manage its teams and keys.
- Hardening from the October 2026 pentest. Compose publishes the gateway, Dex, Jaeger and Prometheus on 127.0.0.1 only, since its sample admin token is public, and the gateway logs a warning while that token is in use. Every response gets `X-Content-Type-Options: nosniff`, and `Strict-Transport-Security` over HTTPS. A key cannot be created or updated with an `expires_at` in the past.
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

- Admin UI tidy-ups from the October 2026 walkthrough. The Providers and Models pages have an Edit button on each row for owners, which opens that object's editor on the Configuration page (`#/config?kind=&edit=`). Viewers no longer see the Playground in the navigation, and opening an owner-only page by its address shows "Owners only" instead of an error and failed API calls. The Models page says that endpoints come from each target's provider type.
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

[Unreleased]: https://github.com/rnikadi/genaiopengateway/compare/v0.11.0...HEAD
[0.11.0]: https://github.com/rnikadi/genaiopengateway/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/rnikadi/genaiopengateway/compare/v0.8.0...v0.10.0
[0.8.0]: https://github.com/rnikadi/genaiopengateway/compare/v0.3.0...v0.8.0
[0.3.0]: https://github.com/rnikadi/genaiopengateway/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/rnikadi/genaiopengateway/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/rnikadi/genaiopengateway/releases/tag/v0.1.0
