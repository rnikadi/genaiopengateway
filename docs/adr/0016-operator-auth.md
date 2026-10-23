# 0016. Operator sign-in with OIDC, server-side sessions and fixed roles

- Status: accepted
- Date: 2026-10-23
- Refines [0009](0009-policy.md) for v0.3

## Context

v0.1 and v0.2 have one admin token. Enterprises need people to sign in with the company's identity provider, need the audit log to name them, and need to limit what each person can change. ADR 0009 planned fixed roles per organization (owner, admin, member, viewer) with Cedar for finer rules in v0.5.

## Decision

1. **OIDC only** for operator sign-in: authorization code flow with PKCE against one issuer, using `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`. SAML and SCIM are deferred (admin-ui.md lists why).
2. **Roles from the identity provider:** config maps groups (or emails) to roles at each sign-in. The gateway stores no users or role assignments, so the identity provider stays the single source of truth.
3. **Three fixed roles:** `owner` (gateway-wide), `admin` (one organization), `viewer` (gateway-wide or one organization). ADR 0009's `member` arrives with self-service keys, after v0.3.
4. **Server-side sessions** in the store, keyed by the hash of a random cookie value, with a fixed lifetime (default 8 hours).
5. **The admin token stays** for automation and break-glass access, and acts as `owner`. Without OIDC, the UI accepts it at sign-in.
6. **CSRF defence by `SameSite=Lax`, JSON-only writes and an `Origin` check** rather than CSRF tokens.

## Consequences

- No user database to keep in sync, and a person who leaves a group loses access within one session lifetime.
- Sessions work across replicas and can be revoked by deleting rows. Each admin request costs one indexed store lookup, which is fine for admin traffic.
- Two well-maintained dependencies are added for OIDC instead of reimplementing discovery, the code exchange and ID token checks.
- Per-team roles and attribute rules wait for Cedar policies (v0.5).
