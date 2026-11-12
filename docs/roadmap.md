# Roadmap

## Released

| Version | Scope |
| --- | --- |
| v0.1 | OpenAI-compatible chat, embeddings, responses; Anthropic translation and passthrough; orgs, teams, keys; RPM, TPM, budgets; usage and cost; metrics and traces; SQLite and Postgres; Valkey |
| v0.2 | MCP gateway: virtual servers over remote MCP servers, tool allow lists and pinning, gateway keys and OAuth 2.1 for clients, rate limits, audit, metrics and traces |
| v0.3 | Admin UI; OIDC sign-in and roles ([admin-ui.md](admin-ui.md)); per-user upstream MCP credentials ([mcp-user-credentials.md](mcp-user-credentials.md)); opt-in body logging with redaction ([body-logging.md](body-logging.md)); budget alerts and webhooks ([alerts.md](alerts.md)) |
| v0.8 | The planned v0.5 and v0.8 scope: guardrails ([guardrails.md](guardrails.md)); Gemini, Vertex AI and Bedrock ([cloud-providers.md](cloud-providers.md)); a Helm chart ([deploy-kubernetes.md](deploy-kubernetes.md)); images, speech, transcription, video, realtime, batch ([modalities.md](modalities.md), [batch.md](batch.md)); response cache ([cache.md](cache.md)); every setting in the admin UI; a docs site, route reference and benchmarks |
| v0.10 | The planned v0.9 and v0.10 scope: zero-config start, a setup wizard, provider presets and a price catalog, an install script and `gogctl` ([quickstart.md](quickstart.md), [catalog.md](catalog.md), [gogctl.md](gogctl.md)); routing strategies, retries, timeouts, circuit breakers and data residency ([routing.md](routing.md)); streams ([streams.md](streams.md)); KMS and Vault ([secrets.md](secrets.md)); GenAI trace conventions, a Grafana dashboard, alerts and a [runbook](runbook.md); a chaos test |
| v0.11 | Enterprise identity: SAML sign-in ([saml.md](saml.md)), SCIM 2.0 provisioning ([scim.md](scim.md)), custom and per-team roles ([roles.md](roles.md)), Cedar policies ([cedar.md](cedar.md)), an MCP catalog with access requests ([mcp-catalog.md](mcp-catalog.md)), declarative tenants and a Terraform provider ([gogctl.md](gogctl.md), [terraform.md](terraform.md)), and a [compliance guide](compliance.md) |
| v1.0 | Every modality: Veo and Nova Reel video, ElevenLabs, Deepgram and AssemblyAI speech, Stability and fal images, Cohere and Mistral ([modalities.md](modalities.md)); Gemini Live and Nova Sonic relays; hosted stdio MCP servers ([mcp.md](mcp.md#hosted-stdio-servers)); A2A agents ([a2a.md](a2a.md)); control and data planes ([control-plane.md](control-plane.md)) and a Kubernetes operator ([operator.md](operator.md)); the [support policy](releasing.md#support) in force. The third-party audit is not done yet ([security-audit.md](security-audit.md)) |

## Next

Plans after 1.0 are made in the open, as RFCs in GitHub discussions and on the roadmap board. Candidates raised so far:

- The [third-party security audit](security-audit.md), as soon as it is funded.
- The A2A gRPC and HTTP+JSON bindings, and push notifications through the gateway.
- A watch-based operator, and CRDs for Cedar policy sets shared by organizations.
- Three maintaining organizations and a foundation proposal, as the [charter](https://github.com/rnikadi/genaiopengateway/blob/main/CHARTER.md) sets out.

## Community, in every phase

- Monthly releases on a fixed date ([releasing.md](releasing.md#cadence)), a public roadmap board, and RFCs as GitHub discussions.
- Co-maintainers from early adopters, listed in `.github/CODEOWNERS`; three maintaining organizations by 1.0, then a foundation proposal ([charter](https://github.com/rnikadi/genaiopengateway/blob/main/CHARTER.md)).
- Good first issues labelled for new contributors.
