# Admin UI: everything that is configured

The v0.3 admin UI manages tenants, usage and MCP, but most of what an operator configures in `config.yaml` never reaches it: providers, guardrail definitions, alert webhooks, sign-in roles and the gateway's own settings. This design makes all of it visible, read-only, with health from the usage records. It is phase 1 of the plan tracked in issue #31; the playground (phase 2) and editing configuration from the UI (phase 3, hybrid configuration) follow in their own designs.

## Goals

- An operator can see from the browser every provider, model, guardrail, webhook and setting the gateway is running with, without reading the config file or the pod.
- Each provider and model target shows whether it is healthy: requests, errors and latency in the last hour, across all replicas.
- No secret ever leaves the gateway: API keys, DSNs, passwords, webhook URLs' paths, client secrets and the admin token are reported only as set or not set.

## Admin API

All new endpoints are read-only, need any operator role unless noted, and come from the live config, so they follow a reload.

| Method and path | Returns |
| --- | --- |
| `GET /admin/v1/providers` | Each provider: `name`, `type`, `base_url`, `region`, `project`, `location`, `auth`, `timeout`, `header_names`, the virtual `models` that use it, and `health` |
| `GET /admin/v1/models` (extended) | Each target also has `provider_type`, `endpoints` (what it serves, from the adapter) and `health`; each model has the union of its targets' `endpoints` |
| `GET /admin/v1/guardrails` | Each guardrail: `name`, `type`, `action`, `stages`, `detect`, `patterns`, `model`, `unsafe_if`, `timeout`, `on_error`, `webhook_host`, and the `virtual_servers` that attach it |
| `GET /admin/v1/alerts/webhooks` | Each webhook: `name`, `format`, `events`, `org`, `host`; and the default `budget_thresholds` |
| `GET /admin/v1/settings` | Owners only. Version; store driver, refresh interval, usage retention; limiter driver and fail-open choices; request size limit; body logging (enabled, retention, size, detectors, number of custom patterns); encryption key set and number of previous keys; TLS on or off; metrics listener; admin token set; SSO issuer, client id, groups claim, session TTL and role mappings; MCP public URL and OAuth issuer; static keys from config with their policies, never the key |
| `GET /admin/v1/usage?group_by=provider\|endpoint` | Two new groupings |

`auth` is one of `api_key`, `none`, `application_default` (Vertex AI without a credentials file), `credentials_file`, or `aws_default` (Bedrock without an API key: the AWS default chain, including workload identity).

`health` covers the last hour, from usage records, so it is the same on every replica: `requests`, `errors` (status 400 or above), `error_rate`, `avg_latency_ms` and `last_error_at`. Usage records hold the final attempt of each request; a target that failed and fell back to the next one is counted in `gog_fallbacks_total`, not here. A primary target with no requests while its model has traffic is the visible sign of that, and the Models page points it out.

A test walks every new response with a config full of secrets and fails if any secret value appears.

## Pages

| Page | Change |
| --- | --- |
| Providers (new) | One row per provider: type, endpoint (base URL, region, or project and location), auth, models, and health. A provider with errors in the last hour is marked. |
| Models | Per target: provider type, endpoints served, and health. A target with no traffic while a later target of the same model has traffic is marked "not answering". |
| Guardrails | Two tabs: Events (today's page) and Definitions. |
| Alerts (new) | The alert list with organization and event filters and paging; the webhooks with events, format and host; a Send test button for owners. The Overview keeps its recent alerts. |
| Settings (new, owners only) | The settings above, in panels, with the static keys table. |
| Usage | Group by provider and by endpoint. |

Navigation gains Providers, Alerts and Settings; Settings is shown only to owners.

## Not in this phase

- Editing any of this from the UI: phase 3, issue #34.
- Per-replica health or live metrics: Prometheus already has them, and per-replica numbers in a UI behind a load balancer mislead.
- Guardrail attachment counts on organization, team and key policies: the policy forms already show which guardrails each one runs.

## Testing

- Unit tests for each endpoint: fields, role checks (settings is owners only), and the secret scan.
- Store tests for the new usage groupings and the health query on SQLite and Postgres.
- The Playwright test opens Providers, Models, Guardrails definitions, Alerts and Settings, and sends a test alert.
