# Admin UI: editing configuration

Phase 3 of the admin UI coverage plan (issue #34). Operators create and change providers, models, guardrails, MCP servers, virtual MCP servers and alert webhooks from the admin UI or API, while the config file keeps working as today. The decision is [ADR 0023](adr/0023-hybrid-config.md); [ADR 0028](adr/0028-ui-wins-over-the-file.md) lets the UI change and delete objects from the file too.

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

The running config is the file with the stored objects added. A stored object with the name of a file object replaces it, and a stored spec of `null` (a deletion marker) removes it (ADR 0028). Because the merge happens before parsing, a stored object gets the same defaults and the same validation as one in the file, and the rest of the gateway does not know where an object came from.

## Secrets

Secret fields (`api_key`, guardrail and webhook `url` and `secret`, MCP server `headers` values, `user_auth.client_secret`) are encrypted with `secrets.encryption_key` in the store, like users' MCP tokens; storing an object with a secret needs that key. The spec is stored with `"set"` in place of each secret, and a response shows exactly that, never the value. In an update:

- `"set"` keeps the stored value;
- leaving a field out keeps it too, except a header left out of a `headers` map that is sent, which is removed;
- `""` removes it.

Stored values are not expanded as `${VAR}`, so the admin API cannot read the gateway's environment.

## Objects from the file

An object from the config file can be edited and deleted like any other, and the change wins over the file:

| Action | What is stored | Shown as |
| --- | --- | --- |
| Edit | A copy with the same name, which replaces the file's object | **Changed here** |
| Delete | A deletion marker, so the object does not come back after a restart | **Deleted here** |
| Reset | Nothing: the copy or marker is removed and the file's version applies | **From config file** |

A secret sent as `"set"` on a file object keeps the file's value. It is read from the file at every merge rather than copied into the store, so a key from `${VAR}` keeps following the environment, and no encryption key is needed unless a new secret is typed. Objects not changed in the UI keep following the file; a file edit to a changed object has no effect until it is reset.

## Admin API

All writes need the owner role and are audited as `config.create`, `config.update` and `config.delete`.

| Method and path | Purpose |
| --- | --- |
| `GET /admin/v1/config` | Every object of every kind, once, with `kind`, `name`, `source` (`file`, `store`, `changed` or `deleted`) and `spec` (the spec in use, secrets as `"set"`). Changed and deleted file objects also have `file_spec`; stored ones `updated_at` and `updated_by`. Also the store's `revision`, and `error` when the stored objects do not apply |
| `PUT /admin/v1/config/{kind}/{name}` | Create or replace a stored object; for a name in the file, store the copy that replaces it. `400` with the validation errors when the merged config would be invalid |
| `DELETE /admin/v1/config/{kind}/{name}` | Remove an object from the running config: a stored one is deleted, one from the file gets a deletion marker. Refused when something still refers to it, as the file would be, such as a model whose targets use the provider |
| `POST /admin/v1/config/{kind}/{name}/reset` | Remove the stored copy or deletion marker of a file object, so the file's version applies again. `404` when there is none |

## Propagation

Before a write is stored, the merged config is built as the gateway would run it, so a write that would break the config is refused with the same message the file would get. The replica that takes a write applies it at once. A revision number in the store changes with every write; the other replicas compare it on their regular refresh and rebuild their runtime config when it moves, within `store.refresh_interval`. A config file reload (SIGHUP) merges the stored objects again. If stored objects stop applying (for example the file dropped a provider a stored model uses), a running replica keeps its config, a starting one serves the file alone, and both report the problem in `error` and on the Configuration page.

## UI

A **Configuration** page for owners has a tab per kind listing every object with its source (**From config file**, **Changed here**, **Deleted here** or **Stored**), with **New**, **Edit** and **Delete**, and **Reset** on changed or deleted file objects. Every kind has a form for all its fields, and **Edit as JSON** for the whole spec. Gateway-wide settings are a stored object too (kind `settings`, named `gateway`), edited on the Settings page; see [admin-ui-settings.md](admin-ui-settings.md). Validation errors from the API are shown on the form. The Providers and Models pages link to it for owners.

## Not in this phase

- Editing file-only settings (store, limiter, TLS, SSO, body logging, secrets).
- Per-organization configuration objects.

## Testing

- Config package: merge order, a stored object replacing or removing a file object, secrets never expanded, invalid objects refused with the same messages as the file.
- Store: CRUD on SQLite and Postgres, the revision counter, encrypted secrets at rest.
- Server: owner only, audit, secrets never returned and kept on update, a second replica picks up a change on refresh, startup with a stored object that the file has made invalid.
- Server: editing, deleting and resetting a file object, file secrets kept by reference and never stored.
- Playwright: an owner adds a provider and a model in the UI, then calls the model in the playground; edits a model from the file, sees **Changed here**, and resets it.
