# 0003. YAML for infrastructure, database for runtime objects

- Status: accepted
- Date: 2026-10-22

## Context

Providers and virtual models change rarely and belong in version control. Organizations, teams, keys and budgets change at runtime and are created through an API.

## Decision

- A YAML file holds listen address, providers, virtual models, price overrides, and store and limiter settings. Secrets are referenced as `${ENV_VAR}`.
- The store holds organizations, teams, API keys, budgets, limits and usage records, managed through the admin API.
- YAML may also declare static keys for single-node setups with no admin API use. Static keys are read-only.
- The file is reloaded on SIGHUP; an invalid file is rejected and the previous config stays active.

## Consequences

- GitOps works for infrastructure; runtime changes need no redeploy.
- Two sources of keys exist; static keys are clearly marked in API responses and cannot be edited through the API.
