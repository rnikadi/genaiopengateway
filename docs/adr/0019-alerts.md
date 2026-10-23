# 0019. Budget alerts detected on charge, delivered by signed webhooks

- Status: accepted
- Date: 2026-10-23

## Context

Budgets refuse requests once spent, and nobody hears about them before that. Admins also need to know when an MCP tool waits for approval or a key is about to expire. Prometheus already covers operational alerting such as error rates and latency.

## Decision

1. Three events only: `budget.threshold`, `mcp.tool_pending` and `key.expiring`.
2. Thresholds are detected **inside `Charge`**, from the before and after values of the atomic spend counter, so the request path pays nothing extra. A unique row in `alerts_sent` makes each alert fire once across replicas and counter re-seeds.
3. Delivery is to **webhooks from config**, as JSON signed with a Stripe-style HMAC, or in Slack format. A background worker sends them with bounded retries, and delivery is at most once.
4. No durable outbox. Sent alerts are recorded in the store and the audit log.

## Consequences

- Warnings arrive while there is still time to act, with no extra store or Valkey calls per request.
- An alert can be lost if a replica dies with it queued. The record shows it was raised, which is acceptable for warnings.
- Webhook targets live in config, not the API, which avoids SSRF through the admin API and keeps secrets in the environment.
