# Admin UI: every setting in the UI

Every setting the gateway supports can be changed from the admin UI, with its default shown, except the few the process needs before it can start. This design closes the gaps listed in the configuration audit: about 35 runtime settings that the Settings page shows read-only or not at all, and four kinds of objects that can only be edited as raw JSON. It also adds a check that fails the build when a new setting has no control in the UI. The decision is [ADR 0027](adr/0027-settings-in-the-ui.md). It extends [admin-ui-config-editing.md](admin-ui-config-editing.md).

## Startup settings and runtime settings

**Startup settings** stay in the environment (or the config file, which expands `${VAR}`). The gateway reads them before the store exists, or a mistake made in the UI could lock everyone out:

| Setting | Why it is read at startup |
| --- | --- |
| `listen`, `metrics.listen`, `tls.cert_file`, `tls.key_file` | The ports and certificates the process opens |
| `store.driver`, `store.dsn` | Where everything else, settings included, is stored |
| `limiter.driver`, `limiter.address`, `limiter.password` | The counters replicas share, opened once |
| `secrets.encryption_key`, `secrets.previous_keys` | They decrypt what the UI stores |
| `admin.token` | The break-glass sign-in, which must work when SSO settings are wrong |
| `ui.enabled` | Whether the UI is served at all |
| `keys[]` | Static keys, for running without a database |

Deployments pass their values as `${VAR}` from the environment, as the compose example does with `${GATEWAY_ADMIN_TOKEN}` and the Helm chart with `envFrom`. The Settings page shows them read-only and says how to change them.

**Runtime settings** are everything else. They are the named objects of [admin-ui-config-editing.md](admin-ui-config-editing.md) (providers, models, guardrails, MCP servers, virtual MCP servers and alert webhooks) and the gateway settings below.

## Gateway settings

A new stored object of kind `settings`, always named `gateway`, holds the gateway settings changed in the UI. Its spec has the config file's shape, limited to these fields:

```yaml
store:        {refresh_interval, usage_retention}
limiter:      {fail_open_rate_limits, fail_open_budgets}
limits:       {max_request_bytes, max_realtime_session, max_batch_file_bytes}
body_logging: {enabled, retention, max_body_bytes, redact: {builtin, patterns}}
cache:        {max_entry_bytes, max_memory_bytes, semantic: {model, threshold, max_entries}}
alerts:       {budget_thresholds}
sso:          {session_ttl, roles, oidc: {issuer, client_id, client_secret, redirect_url, groups_claim, scopes}}
mcp:          {public_url, refresh_interval, max_user_sessions, oauth: {issuer, jwks_url, audience, organization, team_claim, scopes}}
```

A startup field in the spec is refused with `400`. `sso.oidc.client_secret` is a secret and is stored encrypted, as provider keys are.

**Precedence.** A field set in the UI wins over the config file. The config file supplies the first values, and the defaults fill in the rest. Named objects keep the opposite rule from ADR 0023, where the file wins on a name clash, because an object defined in a file is usually deployed from version control as a whole. A single gateway setting is a knob someone turns in the UI, so the UI value wins. Resetting a field in the UI removes it from the stored spec, so the file's value or the default applies again.

**Validation and propagation.** A change is checked by merging it into the file and parsing the result, as stored objects are. A bad value is refused with the parser's message, and nothing is saved. Every replica applies the change within `store.refresh_interval`, with no restart. The parts read only at startup today are made to follow changes:

- the usage pruning loop reads `usage_retention` each time it runs;
- the limiter's fail-open flags can be changed while it runs;
- the in-memory cache and the semantic index can be resized.

**Who.** Only owners can change gateway settings, as with other configuration. Each change is audited as `config.update` with the fields that changed. A change to `sso` takes effect at the next sign-in. The admin token always works, so a broken identity provider setting can be fixed.

## The Settings page

The page becomes editable for owners, in the sections above, each with its own Save button. Every field:

- shows its current value, and its default beside it ("Default: 30 days");
- shows where the value comes from: **default**, **config file** or **set here**;
- has **Reset**, which goes back to the file's value or the default.

Sizes are entered in KiB or MiB, durations as `30m` or `24h`, and lists one item per line. Role mappings are edited as a table of group or email, role and organization. Secrets show as set or not set, as elsewhere.

## Forms for every object

The Configuration page gets forms in place of raw JSON for the four kinds that lack one. The JSON editor stays as an alternative.

- **Guardrails:** type, action, stages, and the type's fields: detectors for `pii`, patterns for `regex`, model, prompt and `unsafe_if` for `model`, and URL, secret, timeout and `on_error` for `webhook`.
- **MCP servers:** URL, headers (secret values), timeout, tool pinning, and per-user sign-in (`user_auth`).
- **Virtual MCP servers:** description, guardrails, and each server with its tools, prefix, prompts and resources.
- **Alert webhooks:** URL, secret, format, events and organization.

The provider form gains `headers` and `credentials_file`, and the model form gains `cached_input_per_mtok`. Every form starts new objects with the defaults filled in, such as a 5 minute provider timeout or a 2 second guardrail timeout, rather than empty fields that say "default".

## The check

The `ui` package's tests walk every field of the config types and name each one by its path, such as `cache.semantic.threshold` or `models.targets.price.per_image`, leaving out map keys and list indexes. Every path must be either:

- listed as a startup setting in `config.StartupPaths`, or
- named by a control in the UI: an element with `data-setting="<path>"` in `ui/static`.

A new field without one fails the test, with the path in the message. The browser test also opens each Settings section and form, and checks that the controls render.

## Admin API

- `GET /admin/v1/settings` gains `fields`, one entry per runtime setting with `path`, `value`, `default`, and `source` (`default`, `file` or `stored`), plus `stored` (the stored spec, secrets masked), `startup` (the startup settings) and `detectors` (built-in PII detectors). Secrets are reported as set or not set.
- Writes use the existing object API: `PUT /admin/v1/config/settings/gateway` with the full stored spec, and `DELETE` to reset everything. The UI sends the stored fields with the section's changes applied, so resetting a field means leaving it out.

## Testing

- Config: merge precedence (the stored value beats the file, which beats the default), refusing startup fields, and validation messages.
- Server: settings saved through the API take effect without a restart for each section (limits, body logging, cache sizes, fail-open, retention, SSO roles, MCP OAuth). Also the audit record, owner-only writes, encrypted client secret, and the `fields` report with sources and defaults.
- UI: the coverage check above, and the browser test changing a setting, seeing **set here**, resetting it and seeing the default again.

## Not in this design

- Moving startup settings into the UI.
- Editing objects defined in the config file. They stay read-only and are marked as coming from the file, and teams that want UI control define them in the UI instead.
- Per-replica overrides.
