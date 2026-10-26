# Architecture decision records

| ADR | Decision |
| --- | --- |
| [0001](0001-api-surface.md) | OpenAI-compatible API as the contract, plus native passthrough |
| [0002](0002-request-model.md) | Typed request envelope with pass-through of unknown fields |
| [0003](0003-config-model.md) | YAML for infrastructure, database for runtime objects |
| [0004](0004-storage.md) | Postgres for clusters, SQLite for single node |
| [0005](0005-hot-state.md) | Valkey for shared counters, memory for single node |
| [0006](0006-tenancy.md) | Organization, team, key hierarchy |
| [0007](0007-metering.md) | Per-modality usage units and an editable price catalog |
| [0008](0008-plugins.md) | Go interfaces for built-ins, WebAssembly for third-party plugins |
| [0009](0009-policy.md) | Built-in RBAC plus Cedar for attribute-based policy |
| [0010](0010-config-sync.md) | Data plane serves from a local snapshot |
| [0011](0011-mcp-scope.md) | MCP gateway scope |
| [0012](0012-governance.md) | Governance |
| [0013](0013-mcp-termination.md) | Terminate MCP at the gateway with the official Go SDK |
| [0014](0014-mcp-auth.md) | MCP authentication: gateway keys and OAuth 2.1 resource server |
| [0015](0015-admin-ui.md) | Admin UI: a single-page client of the admin API, embedded in the binary |
| [0016](0016-operator-auth.md) | Operator sign-in with OIDC, server-side sessions and fixed roles |
| [0017](0017-user-credentials.md) | Per-user upstream MCP credentials through a gateway-run OAuth flow |
| [0018](0018-body-logging.md) | Opt-in body logging, redacted and encrypted in the store |
| [0019](0019-alerts.md) | Budget alerts detected on charge, delivered by signed webhooks |
| [0020](0020-guardrails.md) | Guardrails: built-in checks plus model and webhook checks, no WebAssembly yet |
| [0021](0021-cloud-providers.md) | Native Gemini, Vertex AI and Bedrock adapters with cloud credentials |
| [0022](0022-helm-chart.md) | A basic Helm chart without bundled databases |
| [0023](0023-hybrid-config.md) | Configuration objects stored through the admin API, merged into the file |
| [0024](0024-modalities.md) | Images, audio, video and realtime through the one model path |
| [0025](0025-batch.md) | Batch through gateway-checked files, charged once when a batch finishes |
| [0026](0026-response-cache.md) | An opt-in response cache per team, with semantic matching on one replica's vectors |
| [0027](0027-settings-in-the-ui.md) | Every runtime setting is edited in the admin UI, and a test keeps it so |
| [0028](0028-ui-wins-over-the-file.md) | A change made in the UI wins over the config file for named objects |

New ADRs copy [0000-template.md](0000-template.md).
