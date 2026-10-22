# Roadmap

| Version | Scope | Gate to move on |
| --- | --- | --- |
| v0.1 | OpenAI-compatible chat, embeddings, responses; Anthropic translation and passthrough; orgs, teams, keys; RPM, TPM, budgets; usage and cost; metrics and traces; SQLite and Postgres; Valkey | OpenAI SDKs work unchanged; limits hold across replicas; five-minute quick start |
| v0.5 | SSO (OIDC, SAML), SCIM, roles, admin UI; MCP gateway; guardrail pipeline with Wasm plugins; opt-in body logging with redaction; Cedar policies | One organization running it in production |
| v0.8 | Text to speech, speech to text, images, batch, realtime audio over WebSocket and WebRTC, async video jobs, semantic cache | Every modality passes conformance tests |
| v1.0 | Separate control plane with gRPC snapshot streaming, Helm chart and operator, Kubernetes CRDs, A2A, third-party security audit | Stable APIs and a published support policy |
