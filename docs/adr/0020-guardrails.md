# 0020. Guardrails: built-in checks plus model and webhook checks, no WebAssembly yet

- Status: accepted
- Date: 2026-10-23

## Context

v0.5 adds guardrails ([guardrails.md](../guardrails.md)). ADR 0008 planned third-party guardrails as WebAssembly modules through wazero. Most companies want three things: keep personal data and secrets away from providers, catch prompt injection in tool output, and use the moderation or DLP service they already have.

## Decision

1. Four guardrail types: `pii` and `regex` run in-process; `model` asks a classifier through a configured virtual model; `webhook` calls an operator service over signed HTTPS.
2. Four stages: `input`, `output`, `tool_input` and `tool_output`. Three actions: `block`, `redact` and `log`.
3. Guardrails are defined in config and attached by name in policies, where the lists **add up** across organization, team and key, and on virtual MCP servers.
4. `output` guardrails on streams buffer the response and send it after the check.
5. WebAssembly plugins are **deferred**. This supersedes the guardrail part of ADR 0008 for v0.5.

## Consequences

- An admin can turn on PII protection in minutes, and plug in any external service without new code in the gateway.
- No plugin host API to design, version and secure in v0.5. Custom logic runs out of process, at the cost of a network round trip.
- Adding up lists is a different rule from other policy fields (where the most specific wins). It is the safe one for controls an organization must enforce, and it is documented.
- Streamed responses with `output` guardrails lose time to first token. This is the honest cost of checking output before the client sees it.
