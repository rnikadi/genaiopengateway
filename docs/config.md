# Configuration and data model

## Config file

The gateway reads one YAML file (`-config`, default `config.yaml`). Any `${VAR}` is replaced with the environment variable `VAR`; an unset variable is an error, so typos fail at startup. Send SIGHUP to reload; an invalid file is logged and ignored.

```yaml
listen: ":8080"                      # model API, admin API, health

tls:                                 # optional; usually TLS ends at the load balancer
  cert_file: /etc/genaiopengateway/tls.crt
  key_file: /etc/genaiopengateway/tls.key

admin:
  token: ${GATEWAY_ADMIN_TOKEN}      # acts as owner; omit to rely on SSO alone

sso:                                 # operator sign-in to the admin UI; see admin-ui.md
  oidc:
    issuer: https://login.example.com
    client_id: genaiopengateway
    client_secret: ${OIDC_CLIENT_SECRET}
    redirect_url: https://gateway.example.com/ui/auth/callback
    groups_claim: groups             # default
    scopes: [openid, email, profile] # default
  session_ttl: 8h                    # default
  roles:                             # matched at every sign-in, by group or email
    - group: platform-admins
      role: owner                    # everything
    - group: acme-leads
      role: admin                    # one organization: its teams, keys and policies
      org: acme
    - group: finance
      role: viewer                   # read only; omit org for every organization

ui:
  enabled: true                      # default when the admin API is enabled

alerts:                              # see alerts.md; needs a store
  budget_thresholds: [0.5, 0.8, 1.0] # default; shares of each budget
  webhooks:
    - name: finance
      url: https://hooks.example.com/genai   # https, or http to localhost
      secret: ${ALERT_WEBHOOK_SECRET}        # signs each body (X-GOG-Signature)
      events: [budget.threshold]             # budget.threshold, mcp.tool_pending, key.expiring
    - name: platform
      url: ${SLACK_WEBHOOK_URL}
      format: slack                          # json (default) or slack
      events: [budget.threshold, mcp.tool_pending, key.expiring]
      org: acme                              # only this organization's alerts; omit for all

secrets:
  encryption_key: ${GOG_ENCRYPTION_KEY}  # 32 random bytes, base64 (openssl rand -base64 32)
  previous_keys: []                      # old keys, still used to decrypt after rotation

body_logging:                        # see body-logging.md; needs a store and secrets.encryption_key
  enabled: false                     # master switch; policies (log_bodies) choose what is logged
  retention: 30d                     # default 30d, at most 365d
  max_body_bytes: 65536              # each of request and response, after redaction
  redact:
    builtin: [email, phone, credit_card, iban, us_ssn, secret]   # default: all
    patterns: ['EMP-\d{6}']         # extra RE2 expressions, replaced by [REDACTED:custom]

guardrails:                          # see guardrails.md; each runs only where a policy or virtual server names it
  no-pii:
    type: pii                        # pii | regex | model | webhook
    detect: [email, phone, credit_card, iban, us_ssn, secret]   # default: all
    action: redact                   # block | redact | log
    stages: [input]                  # input | output | tool_input | tool_output
  dlp:
    type: webhook
    url: https://dlp.internal.example.com/check
    secret: ${DLP_WEBHOOK_SECRET}
    stages: [input, output]
    timeout: 2s                      # model and webhook only; default 2s, at most 30s
    on_error: allow                  # allow | block when the check fails

metrics:
  listen: ":9090"                    # omit to serve /metrics on the main listener

store:                               # omit to run without a database (static keys only)
  driver: sqlite                     # sqlite | postgres
  dsn: file:/var/lib/genaiopengateway/gateway.db
  refresh_interval: 5s
  usage_retention: 90d               # omit to keep usage forever

limiter:
  driver: memory                     # memory | valkey
  address: valkey:6379
  password: ${VALKEY_PASSWORD}
  fail_open_rate_limits: true
  fail_open_budgets: false

limits:
  max_request_bytes: 33554432        # 32 MiB; also each realtime message
  max_realtime_session: 30m          # realtime sessions close after this
  max_batch_file_bytes: 104857600    # batch input files, read into memory to check (batch.md)

cache:                               # response cache (cache.md); policies turn it on
  max_entry_bytes: 1048576           # larger answers are not cached
  max_memory_bytes: 268435456        # without the valkey limiter: per replica, least recently used out
  semantic:
    model: embed-default             # embeddings model; required for mode: semantic
    threshold: 0.95                  # cosine similarity a reworded question needs
    max_entries: 10000               # questions remembered per replica

providers:
  openai:
    type: openai                     # openai | anthropic | gemini | vertex | bedrock
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
    timeout: 300s                    # whole request, including streaming; default 5m
    headers: {}                      # extra headers sent upstream
  anthropic:
    type: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
  gemini:
    type: gemini                     # base_url defaults to the Gemini API
    api_key: ${GEMINI_API_KEY}
  vertex:
    type: vertex                     # Gemini and Claude on Vertex AI; see cloud-providers.md
    project: acme-ai-prod
    location: global                 # global, us, eu, or a region
    # credentials_file: /var/run/secrets/gcp/key.json   (default: Application Default Credentials)
  bedrock:
    type: bedrock                    # every Bedrock model; see cloud-providers.md
    region: eu-central-1
    # api_key: ${AWS_BEARER_TOKEN_BEDROCK}   (default: the AWS credential chain, signed with SigV4)

models:
  chat-default:
    targets:
      - provider: openai
        model: gpt-4.1-mini
        price: {input_per_mtok: 0.40, output_per_mtok: 1.60, cached_input_per_mtok: 0.10}
      - provider: anthropic
        model: claude-haiku-4-5
        price: {input_per_mtok: 1.00, output_per_mtok: 5.00}
  image:                             # media endpoints; see modalities.md
    targets:
      - provider: openai
        model: gpt-image-1
        price: {per_image: 0.04}     # also per_mchars, per_audio_minute, per_video_second, per_request,
                                     # audio_input_per_mtok, audio_output_per_mtok (realtime),
                                     # batch_discount (0.5 for half price in batches)

keys:                                # static keys; read-only
  - name: local-dev
    key: ${GATEWAY_DEV_KEY}
    allowed_models: []
    allowed_mcp_servers: []
    rpm: 60

mcp:                                 # optional; see mcp.md for every field
  servers:
    github:
      url: https://api.githubcopilot.com/mcp/
      headers: {Authorization: "Bearer ${GITHUB_MCP_TOKEN}"}
    linear:                          # each user connects their own account
      url: https://mcp.linear.app/mcp
      user_auth: {client_id: "${LINEAR_CLIENT_ID}", client_secret: "${LINEAR_CLIENT_SECRET}"}
  virtual_servers:
    engineering:
      guardrails: [no-pii]           # run at tool_input and tool_output
      servers:
        - server: github
          tools: [search_code, get_file_contents]
```

Prices in the example are placeholders; set them from your contracts.

### Provider types

| Type | Works with | Endpoints served |
| --- | --- | --- |
| `openai` | OpenAI, Azure OpenAI (v1 API), Google Gemini OpenAI-compatible endpoint, Mistral, Groq, Together, Fireworks, vLLM, SGLang, Ollama, LM Studio and other OpenAI-compatible servers | chat, embeddings, responses |
| `anthropic` | Anthropic API | chat (translated), messages (passthrough) |
| `gemini` | Gemini API | chat (translated), generateContent (passthrough) |
| `bedrock` | Amazon Bedrock: any model through Converse; Claude (ids with `anthropic.`) also through InvokeModel; Titan and Cohere embedding models | chat (translated), messages (Claude, passthrough), embeddings (translated) |
| `vertex` | Vertex AI: Gemini models (`gemini-2.5-pro` or `google/gemini-2.5-pro`) and Claude models (`anthropic/claude-sonnet-4-6`) | Gemini: chat (translated), generateContent (passthrough). Claude: chat (translated), messages (passthrough) |

### Validation

The admin API and UI are enabled when there is a `store` and either `admin.token` or `sso.oidc`. The UI is served at `/ui/`.

Startup fails if a model references an unknown provider, a virtual MCP server references an unknown upstream or two of its names collide, a provider other than `gemini`, `vertex` or `bedrock` has no `base_url`, a `vertex` provider has no `project` or `location`, a `bedrock` provider has no `region`, a target has no `model`, a static key is empty or duplicated, a duration or number is malformed, a guardrail is incomplete or names an unknown model, or a virtual server names an unknown guardrail, or `sso` is incomplete (an http(s) issuer, a client id and secret, a redirect URL ending in `/ui/auth/callback`, at least one role mapping with exactly one of `group` or `email`, `org` on every `admin`, none on `owner`).

## Data model

All ids are prefixed, time-sortable strings (`org_`, `team_`, `key_`, `req_`). Times are UTC.

| Table | Columns |
| --- | --- |
| `orgs` | `id`, `name`, `policy` (JSON: allowed models and MCP servers, rpm, tpm, budget, guardrails, log_bodies, cache), `created_at`, `updated_at` |
| `teams` | `id`, `org_id`, `name`, `policy`, `created_at`, `updated_at` |
| `api_keys` | `id`, `org_id`, `team_id`, `name`, `prefix`, `secret_hash`, `policy`, `expires_at`, `revoked_at`, `last_used_at`, `created_at`, `updated_at` |
| `usage` | `request_id`, `ts`, `org_id`, `team_id`, `key_id`, `endpoint`, `model`, `provider`, `upstream_model`, `status`, `latency_ms`, `input_tokens`, `output_tokens`, `cached_input_tokens`, `reasoning_tokens`, `cost_usd`, `cache_hit`, `saved_usd` (and the media counts) |
| `audit_log` | `id`, `ts`, `actor`, `action`, `resource_type`, `resource_id`, `org_id`, `details` (JSON) |
| `mcp_tool_pins` | `server`, `tool`, `hash` (of the definition last seen), `definition` (JSON), `approved_hash` (empty until approved), `updated_at`. Status is derived: `approved` when the hashes match, `pending` when nothing was approved, `changed` otherwise. |
| `mcp_calls` | `id`, `ts`, `org_id`, `team_id`, `key_id`, `subject`, `virtual_server`, `server`, `method`, `name`, `status`, `error_code`, `latency_ms`, `request_id`. Pruned with `usage_retention`. |
| `alerts_sent` | `id`, `ts`, `event`, `dedupe_key` (unique), `org_id`, `data` (JSON) |
| `mcp_connections` | `server`, `subject` (key), `access_token` and `refresh_token` (AES-256-GCM encrypted), `scopes`, `expires_at`, `created_at`, `updated_at`, `last_used_at` |
| `mcp_connect_requests` | `id_hash`, `state_hash`, `server`, `subject`, `virtual_server`, `verifier`, `expires_at`. Pruned hourly. |
| `request_bodies` | `id`, `request_id`, `ts`, `org_id`, `team_id`, `key_id`, `kind` (`model` or `mcp`), `endpoint`, `request` and `response` (redacted, then AES-256-GCM encrypted), `truncated`, `redactions` (JSON counts). Pruned after `body_logging.retention`. |
| `guardrail_events` | `id`, `request_id`, `ts`, `org_id`, `team_id`, `key_id`, `guardrail`, `stage`, `action`, `failed`, `reason`, `counts` (JSON, by kind), `latency_ms`. Never the matched text. Pruned with `usage_retention`. |
| `schema_migrations` | `version`, `applied_at` |

Key secrets have the form `sk-gog-` followed by 40 random base62 characters (about 238 bits). Only the SHA-256 hash and the first 11 characters (the prefix, for display) are stored. Because the secret is high-entropy, a fast hash is appropriate and keeps lookups constant time.

Policies are stored as JSON so new policy fields need no migration.

Usage rows older than `store.usage_retention` are pruned daily. `last_used_at` is updated in batches, never on the request path.
