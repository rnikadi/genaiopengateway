# 0017. Per-user upstream MCP credentials through a gateway-run OAuth flow

- Status: accepted
- Date: 2026-10-23
- Refines [0014](0014-mcp-auth.md) for v0.3

## Context

Remote MCP servers such as GitHub, Linear and Atlassian act for a person. v0.2 holds one static credential per upstream, so all users share one account. The MCP spec's own answer, each client running OAuth against each upstream, would put upstream tokens in clients and bypass the gateway's governance. ADR 0014 already identifies users by their OAuth subject on MCP endpoints.

## Decision

1. The gateway is a **confidential OAuth client of each upstream**, registered once by an admin. Each user connects their upstream account once, and the gateway stores that user's tokens by `(server, subject)`.
2. A call that needs a missing connection gets **URL elicitation** (`-32042`, MCP 2025-11-25) with a connect link, and a tool error containing the same link for older clients.
3. When SSO is configured, the connect page **requires sign-in as the same subject**. Otherwise the link is single use and expires in 10 minutes.
4. Tokens are encrypted with **AES-256-GCM** under `secrets.encryption_key`, with server and subject as additional data. `secrets.previous_keys` allows rotation.
5. `user_auth` upstreams get **one upstream session per user**, closed after 10 idle minutes and capped per replica.
6. Per-user credentials need an OAuth caller. Gateway keys keep using static credentials.

## Consequences

- Users act upstream as themselves, and the gateway still enforces policy, limits, pinning and audit on every call.
- The gateway now holds long-lived user tokens. That makes the encryption key a critical secret, and its loss forces every user to reconnect.
- One encryption key and one encrypted-column helper are added. Body logging (ADR 0018) reuses them.
- More upstream sessions per replica. The cap and idle timeout bound memory.
