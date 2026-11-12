# Third-party security audit

The [roadmap](roadmap.md) puts an independent security audit in the 1.0 phase. This page says what it covers and where it stands, and will link the report when there is one.

## Status

**Not yet performed.** Version 1.0.0 was released without a third-party audit. The project has no audit funding yet, and an audit needs an independent firm, so it cannot be done by the maintainers. The aim is an audit through the [OpenSSF](https://openssf.org) or [OSTIF](https://ostif.org) programs, which fund audits of open source projects, or by an adopter who commissions one.

Until then, the security work behind 1.0 is the project's own:

- A STRIDE [threat model](security.md), reviewed at each minor release.
- `govulncheck`, `staticcheck` and race-enabled tests in CI on every change.
- Signed releases with SBOMs ([releasing.md](releasing.md#verifying-a-release)).
- A [disclosure policy](https://github.com/rnikadi/genaiopengateway/blob/main/SECURITY.md) with response times.

## Scope

The scope offered to auditors, in priority order:

1. **Authentication and tenancy:** gateway keys, the admin token, OIDC and SAML sign-in, SCIM, roles and Cedar policies, and the scoping of every admin query to its organization.
2. **Secrets:** encryption of stored credentials and bodies, KMS and Vault, and redaction in logs and errors.
3. **Request handling:** the OpenAI and Anthropic translation, streaming, request size limits, and the WebSocket relays for realtime, Gemini Live and Nova Sonic.
4. **MCP:** OAuth 2.1 for clients, per-user upstream credentials, tool pinning, and hosted stdio servers.
5. **A2A:** task and context ownership, and the card rewriting.
6. **Control and data planes, and the operator:** the snapshot protocol, the cache file, and the operator's access to Secrets.
7. **Supply chain:** the release workflow, signatures and dependencies.

## Findings

Findings will be published here with their fixes, after the 90-day disclosure window in SECURITY.md. Fixes for findings rated high or critical are backported to every supported release ([support policy](releasing.md#support)).
