# Roadmap

| Version | Scope | Gate to move on |
| --- | --- | --- |
| v0.1 | OpenAI-compatible chat, embeddings, responses; Anthropic translation and passthrough; orgs, teams, keys; RPM, TPM, budgets; usage and cost; metrics and traces; SQLite and Postgres; Valkey | OpenAI SDKs work unchanged; limits hold across replicas; five-minute quick start |
| v0.2 | MCP gateway: virtual servers over remote MCP servers, tool allow lists and pinning, gateway keys and OAuth 2.1 for clients, rate limits, audit, metrics and traces | Official MCP SDK clients on 2025-06-18 and 2026-07-28 work through the gateway against real servers |
| v0.3 | Admin UI; OIDC sign-in and roles ([admin-ui.md](admin-ui.md)); per-user upstream MCP credentials ([mcp-user-credentials.md](mcp-user-credentials.md)); opt-in body logging with redaction ([body-logging.md](body-logging.md)); budget alerts and webhooks ([alerts.md](alerts.md)) | One organization running it in production |
| v0.5 | Guardrails on model and MCP calls, including tool output ([guardrails.md](guardrails.md)); native Gemini, Vertex AI and Bedrock adapters ([cloud-providers.md](cloud-providers.md)); a basic Helm chart ([helm.md](helm.md)); Cedar policies; SAML and SCIM if asked for | Guardrails add under 10 ms at p50 |
| v0.8 | Text to speech, speech to text, images, batch, realtime audio over WebSocket and WebRTC, async video jobs, semantic cache | Every modality passes conformance tests |
| v1.0 | Separate control plane with gRPC snapshot streaming, Helm chart and operator, Kubernetes CRDs, A2A, third-party security audit | Stable APIs and a published support policy |
