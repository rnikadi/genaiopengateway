# Documentation

| Doc | Contents |
| --- | --- |
| [Product scope](product-scope.md) | Users, v0.1 use cases, non-goals, quality bar |
| [Architecture](architecture.md) | Components, packages, request lifecycle |
| [Decisions](adr/README.md) | Architecture decision records |
| [API](api.md) | Model API, admin API, errors, headers |
| [Configuration and data model](config.md) | YAML reference, database tables |
| [Security and threat model](security.md) | Assets, trust boundaries, STRIDE, defaults |
| [Provider adapters](providers.md) | Adapter interface, translation rules, conformance tests |
| [MCP gateway](mcp.md) | Virtual servers, tool pinning, auth, audit, MCP configuration |
| [Admin UI and SSO](admin-ui.md) | Pages, roles, OIDC sign-in, sessions, admin API changes |
| [Admin UI: configuration views](admin-ui-config.md) | Providers, models, guardrails, alerts and settings in the UI |
| [Playground](playground.md) | Calling a model as a team from the admin UI |
| [Admin UI: editing configuration](admin-ui-config-editing.md) | Providers, models, guardrails, MCP servers and webhooks stored through the admin API |
| [Per-user MCP credentials](mcp-user-credentials.md) | Connecting each user's upstream account, token storage and refresh |
| [Body logging](body-logging.md) | Opt-in request and response logging, redaction, retention, access |
| [Alerts](alerts.md) | Budget thresholds and other events, webhooks and Slack |
| [Guardrails](guardrails.md) | PII, pattern, classifier and webhook checks on model and MCP calls |
| [Gemini, Vertex AI and Bedrock](cloud-providers.md) | Cloud adapters, credentials and translation rules |
| [Deploying to Kubernetes](deploy-kubernetes.md) | Installing and running the Helm chart |
| [Helm chart](helm.md) | Kubernetes deployment design |
| [Observability](observability.md) | Logs, metrics, traces, usage records |
| [Releases](releasing.md) | Versioning, artifacts, support |
| [Roadmap](roadmap.md) | Phases and gates |
