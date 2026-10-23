# Per-user upstream MCP credentials

Many remote MCP servers act for a person: GitHub, Linear, Atlassian, Notion and Google Drive return what that person may see and record what they did. With v0.2 the gateway holds one static credential per upstream, so every user acts as one shared account. This design lets each user connect their own upstream account once, after which the gateway calls that upstream with the user's own token. Decisions are recorded in [ADR 0017](adr/0017-user-credentials.md).

## Goals

- A user connects an upstream account from their MCP client in one browser round trip, with no admin involved.
- Tokens are stored encrypted, refreshed automatically and never reach clients or logs.
- The person who completes the browser flow is provably the person whose MCP session asked for it.
- Admins can see who connected which upstream and revoke a connection.

## Not in scope

| Feature | Why |
| --- | --- |
| Per-user credentials for callers using gateway keys | A key identifies a team's application, not a person. Per-user servers need an OAuth caller (`mcp.oauth`, see [mcp.md](mcp.md#configuration)). |
| Upstreams that need API keys typed in by each user | Rare for remote servers, which use OAuth. Added if users ask. |
| Token exchange (RFC 8693) with the company's identity provider | Only works when the upstream trusts that provider, which public SaaS servers do not. Revisit with hosted servers. |
| Dynamic client registration with upstreams | The gateway is a confidential client registered once per upstream by an admin, which is what SaaS providers expect for an organization-wide app. |

## Config

```yaml
mcp:
  servers:
    github:
      url: https://api.githubcopilot.com/mcp/
      user_auth:                       # instead of static headers
        client_id: ${GITHUB_OAUTH_CLIENT_ID}
        client_secret: ${GITHUB_OAUTH_CLIENT_SECRET}
        scopes: [repo, read:org]
        # Discovered from the server's protected resource metadata (RFC 9728)
        # and the authorization server metadata (RFC 8414) when omitted:
        authorization_url: https://github.com/login/oauth/authorize
        token_url: https://github.com/login/oauth/access_token

secrets:
  encryption_key: ${GATEWAY_ENCRYPTION_KEY}   # 32 random bytes, base64; required for user_auth
```

`user_auth` and `headers` can be combined: static headers are still sent, for example an API version header. The redirect URL to register with the upstream is `{mcp.public_url}/mcp/connect/callback`.

Startup fails if a server has `user_auth` without `secrets.encryption_key`, without `mcp.oauth` (there would be no users), or without a store.

## The connect flow

```
MCP client ──tools/call github_search──▶ gateway: no token for (github, alice)
           ◀── error -32042 with a connect URL ──
user opens URL ──▶ gateway /mcp/connect/{id} ──(SSO sign-in if configured)──▶ GitHub consent
GitHub ──callback──▶ gateway: exchange code, store tokens for (github, alice) ──▶ "Connected, return to your client"
MCP client retries the call ──▶ gateway calls GitHub as alice
```

1. **A call needs a token.** A `tools/call`, `prompts/get` or `resources/read` routed to a `user_auth` upstream, from a caller with OAuth subject `alice` and no stored token, does not reach the upstream. The gateway creates a **connect request**: a random id, the server, the subject, the virtual server, and a 10 minute expiry, stored in the store.
2. **The client is told where to go.** The gateway answers with the JSON-RPC error `-32042` (URL elicitation required, MCP 2025-11-25) carrying `{mode: "url", url, elicitationId, message}`. Clients on older versions cannot show that, so for them the gateway returns a tool result with `isError: true` and the same URL and instructions as text, which every client shows to the user.
3. **The user opens the URL**, `{public_url}/mcp/connect/{id}`. When `sso.oidc` is configured, the gateway first signs the user in with the identity provider, the same one that issues MCP tokens, and refuses to continue unless the signed-in subject equals the connect request's subject. This is what stops a leaked or phished link from binding someone else's upstream account to Alice. Without `sso.oidc` the link itself is the proof, so it is single use, expires in 10 minutes, and is only ever sent to the authenticated MCP client.
4. **Upstream consent.** The gateway redirects to the upstream's authorization endpoint with PKCE, `state` bound to the connect request, and the configured scopes.
5. **Callback.** The gateway exchanges the code, encrypts and stores the access and refresh tokens for `(server, subject)`, deletes the connect request, writes an audit entry (`mcp.connection.create`), and shows a page saying the connection worked and the user can return to their client.
6. **Retry.** The client calls again. Clients that support URL elicitation retry on their own after `notifications/elicitation/complete`; the gateway cannot send that on a stateless request, so they retry when the user confirms, as the spec allows.

## Calls with a user's token

- The upstream gets `Authorization: Bearer <user access token>`. The client's own token is never forwarded (unchanged from v0.2).
- **Refresh.** A token within 60 seconds of expiry is refreshed before the call, under a per-connection lock, so concurrent calls refresh once. If refresh fails with `invalid_grant`, the connection is deleted and the call starts a new connect flow.
- **Upstream `401`.** The token is refreshed once and the call retried only if the request never reached the tool (the upstream answered `401` at the HTTP layer). Otherwise the connection is marked broken and the next call starts a connect flow.
- **Sessions.** v0.2 shares one upstream session per server. A `user_auth` server gets one session per `(server, subject)` instead, created on first use, closed after 10 minutes idle, and capped at `mcp.max_user_sessions` per replica (default 1000, least recently used closed first).

## The catalog

Listing still runs on the refresh interval, but a `user_auth` upstream may hide tools from anonymous callers. The gateway lists it with the token of the most recently active connected user, and keeps the last good catalog when no user is connected. Tool pinning and approval work as in v0.2. Users who have not connected see the tools and get the connect flow on their first call.

When the upstream returns different tools to different users, users only reach the tools in the shared catalog. Per-user catalogs are not worth their cost: tool lists of SaaS servers depend on scopes, not on the person.

## Storage

New table `mcp_connections`:

| Column | Notes |
| --- | --- |
| `server`, `subject` | Primary key. |
| `access_token`, `refresh_token` | AES-256-GCM with `secrets.encryption_key`. The nonce is random per write, and the additional data is `server` and `subject`, so a ciphertext copied to another row does not decrypt. |
| `key_id` | Which key encrypted the row, for rotation. |
| `scopes`, `expires_at` | From the token response. |
| `created_at`, `last_used_at` | Shown to admins. |

New table `mcp_connect_requests` (id hash, server, subject, virtual server, PKCE verifier, expiry), pruned hourly.

**Key rotation.** `secrets.encryption_key` takes the current key; `secrets.previous_keys` lists old ones that still decrypt. Rows are re-encrypted with the current key on their next refresh.

## Admin API and UI

- `GET /admin/v1/mcp/connections?server=&subject=`: subject, server, scopes, created, last used. Never tokens.
- `DELETE /admin/v1/mcp/connections/{server}/{subject}`: revokes at the upstream when it has a revocation endpoint (RFC 7009), then deletes. Audited.
- Users disconnect from `{public_url}/mcp/connections`, a small page behind SSO sign-in that lists their own connections.
- Roles: owners see and revoke all connections. Organization admins and viewers see none, because connections belong to people, not organizations.
- The admin UI's MCP page gets a Connections table with a Revoke action.

## Security

| Threat | Mitigation |
| --- | --- |
| A connect link is phished or leaked, and the attacker completes it to bind their upstream account to the victim | With SSO, the browser user must sign in as the same subject. Without it, links are single use, expire in 10 minutes and are only sent over the victim's authenticated MCP connection. |
| Token theft from the database | Tokens are encrypted with a key that is not in the database. A stolen backup without the key is useless. |
| A token moved to another user's row | GCM additional data binds each ciphertext to its server and subject. |
| Tokens in logs, traces or audit | Tokens are never logged. Audit entries name the server, subject and scopes only. |
| Authorization code interception | PKCE with S256, and `state` bound to the connect request. |
| Open redirect via the connect flow | The only redirect targets are the configured authorization URL and the gateway's own pages. |
| One user's upstream rate limit or outage affects others | Per-user sessions isolate state. Rate limits at the gateway are unchanged. |

## Testing

- Unit tests: encryption round trip and the additional data check, key rotation, the refresh lock, connect request expiry and single use, and the subject check.
- Integration test with a fake upstream that is both an MCP server and an OAuth authorization server: first call returns `-32042`, the browser flow completes, the retry succeeds with the user's token, refresh happens on expiry, `invalid_grant` restarts the flow, and two users see their own data.
- The Compose setup gets an upstream that needs per-user OAuth, using Dex as its authorization server, and the browser test runs the connect flow.

## Metrics

- `gog_mcp_connections` (gauge, by server)
- `gog_mcp_connect_flows_total` (by server and result: `completed`, `expired` or `denied`)
- `gog_mcp_token_refreshes_total` (by server and result)
