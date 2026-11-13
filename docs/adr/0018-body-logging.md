# 0018. Opt-in body logging, redacted and encrypted in the store

- Status: accepted
- Date: 2026-10-23
- Refines [0007](0007-metering.md) for v0.3

## Context

v0.1 deliberately never stores request or response content. Enterprises need content for debugging, incident review and retention rules, but only for some teams, and with personal data and secrets removed.

## Decision

1. A **`log_bodies` policy field**, set at organization, team or key level, where the most specific setting wins. It is off by default, and there is a gateway-wide master switch.
2. **Redaction happens before storage**, on JSON string values, with checksum-validated builtins (email, phone, card numbers, IBAN, US SSN, well-known secrets) and custom RE2 patterns. Unredacted content is never persisted.
3. Bodies go into a **`request_bodies` table** in the existing store, keyed by request id, written asynchronously and **encrypted** with the key from ADR 0017. Streams are stored assembled, not as raw events.
4. **Truncation** (64 KiB per body by default) and **retention** (30 days by default) are enforced by the gateway.
5. Only owners and the organization's admins **read bodies**, and every read is audited.

## Consequences

- Teams that need content can have it without a sidecar or a separate logging pipeline.
- The store grows with content. Truncation and retention bound it, and operators must size the database for the teams they enable.
- Pattern-based redaction is not complete. The docs say so, and an external PII hook arrives with guardrails in v0.5.
- Shipping bodies to a SIEM waits for the export plugins in v0.5.
