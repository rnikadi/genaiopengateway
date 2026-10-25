# Admin UI and SSO

v0.3 adds a web UI for operators, sign-in through the company's identity provider, and roles. This document is the design. Decisions are recorded in ADRs [0015](adr/0015-admin-ui.md) and [0016](adr/0016-operator-auth.md).

## Goals

- An operator can do everything the admin API offers from a browser: manage organizations, teams and keys, see usage and spend, approve MCP tools and read the audit log.
- People sign in with the company's identity provider (OIDC), not a shared token, and every change in the audit log names a person.
- Roles limit who can change what, so a team lead can see their organization's spend without being able to touch anything else.
- The UI ships inside the gateway binary: nothing extra to build, deploy or patch.

## Not in v0.3

| Feature | Why, and when |
| --- | --- |
| SAML | Every identity provider we target speaks OIDC, and SAML-only ones can be bridged with Keycloak or Dex. Added if users ask. |
| SCIM provisioning | Roles come from the identity provider's groups at each sign-in, so users and access stay in sync without SCIM. Revisited with self-service keys. |
| Custom roles, team-level roles, self-service keys for members | Fixed roles cover operators and team leads. Finer rules are Cedar policies in v0.5 (ADR 0009). |
| Editing providers, models and MCP servers from the UI | They live in the config file by design (ADR 0003). The UI shows them read-only ([admin-ui-config.md](admin-ui-config.md)); editing them from the UI as well is planned (issue #34). |
| Personal API tokens for the admin API | Automation keeps using the admin token. Per-user tokens can follow if needed. |

## Pages

| Page | Contents | Admin API used |
| --- | --- | --- |
| Overview | Spend and requests this month, top teams and models, MCP calls, tools awaiting approval, providers with errors in the last hour | `usage`, `mcp/servers`, `mcp/tools`, `providers` |
| Organizations | List, create, edit policy (allowed models and MCP servers, RPM, TPM, budget, response cache), delete | `orgs` |
| Organization | Its teams, usage by team, edit, clear the response cache | `orgs/{id}`, `orgs/{id}/teams`, `usage`, `cache` |
| Team | Its keys, usage by key and model, edit policy, clear the response cache | `teams/{id}`, `teams/{id}/keys`, `usage`, `cache` |
| Key | Limits, expiry, revoke; the secret is shown once, on create | `keys/{id}`, `keys/{id}/revoke` |
| Usage | Filter by organization, team, key and dates; group by model, team, key, day, provider or endpoint; chart, table and CSV download; cache hits and money saved when the cache answered | `usage` |
| MCP | Upstream servers and their status, virtual servers, tools with pin status; review a tool's definition and approve it; users' connections to per-user servers, with revoke (owners); call records | `mcp/*` |
| Jobs | Batches and video jobs of an organization: team, model, target, status, request counts and cost; cancelling a running batch (owners and organization admins; audited). Metadata only, never file contents ([batch.md](batch.md)) | `jobs` |
| Requests | Recent model requests; for those with a logged body, the redacted request and response (owners and organization admins; each view is audited); deleting an organization's bodies before a date (owners) | `requests` |
| Guardrails | Events: what guardrails blocked, redacted or logged, by organization and action, never the matched text ([guardrails.md](guardrails.md)). Definitions: each guardrail's checks, action, stages and error handling | `guardrail-events`, `guardrails` |
| Audit log | Who changed what, filterable | `audit` |
| Models | Virtual models, the endpoints they serve, and their targets with provider type, price and last-hour health, read-only ([admin-ui-config.md](admin-ui-config.md)) | `models` |
| Playground | Call a model as a team with the team's policies; metered and audited (owners and organization admins, [playground.md](playground.md)) | `playground/chat/completions` |
| Providers | Upstream providers with type, endpoint, auth, the models using them and last-hour health | `providers` |
| Alerts | Alerts raised, filterable; webhooks with a test button (owners) | `alerts`, `alerts/webhooks`, `alerts/test` |
| Configuration | Add, change and delete providers, models, guardrails, MCP servers and alert webhooks stored in the database; objects from the file are read-only (owners, [admin-ui-config-editing.md](admin-ui-config-editing.md)) | `config` |
| Settings | Every runtime setting with its default and where its value comes from, editable by owners; startup settings read-only; secrets shown only as set | `settings` |

Forms for allowed models and MCP servers offer the configured names, so a typo cannot create a policy that silently allows nothing.

## Roles

Three fixed roles. A role is granted gateway-wide or for one organization.

| Role | Gateway-wide | For one organization |
| --- | --- | --- |
| `owner` | Everything: all organizations, MCP tool approvals, the full audit log | Not applicable |
| `admin` | Not applicable | Edit the organization's policy; create, edit and delete its teams; create, edit and revoke its keys; see its usage, MCP calls and audit entries |
| `viewer` | Read everything | Read the organization, its teams, keys, usage, MCP calls and audit entries |

Creating and deleting organizations, approving MCP tools, and reading entries not tied to an organization need `owner`. The admin token acts as `owner`.

Roles come from the identity provider on each sign-in, through mappings in config:

```yaml
sso:
  roles:
    - {group: ai-platform, role: owner}
    - {group: search-leads, role: admin, org: acme}       # org name or id
    - {group: finance, role: viewer}
    - {email: alice@example.com, role: viewer, org: acme}
```

A user gets every role whose `group` is in the groups claim or whose `email` matches. A user with no role cannot sign in. Changing a group in the identity provider takes effect at the user's next sign-in, and at most `session_ttl` later.

## Sign-in

### With OIDC

```yaml
sso:
  oidc:
    issuer: https://login.example.com/realms/acme
    client_id: genaiopengateway
    client_secret: ${OIDC_CLIENT_SECRET}
    redirect_url: https://gateway.example.com/ui/auth/callback
    groups_claim: groups          # default "groups"
    scopes: [openid, email, profile]
  session_ttl: 8h                 # default 8h
  roles: [...]
```

1. `/ui/` without a session redirects to `/ui/auth/login`, which redirects to the issuer with the authorization code flow, PKCE (S256), `state` and `nonce`.
2. `/ui/auth/callback` exchanges the code, verifies the ID token (signature through the issuer's JWKS, `iss`, `aud`, `exp`, `nonce`), and reads `sub`, `email`, `name` and the groups claim. If the ID token has no groups, the UserInfo endpoint is asked.
3. The roles are resolved. With none, the user sees "no access" and nothing is stored.
4. A session is created and the user is sent back to the page they asked for (a same-origin path only).

The client is confidential (it has a secret) and only needs the redirect URL registered at the identity provider. It uses only standard OIDC discovery, so any compliant provider works (Keycloak, Dex, Okta, Entra ID, Google, Auth0). The Compose setup tests it against Dex.

### Without OIDC

When `sso.oidc` is not configured, the sign-in page asks for the admin token. This suits local development and small setups. The session acts as `owner` and the audit actor is `admin-token`, as for API calls.

## Sessions

- A session is a random 256-bit id in a cookie: `HttpOnly`, `SameSite=Lax`, `Path=/`, and `Secure` with the `__Host-` prefix when the gateway is reached over HTTPS (TLS on the gateway, or `X-Forwarded-Proto: https` from the load balancer).
- The store keeps only the id's SHA-256 hash, the user, the resolved roles and the expiry, so sessions work across replicas and a database leak yields no usable cookie.
- Sessions expire `session_ttl` after sign-in; there is no sliding renewal. Signing out deletes the session. Expired sessions are pruned with usage records.
- Every admin API request with a session cookie checks the session in the store. Admin traffic is light, and this keeps revocation immediate.

## Admin API changes

- **Authentication:** the admin API accepts the admin token (`Authorization: Bearer`) or a session cookie.
- **CSRF:** a request authenticated by cookie that changes something (`POST`, `PATCH`, `DELETE`) must carry `Content-Type: application/json` and an `Origin` header matching the gateway's own origin. Browsers do not let other sites send either, so no CSRF token is needed.
- **Authorization:** every handler checks the caller's role for the organization the resource belongs to, in one place. Lists are filtered to the organizations the caller can see. A forbidden request answers `403` with code `forbidden`.
- **Audit actor:** the user's email (or `sub` when there is none) instead of `admin-token`.
- **New endpoints:**

| Method and path | Purpose |
| --- | --- |
| `GET /admin/v1/me` | The signed-in user, their roles, and whether SSO is on |
| `GET /admin/v1/models` | Virtual models with their targets (no credentials) |
| `GET /admin/v1/mcp/virtual-servers` | Virtual MCP servers and the tools they expose |
| `GET /ui/auth/login`, `GET /ui/auth/callback`, `POST /ui/auth/logout` | Sign-in and sign-out |
| `POST /ui/auth/token` | Sign in with the admin token (only without OIDC) |

## Data model

| Table | Columns |
| --- | --- |
| `admin_sessions` | `id_hash` (primary key), `subject`, `email`, `name`, `roles` (JSON), `created_at`, `expires_at` |

Users are not stored beyond their sessions: the identity provider is the source of truth, and the audit log keeps who did what.

## Serving the UI

- Static files are embedded in the binary with `go:embed` and served at `/ui/` from the main listener, only when the admin API is enabled (a store and either an admin token or SSO).
- Strict headers on every UI response: `Content-Security-Policy: default-src 'self'; img-src 'self' data: blob: https:; media-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`. No inline scripts or styles. The image and media sources let the playground show generated images, inline or at the provider's URL, and play generated speech.
- Every file is sent with an `ETag` and `Cache-Control: no-cache`, so browsers revalidate cheaply and pick up a new gateway version on the next load without hashed file names or a build step.
- `ui.enabled: false` turns the UI off while keeping the admin API.

## Docker Compose

An override file, `compose.sso.yaml`, adds Dex with two static users (an owner and an organization viewer) and points the gateway at a config with `sso` set, so sign-in and roles can be tried and tested locally without an identity provider account.

## Testing

- Unit tests for role resolution and for the authorization check on every admin handler (each role against each resource).
- An end-to-end OIDC test against a fake issuer: code flow, PKCE, nonce, a bad `state`, a user with no roles, session expiry and sign-out.
- CSRF tests: cookie requests without `Origin`, with a foreign `Origin`, and with a form content type are rejected.
- Browser tests with Playwright against the Compose setup: sign in through Dex, create an organization, team and key, see the key once, revoke it, approve an MCP tool, and check that the viewer cannot change anything.

## Security

| Threat | Mitigation |
| --- | --- |
| Stolen session cookie | `HttpOnly`, `Secure` over HTTPS, 8 hour lifetime, deleted on sign-out; only the hash is stored |
| CSRF from another site | `SameSite=Lax`, plus JSON content type and `Origin` checks on every change |
| XSS in data shown by the UI (tool descriptions, team names) | Text is set with `textContent` only, never as HTML; strict CSP with no inline scripts |
| Clickjacking | `frame-ancestors 'none'` |
| Login CSRF and code injection | `state`, `nonce` and PKCE, each bound to a short-lived, signed cookie |
| Open redirect after sign-in | Only same-origin paths are accepted as return targets |
| A user keeps access after leaving a group | Roles are re-read at each sign-in and sessions last at most `session_ttl` |
| Privilege escalation through the API | Authorization is enforced in the admin API, not the UI; the UI can do nothing the API would refuse |
