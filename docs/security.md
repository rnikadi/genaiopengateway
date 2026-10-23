# Security and threat model

This is the threat model as of v0.2. It is reviewed at every minor release.

## Assets

1. Upstream provider credentials.
2. Gateway API keys and the admin token.
3. Prompts and completions, which may contain personal or confidential data.
4. Usage and cost records, and the audit log.
5. Availability of the gateway, which sits in the path of every AI call.

## Trust boundaries

- **Client to gateway:** untrusted. Every request is authenticated with a gateway key.
- **Operator to gateway:** trusted, authenticated with the admin token (SSO in v0.3).
- **Gateway to provider:** the provider is trusted with the request content it receives, but its responses are treated as untrusted input.
- **Gateway to store and Valkey:** trusted network; credentials from environment.

## Threats and mitigations (STRIDE)

| Threat | Example | Mitigation |
| --- | --- | --- |
| Spoofing | Guessing or stealing a gateway key | 238-bit random keys; constant-time hash lookup; keys revocable and expirable; keys never logged (only the prefix) |
| Spoofing | Client sends its own upstream key to reach a provider directly | Client `Authorization` and `x-api-key` headers are never forwarded upstream; the gateway sets upstream credentials itself |
| Tampering | Client sets `model` to an upstream model name to bypass allow lists | `model` must name a configured virtual model; unknown names are rejected |
| Tampering | Config edited to point a provider at an attacker host | Config file is operator-owned; reloads are logged with a content hash; v0.5 audit covers config changes |
| Repudiation | An admin deletes a team and denies it | Every admin write is recorded in `audit_log` with actor, action, resource and before/after |
| Information disclosure | Provider keys leaked in logs, errors or responses | Secrets are typed so they redact themselves when logged; upstream error bodies pass through but upstream request headers never do |
| Information disclosure | Prompts stored in logs | Request and response bodies are not logged in v0.1. Body logging arrives in v0.3 as opt-in with redaction and retention |
| Information disclosure | Key from one org reads another org's data | Every admin query is scoped by `org_id`; usage queries filter by the caller's scope |
| Denial of service | Huge bodies or slow clients exhaust memory | `max_request_bytes`, header read timeout, idle timeout, upstream timeouts |
| Denial of service | One team floods a shared provider quota | Per-key, per-team and per-org RPM and TPM limits; budgets |
| Denial of service | Store outage stops traffic | Request path serves from an in-memory snapshot (ADR 0004) |
| Elevation of privilege | Model key used against the admin API | Admin API accepts only the admin token; model keys are rejected |
| Server-side request forgery | Client-controlled URL fetched by the gateway | The gateway only calls configured provider base URLs; no endpoint fetches client-supplied URLs in v0.1 |

## MCP-specific threats

Tool poisoning through malicious tool descriptions, silent changes to tool definitions after approval, token passthrough to upstream MCP servers, confused-deputy OAuth flows, and prompt injection through tool output. The controls are in [mcp.md](mcp.md#security).

## Secure defaults

- The gateway refuses to start with an admin token shorter than 32 characters.
- With no keys configured at all, the model API rejects every request; there is no open mode.
- TLS is expected to terminate at a load balancer or ingress; the gateway can also serve TLS directly (`tls.cert_file`, `tls.key_file`).
- Container image is distroless, runs as non-root, read-only root filesystem.

## Supply chain

Dependencies are kept few and reviewed. CI runs `govulncheck`. Releases are built reproducibly, signed with Sigstore cosign and published with an SBOM.

## Reporting

See [SECURITY.md](../SECURITY.md).
