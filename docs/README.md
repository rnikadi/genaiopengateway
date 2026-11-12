# Documentation

| Doc | Contents |
| --- | --- |
| [Quick start](quickstart.md) | Zero-config start, the setup wizard, moving to production |
| [Provider presets and prices](catalog.md) | The catalog the setup wizard fills in from |
| [gogctl](gogctl.md) | The command line tool: tenants, keys, usage, declarative configuration export and import |
| [Terraform provider](terraform.md) | Organizations, teams, keys, Cedar policies and configuration objects from Terraform |
| [Product scope](product-scope.md) | Users, v0.1 use cases, non-goals, quality bar |
| [Architecture](architecture.md) | Components, packages, request lifecycle |
| [Decisions](adr/README.md) | Architecture decision records |
| [API](api.md) | Model API, admin API, errors, headers |
| [Route reference](reference/routes.md) | Every route the gateway serves, generated from the handlers |
| [Benchmarks](benchmarks.md) | Added latency and throughput, method and results |
| [Configuration and data model](config.md) | YAML reference, database tables |
| [Security and threat model](security.md) | Assets, trust boundaries, STRIDE, defaults |
| [Compliance guide](compliance.md) | SOC 2, ISO 27001, GDPR and EU AI Act control mapping, records kept, hardening checklist |
| [Provider adapters](providers.md) | Adapter interface, translation rules, conformance tests |
| [MCP gateway](mcp.md) | Virtual servers, tool pinning, auth, audit, MCP configuration |
| [MCP catalog](mcp-catalog.md) | Browsing virtual servers, requesting and approving team access |
| [Admin UI and SSO](admin-ui.md) | Pages, roles, OIDC sign-in, sessions, admin API changes |
| [Roles and permissions](roles.md) | Built-in and custom roles, permissions, team grants |
| [SAML sign-in](saml.md) | Signing operators in with a SAML 2.0 identity provider |
| [Identity providers](identity-providers.md) | Setup for Keycloak, Okta, Entra ID, Google and PingOne, and how each is tested |
| [Developer portal](developer-portal.md) | Developers creating and rotating their own keys, and requesting MCP access |
| [Why was this denied?](why-denied.md) | Which policy, limit, Cedar rule or guardrail refused a request, and what would change it |
| [Chargeback and FOCUS exports](chargeback.md) | CSV statements of spend per team, and the same usage in FinOps FOCUS columns for cost tools |
| [SCIM provisioning](scim.md) | Users and groups from Okta, Entra ID and others, deprovisioning |
| [Cedar policies](cedar.md) | Attribute-based rules per organization for models and MCP tools |
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
| [Encryption keys and KMS](secrets.md) | The encryption key, rotation, and unwrapping it with AWS KMS, Google Cloud KMS or Vault Transit |
| [Streams](streams.md) | Usage, audit, MCP call and guardrail records sent to a webhook or a file as they are written |
| [Alerts](alerts.md) | Budget thresholds and other events, webhooks and Slack |
| [Guardrails](guardrails.md) | PII, pattern, classifier and webhook checks on model and MCP calls |
| [Gemini, Vertex AI and Bedrock](cloud-providers.md) | Cloud adapters, credentials and translation rules |
| [Deploying to Kubernetes](deploy-kubernetes.md) | Installing and running the Helm chart |
| [Helm chart](helm.md) | Kubernetes deployment design |
| [Observability](observability.md) | Logs, metrics, traces, usage records |
| [Runbook](runbook.md) | Dashboard, alert rules, and what to do for each alert |
| [Backup and upgrades](backup.md) | Backing up and restoring the database, moving between SQLite and Postgres, upgrading |
| [Releases](releasing.md) | Versioning, artifacts, support |
| [Roadmap](roadmap.md) | Phases and gates |
