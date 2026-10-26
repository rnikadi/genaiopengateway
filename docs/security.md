# Security and threat model

This is the threat model as of v0.3. It is reviewed at every minor release.

## Assets

1. Upstream provider credentials.
2. Gateway API keys, the admin token and operator sessions.
3. Prompts and completions, which may contain personal or confidential data, including request bodies stored when body logging is on.
4. Users' upstream MCP tokens, stored when a `user_auth` server is connected, and `secrets.encryption_key`, which seals them and stored bodies.
5. Usage and cost records, and the audit log.
6. Availability of the gateway, which sits in the path of every AI call.

## Trust boundaries

- **Client to gateway:** untrusted. Every request is authenticated with a gateway key.
- **Operator to gateway:** authenticated with the admin token or, from v0.3, an OIDC sign-in with roles. Admin UI threats and controls are in [admin-ui.md](admin-ui.md#security).
- **Gateway to provider:** the provider is trusted with the request content it receives, but its responses are treated as untrusted input.
- **Gateway to upstream MCP servers and their authorization servers:** configured by the operator; responses, tool definitions and token endpoint replies are untrusted input.
- **User's browser in a connect flow:** untrusted. Connect links work once and expire, and the upstream callback must come back to the browser that opened the link.
- **Gateway to store and Valkey:** trusted network; credentials from environment. Stored user tokens and bodies are encrypted, so a database dump alone does not reveal them.

## Threats and mitigations (STRIDE)

| Threat | Example | Mitigation |
| --- | --- | --- |
| Spoofing | Guessing or stealing a gateway key | 238-bit random keys; constant-time hash lookup; keys revocable and expirable; keys never logged (only the prefix) |
| Spoofing | A user gets a role meant for someone else at sign-in | Organization names are unique and only owners rename one, so a role mapping names one organization; `email` mappings need an email the identity provider has not marked unverified (ADR 0029) |
| Spoofing | Client sends its own upstream key to reach a provider directly | Client `Authorization` and `x-api-key` headers are never forwarded upstream; the gateway sets upstream credentials itself |
| Spoofing | A user sends someone else a connect link so the victim's upstream account is bound to the attacker's identity, or the reverse | Links work once and expire in 10 minutes; PKCE and a state bound to a cookie set when the link was opened; with SSO on the same identity provider, only the user who asked can complete the link ([mcp-user-credentials.md](mcp-user-credentials.md#security)) |
| Tampering | Client sets `model` to an upstream model name to bypass allow lists | `model` must name a configured virtual model; unknown names are rejected |
| Tampering | Config edited to point a provider at an attacker host | Config file is operator-owned; reloads are logged with a content hash; v0.5 audit covers config changes |
| Repudiation | An admin deletes a team and denies it | Every admin write is recorded in `audit_log` with actor, action, resource and before/after |
| Information disclosure | Provider keys leaked in logs, errors or responses | Secrets are typed so they redact themselves when logged; upstream error bodies pass through but upstream request headers never do |
| Information disclosure | Prompts stored in logs | Request and response bodies are not logged in v0.1. From v0.3, body logging is opt-in per scope, redacted before storage, encrypted, size-bounded and expired after a retention period ([body-logging.md](body-logging.md)) |
| Information disclosure | Database dump reveals users' upstream tokens or stored bodies | AES-256-GCM under `secrets.encryption_key`, with the row's identity as associated data so a sealed value cannot be moved to another row; tokens are never returned by the API or logged; owners revoke connections, which also revokes them at the upstream |
| Information disclosure | Key from one org reads another org's data | Every admin query is scoped by `org_id`; usage queries filter by the caller's scope |
| Denial of service | Huge bodies or slow clients exhaust memory | `max_request_bytes`, header read timeout, idle timeout, upstream timeouts |
| Denial of service | One team floods a shared provider quota | Per-key, per-team and per-org RPM and TPM limits; budgets |
| Denial of service | Store outage stops traffic | Request path serves from an in-memory snapshot (ADR 0004) |
| Elevation of privilege | Model key used against the admin API | Admin API accepts only the admin token or an operator session; model keys are rejected |
| Elevation of privilege | An organization admin reads another organization's data or stored bodies | Roles are scoped to one organization and checked on every admin request; reading a stored body needs write access to its organization and is audited ([admin-ui.md](admin-ui.md#security)) |
| Server-side request forgery | Client-controlled URL fetched by the gateway | The gateway only calls URLs from config: provider base URLs, MCP servers, webhooks (`https` only) and identity providers, plus the authorization server an operator-configured MCP server names in its metadata. No endpoint fetches a client-supplied URL |

## MCP-specific threats

Tool poisoning through malicious tool descriptions, silent changes to tool definitions after approval, token passthrough to upstream MCP servers, confused-deputy OAuth flows, and prompt injection through tool output. The controls are in [mcp.md](mcp.md#security).

## Secure defaults

- The gateway refuses to start with an admin token shorter than 32 characters.
- Body logging and per-user MCP credentials refuse to start without a 32-byte `secrets.encryption_key`. Body logging is off unless a policy turns it on.
- With no keys configured at all, the model API rejects every request; there is no open mode.
- TLS is expected to terminate at a load balancer or ingress; the gateway can also serve TLS directly (`tls.cert_file`, `tls.key_file`).
- Container image is distroless, runs as non-root, read-only root filesystem.

## Supply chain

Dependencies are kept few and reviewed. CI runs `govulncheck`. Releases are built reproducibly, signed with Sigstore cosign and published with an SBOM.

## Reporting

See [SECURITY.md](../SECURITY.md).
