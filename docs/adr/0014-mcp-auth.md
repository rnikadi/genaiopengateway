# 0014. MCP authentication: gateway keys and OAuth 2.1 resource server

- Status: accepted
- Date: 2026-10-23

## Context

MCP clients authenticate in two ways today. Developer tools and agents built on SDKs accept a static bearer token or header. Hosted clients, such as Claude, ChatGPT and IDE connectors, implement the MCP authorization spec: OAuth 2.1 with protected resource metadata (RFC 9728), resource indicators (RFC 8707), and client registration through Client ID Metadata Documents or dynamic registration.

The gateway has no user identities until SSO lands, but organizations already run an identity provider that can issue OAuth access tokens.

## Decision

1. **Gateway keys** work on MCP endpoints exactly as on model endpoints: `Authorization: Bearer sk-gog-…`, with org, team and key policies.
2. **OAuth 2.1 resource server.** When `mcp.oauth` is configured:
   - The gateway validates JWT access tokens from one external authorization server: signature through JWKS (discovered from the issuer's metadata), `iss`, `exp`, `nbf`, the audience (the virtual server's URL or the configured `audience`), and required scopes.
   - It publishes protected resource metadata at `/.well-known/oauth-protected-resource/mcp/{virtual_server}`.
   - It answers `401` with `WWW-Authenticate: Bearer resource_metadata="…"`, plus `scope` when scopes are configured.
   - It maps a valid token to the configured organization and to the team named by the configured claim. Limits apply at org and team level, and the token's `sub` is recorded in audit.
3. The gateway is **not an authorization server**: it does not issue tokens, proxy OAuth flows, or accept client registrations. Organizations use their identity provider (Okta, Entra ID, Auth0, Keycloak, and others) to authorize MCP clients.
4. **Upstream credentials** are static per upstream server in v0.2. Per-user upstream OAuth, where the gateway holds each user's token for each upstream, comes with SSO in v0.3.

## Consequences

- Hosted MCP clients can connect with the company's existing sign-in, and every call is attributable to a person.
- Not issuing tokens keeps the gateway clear of the confused-deputy and token-passthrough problems the MCP security guidance warns about.
- Opaque (non-JWT) access tokens are not supported in v0.2; token introspection (RFC 7662) can be added if users need it.
- One issuer per gateway in v0.2. Several issuers, for multiple organizations, follow with SSO.
