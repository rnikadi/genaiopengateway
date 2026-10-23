# Admin UI: editing configuration

Phase 3 of the admin UI coverage plan (issue #34). Operators create and change providers, models, guardrails, MCP servers, virtual MCP servers and alert webhooks from the admin UI or API, while the config file keeps working as today. The decision is [ADR 0023](adr/0023-hybrid-config.md).

## Model

A stored object has a `kind`, a `name` and a `spec`. The spec is the object exactly as it would be written under its key in `config.yaml`, in JSON:

| Kind | Config file location | Example spec |
| --- | --- | --- |
| `providers` | `providers.<name>` | `{"type": "bedrock", "region": "eu-west-1"}` |
| `models` | `models.<name>` | `{"targets": [{"provider": "openai", "model": "gpt-4.1-mini", "price": {"input_per_mtok": 0.4, "output_per_mtok": 1.6}}]}` |
| `guardrails` | `guardrails.<name>` | `{"type": "pii", "action": "redact", "stages": ["input", "output"]}` |
| `mcp_servers` | `mcp.servers.<name>` | `{"url": "https://mcp.example.com/mcp", "pin_tools": "strict"}` |
| `mcp_virtual_servers` | `mcp.virtual_servers.<name>` | `{"servers": [{"server": "docs", "tools": ["search"]}]}` |
| `alert_webhooks` | `alerts.webhooks[]`, by `name` | `{"url": "https://hooks.slack.com/...", "format": "slack", "events": ["budget.threshold"]}` |

The running config is the file with the stored objects added. Because the merge happens before parsing, a stored object gets the same defaults and the same validation as one in the file, and the rest of the gateway does not know where an object came from.

## Secrets

Secret fields (`api_key`, webhook `url` and `secret`, MCP server `headers` values, `user_auth.client_secret`) are encrypted with `secrets.encryption_key` in the store, like users' MCP tokens; storing an object with a secret needs that key. A response lists secret fields as `"set"`, never the value. An update that omits a secret field, or sends `"set"`, keeps the stored value. Stored values are not expanded as `${VAR}`.

## Admin API

All writes need the owner role and are audited as `config.create`, `config.update` and `config.delete`.

| Method and path | Purpose |
| --- | --- |
| `GET /admin/v1/config` | Every object of every kind with `kind`, `name`, `source` (`file` or `store`), `shadowed`, `spec` (secrets as `"set"`), `updated_at`, `updated_by`; and `error` when the stored objects could not be applied |
| `PUT /admin/v1/config/{kind}/{name}` | Create or replace a stored object. `400` with the validation errors when the merged config would be invalid; `409` when the file defines that name |
| `DELETE /admin/v1/config/{kind}/{name}` | Delete a stored object. Refused when something still refers to it, as the file would be, such as a model whose targets use the provider |

## Propagation

The replica that takes a write applies it at once. A revision number in the store changes with every write; the other replicas compare it on their regular refresh and rebuild their runtime config when it moves, within `store.refresh_interval`. A config file reload (SIGHUP) merges the stored objects again.

## UI

The Providers, Models, Guardrails, MCP and Alerts pages get **New** and **Edit** buttons for owners. Each kind has a form for its common fields and an **Edit as JSON** view of the whole spec, for fields the form does not cover. Objects from the file show a "From config file" label and no edit button. Validation errors from the API are shown on the form.

## Not in this phase

- Editing file-only settings (store, limiter, TLS, SSO, body logging, secrets).
- Moving objects between the file and the store; an operator copies the spec by hand.
- Per-organization configuration objects.

## Testing

- Config package: merge order, file wins, secrets never expanded, invalid objects refused with the same messages as the file.
- Store: CRUD on SQLite and Postgres, the revision counter, encrypted secrets at rest.
- Server: owner only, audit, secrets never returned and kept on update, a second replica picks up a change on refresh, startup with a stored object that the file has made invalid.
- Playwright: an owner adds a provider and a model in the UI, then calls the model in the playground.
