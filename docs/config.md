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
  max_request_bytes: 33554432        # 32 MiB

providers:
  openai:
    type: openai                     # openai | anthropic
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
    timeout: 300s                    # whole request, including streaming; default 5m
    headers: {}                      # extra headers sent upstream
  anthropic:
    type: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}

models:
  chat-default:
    targets:
      - provider: openai
        model: gpt-4.1-mini
        price: {input_per_mtok: 0.40, output_per_mtok: 1.60, cached_input_per_mtok: 0.10}
      - provider: anthropic
        model: claude-haiku-4-5
        price: {input_per_mtok: 1.00, output_per_mtok: 5.00}

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
  virtual_servers:
    engineering:
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

### Validation

The admin API and UI are enabled when there is a `store` and either `admin.token` or `sso.oidc`. The UI is served at `/ui/`.

Startup fails if a model references an unknown provider, a virtual MCP server references an unknown upstream or two of its names collide, a provider has no `base_url`, a target has no `model`, a static key is empty or duplicated, a duration or number is malformed, or `sso` is incomplete (an http(s) issuer, a client id and secret, a redirect URL ending in `/ui/auth/callback`, at least one role mapping with exactly one of `group` or `email`, `org` on every `admin`, none on `owner`).

## Data model

All ids are prefixed, time-sortable strings (`org_`, `team_`, `key_`, `req_`). Times are UTC.

| Table | Columns |
| --- | --- |
| `orgs` | `id`, `name`, `policy` (JSON: allowed models, rpm, tpm, budget), `created_at`, `updated_at` |
| `teams` | `id`, `org_id`, `name`, `policy`, `created_at`, `updated_at` |
| `api_keys` | `id`, `org_id`, `team_id`, `name`, `prefix`, `secret_hash`, `policy`, `expires_at`, `revoked_at`, `last_used_at`, `created_at`, `updated_at` |
| `usage` | `request_id`, `ts`, `org_id`, `team_id`, `key_id`, `endpoint`, `model`, `provider`, `upstream_model`, `status`, `latency_ms`, `input_tokens`, `output_tokens`, `cached_input_tokens`, `reasoning_tokens`, `cost_usd` |
| `audit_log` | `id`, `ts`, `actor`, `action`, `resource_type`, `resource_id`, `org_id`, `details` (JSON) |
| `mcp_tool_pins` | `server`, `tool`, `hash` (of the definition last seen), `definition` (JSON), `approved_hash` (empty until approved), `updated_at`. Status is derived: `approved` when the hashes match, `pending` when nothing was approved, `changed` otherwise. |
| `mcp_calls` | `id`, `ts`, `org_id`, `team_id`, `key_id`, `subject`, `virtual_server`, `server`, `method`, `name`, `status`, `error_code`, `latency_ms`, `request_id`. Pruned with `usage_retention`. |
| `schema_migrations` | `version`, `applied_at` |

Key secrets have the form `sk-gog-` followed by 40 random base62 characters (about 238 bits). Only the SHA-256 hash and the first 11 characters (the prefix, for display) are stored. Because the secret is high-entropy, a fast hash is appropriate and keeps lookups constant time.

Policies are stored as JSON so new policy fields need no migration.

Usage rows older than `store.usage_retention` are pruned daily. `last_used_at` is updated in batches, never on the request path.
