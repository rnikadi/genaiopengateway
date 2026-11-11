# Documentation

| Doc | Contents |
| --- | --- |
| [Quick start](quickstart.md) | Zero-config start, the setup wizard, moving to production |
| [Provider presets and prices](catalog.md) | The catalog the setup wizard fills in from |
| [gogctl](gogctl.md) | The command line tool: tenants, keys, usage, configuration export and import |
| [Product scope](product-scope.md) | Users, v0.1 use cases, non-goals, quality bar |
| [Architecture](architecture.md) | Components, packages, request lifecycle |
| [Decisions](adr/README.md) | Architecture decision records |
| [API](api.md) | Model API, admin API, errors, headers |
| [Route reference](reference/routes.md) | Every route the gateway serves, generated from the handlers |
| [Benchmarks](benchmarks.md) | Added latency and throughput, method and results |
| [Configuration and data model](config.md) | YAML reference, database tables |
| [Security and threat model](security.md) | Assets, trust boundaries, STRIDE, defaults |
| [Provider adapters](providers.md) | Adapter interface, translation rules, conformance tests |
| [MCP gateway](mcp.md) | Virtual servers, tool pinning, auth, audit, MCP configuration |
| [Admin UI and SSO](admin-ui.md) | Pages, roles, OIDC sign-in, sessions, admin API changes |
| [Admin UI: configuration views](admin-ui-config.md) | Providers, models, guardrails, alerts and settings in the UI |
| [Playground](playground.md) | Calling a model as a team from the admin UI |
| [Images, audio, video and realtime](modalities.md) | Media endpoints, how they are metered and priced, and the rollout |
| [Batch and files](batch.md) | The Batch API through the gateway: checked uploads, ownership, charging once, the Jobs page |
| [Routing and failover](routing.md) | Strategies, retries, timeouts, circuit breakers and data residency |
| [Response cache](cache.md) | Exact and semantic caching per team: keys, guardrails, metering, the admin UI |
| [Admin UI: editing configuration](admin-ui-config-editing.md) | Providers, models, guardrails, MCP servers and webhooks stored through the admin API |
| [Admin UI: every setting in the UI](admin-ui-settings.md) | Gateway settings edited on the Settings page, forms for every object, and the check that keeps the UI complete |
| [Virtual keys](virtual-keys.md) | The Keys page, rotation with a grace period, and key owners |
| [Team provider credentials](team-credentials.md) | A team's own provider API keys, used for its requests instead of the shared ones |
| [Per-user MCP credentials](mcp-user-credentials.md) | Connecting each user's upstream account, token storage and refresh |
| [Body logging](body-logging.md) | Opt-in request and response logging, redaction, retention, access |
| [Alerts](alerts.md) | Budget thresholds and other events, webhooks and Slack |
| [Guardrails](guardrails.md) | PII, pattern, classifier and webhook checks on model and MCP calls |
| [Gemini, Vertex AI and Bedrock](cloud-providers.md) | Cloud adapters, credentials and translation rules |
| [Deploying to Kubernetes](deploy-kubernetes.md) | Installing and running the Helm chart |
| [Helm chart](helm.md) | Kubernetes deployment design |
| [Observability](observability.md) | Logs, metrics, traces, usage records |
| [Runbook](runbook.md) | Dashboard, alert rules, and what to do for each alert |
| [Releases](releasing.md) | Versioning, artifacts, support |
| [Roadmap](roadmap.md) | Phases and gates |
