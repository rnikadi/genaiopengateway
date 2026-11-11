# Roadmap

## Released

| Version | Scope |
| --- | --- |
| v0.1 | OpenAI-compatible chat, embeddings, responses; Anthropic translation and passthrough; orgs, teams, keys; RPM, TPM, budgets; usage and cost; metrics and traces; SQLite and Postgres; Valkey |
| v0.2 | MCP gateway: virtual servers over remote MCP servers, tool allow lists and pinning, gateway keys and OAuth 2.1 for clients, rate limits, audit, metrics and traces |
| v0.3 | Admin UI; OIDC sign-in and roles ([admin-ui.md](admin-ui.md)); per-user upstream MCP credentials ([mcp-user-credentials.md](mcp-user-credentials.md)); opt-in body logging with redaction ([body-logging.md](body-logging.md)); budget alerts and webhooks ([alerts.md](alerts.md)) |
| v0.8 | The planned v0.5 and v0.8 scope: guardrails ([guardrails.md](guardrails.md)); Gemini, Vertex AI and Bedrock ([cloud-providers.md](cloud-providers.md)); a Helm chart ([deploy-kubernetes.md](deploy-kubernetes.md)); images, speech, transcription, video, realtime, batch ([modalities.md](modalities.md), [batch.md](batch.md)); response cache ([cache.md](cache.md)); every setting in the admin UI; a docs site, route reference and benchmarks |

## Next

Each phase ends at a gate that can be checked, and the next starts when it passes. Ease of use comes before new features, because the features already exceed what most users will find.

| Phase | Scope | Gate |
| --- | --- | --- |
| Easy to use (v0.9) | Zero-config start (SQLite, in-memory limits, generated admin token and encryption key); a setup wizard in the admin UI; provider presets and a versioned model price catalog; `docker run` without a mounted file and an install script; a `gogctl` CLI over the admin API; empty-state guides | A new user gets a first completion through the gateway in under 5 minutes without writing YAML |
| Reliable at scale (v0.10) | Weighted, round-robin, least-latency, lowest-cost and sticky routing; retries with backoff and jitter, per-target timeouts, circuit breakers and recovery probes; region and data-residency pinning; audit and usage streaming; KMS and Vault for encryption keys; a chaos test in CI | A provider killed mid-load costs no failed requests when another target exists |
| Enterprise identity (v0.11) | SAML sign-in, SCIM 2.0 provisioning, custom and per-team roles, Cedar policies (ADR 0009), an MCP catalog with requests and approvals, declarative config import, a compliance guide | Okta, Entra ID and Keycloak sign-in and provisioning work end to end |
| Every modality and v1.0 | Native video (Veo, Nova Reel), speech (ElevenLabs, Deepgram, AssemblyAI), images (Stability, fal), Cohere and Mistral; Gemini Live and Nova Sonic relays; hosted stdio MCP servers; A2A; a separate control plane with an operator and CRDs (ADR 0010); a third-party security audit | Stable APIs, the [support policy](releasing.md#support) in force, every modality passing conformance tests |

## Community, in every phase

- Monthly releases on a fixed date ([releasing.md](releasing.md#cadence)), a public roadmap board, and RFCs as GitHub discussions.
- Co-maintainers from early adopters, listed in `.github/CODEOWNERS`; three maintaining organizations by 1.0, then a foundation proposal ([charter](https://github.com/rnikadi/genaiopengateway/blob/main/CHARTER.md)).
- Good first issues labelled for new contributors.
