# 0030: Only owners change an organization

- Status: accepted
- Date: 2026-10-26

## Context

An organization's policy (allowed models and MCP servers, rate limits, budget, guardrails, body logging, cache) bounds every team and key in it: lists narrow and limits apply at each level. The October 2026 pentest found that an organization's own admins could change that policy. They could raise a budget the owner set, widen the allowed models, or remove a guardrail the owner required. Each change was audited, but nothing prevented it, so the owner's limits were not a ceiling. ADR 0029 had already made renaming owner-only.

Ram chose to make organization limits owner-only.

## Decision

- `PATCH /admin/v1/orgs/{id}` needs the owner role, for every field.
- Organization admins see the organization's policy read-only. They still create, edit and delete its teams, create, edit, rotate and revoke its keys, set team provider credentials, and clear its cache, all within the organization's limits.

## Consequences

- Delegating an organization means the owner sets its ceiling, and its admins shape teams and keys under it.
- An organization admin who used to raise their own limits now asks an owner.
