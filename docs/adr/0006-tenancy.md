# 0006. Organization, team, key hierarchy

- Status: accepted
- Date: 2026-10-22

## Context

Retrofitting tenancy is the most expensive change a gateway can make. Budgets and access must be set at several levels.

## Decision

Three levels in v0.1: organization, team, API key. Every key belongs to one team; every team to one organization. Model allow lists, rate limits and budgets can be set at each level; a request must pass every level above it (the tightest wins). Users and roles attach to organizations in v0.3 with SSO.

Every store row carries `org_id`, and every admin query is scoped by it.

## Consequences

- Chargeback by team works from day one.
- A "project" level can be added later as an optional level between team and key without breaking keys.
