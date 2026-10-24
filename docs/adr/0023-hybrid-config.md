# 0023. Configuration from the file or the admin API

- Status: accepted
- Date: 2026-10-23

## Context

ADR 0003 put providers, models, guardrails, MCP servers and alert webhooks in `config.yaml`. Platform teams that run the gateway with GitOps want that. Operators who only have the admin UI cannot add a provider or a model without a deploy, which is the most common complaint about the UI. Ram chose the hybrid option in the admin UI coverage plan (issue #31).

## Decision

1. Providers, models, guardrails, MCP servers, virtual MCP servers and alert webhooks can also be created, changed and deleted through the admin API and UI. They are stored in the database as objects with a kind, a name and a spec in exactly the config file's format.
2. The running config is the file merged with the stored objects. A name defined in the file wins; the stored object is shown as shadowed and not used. Objects from the file are read-only in the UI.
3. Every write is validated by merging it into the current file config and running the same validation as the file. An object that would make the merged config invalid is refused.
4. Secrets in stored objects are encrypted with `secrets.encryption_key`, are never returned, and are kept unchanged when an update leaves them out. Stored values are never expanded as `${VAR}`: an admin API user must not be able to read the gateway's environment.
5. Writes are for owners only and are audited. Replicas pick up changes within `store.refresh_interval`, the same way they pick up keys.
6. Store, limiter, TLS, SSO, body logging, secrets and the listeners stay file-only: a mistake there can lock everyone out or stop the gateway from starting.

## Consequences

- Operators manage what changes often from the UI; platform teams keep the reviewed file for the rest, and can still pin anything by defining it in the file.
- If a file change makes a stored object invalid, such as removing a provider a stored model uses, the reload is refused as any invalid file is. At startup the gateway starts with the file alone, logs the error and shows it in the UI, rather than not starting.
- Backups of the database now hold configuration as well as tenants.
