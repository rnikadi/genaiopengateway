# 0027: Every runtime setting is edited in the admin UI, and a test keeps it so

- Status: accepted
- Date: 2026-10-25

## Context

Operators want to run the gateway from the admin UI: whatever the gateway supports must be configurable there, with its default visible and changeable. Stored objects (ADR 0023) already cover providers, models, guardrails, MCP servers, virtual servers and webhooks. About 35 gateway-wide settings, such as limits, body logging, the cache, alert thresholds, SSO, role mappings and MCP OAuth, are only in the config file. Some objects have only a raw JSON editor. Nothing stops a new setting from shipping without a UI control.

## Decision

- Split settings into startup settings and runtime settings. Startup settings stay in the environment and config file: listeners, TLS, store, limiter connection, encryption keys, the admin token, `ui.enabled` and static keys. Runtime settings are everything else.
- Store the gateway-wide runtime settings as one object of kind `settings`, named `gateway`, in the config file's shape. Merge it over the file, so a value set in the UI wins and resetting it falls back to the file, then the default. Named objects keep ADR 0023's rule that the file wins.
- Make the Settings page editable for owners, showing each setting's value, default and source, with reset. Give every object kind a form.
- Apply changes on every replica within the refresh interval, without restarts.
- Fail the `ui` tests when a config path is neither a declared startup setting nor named by a `data-setting` control in the UI.

## Consequences

- An operator can run the gateway with a config file that only holds startup settings.
- Values set in the UI and in the file can differ, and the UI value wins. The Settings page shows each value's source, so the difference is visible.
- Each new config field needs a UI control in the same change, or the build fails.
- A wrong SSO setting can lock out SSO users, but the admin token still signs in.
