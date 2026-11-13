# Compliance guide

This guide is for the people who have to show an auditor how AI traffic is controlled: security, risk and compliance teams, and the platform team that runs the gateway. It maps the gateway's controls to the frameworks most often asked about, lists the evidence each control produces, and ends with a hardening checklist.

The gateway is software you run, not a service, so it holds no certification of its own. What it gives you is a set of controls and records that support your own SOC 2, ISO 27001, GDPR or EU AI Act work. Whether a control is enough is a decision for you and your auditor.

## What the gateway records

| Record | Contents | Kept for | Where |
| --- | --- | --- | --- |
| Audit log | Every admin write and every read of a logged body: actor, action, resource, organization, before and after | Forever; it is never pruned | `GET /admin/v1/audit`, the **Audit** page, [streams](streams.md) |
| Usage | One row per model call: key, team, organization, model, provider, tokens, cost, status, latency | `store.usage_retention` (omit to keep forever) | `GET /admin/v1/usage`, streams |
| MCP calls | One row per tool call: caller, server, tool, status, latency. Never arguments or results | `store.usage_retention` | `GET /admin/v1/mcp/calls`, streams |
| Guardrail events | Which guardrail ran, at what stage, what it did, counts by kind. Never the matched text | `store.usage_retention` | `GET /admin/v1/guardrail-events` |
| Request bodies | Only where a policy turns on `log_bodies`: redacted, encrypted, size-bounded | `body_logging.retention` (default 30 days, at most 365) | `GET /admin/v1/requests`, audited on every read |

Bodies are off by default. Without body logging the gateway stores who called what, when and at what cost, never the content. The table schemas are in [config.md](config.md#data-model).

## Controls by framework

### SOC 2 (Trust Services Criteria) and ISO/IEC 27001:2022

| Criterion or control | What the gateway does | Evidence |
| --- | --- | --- |
| CC6.1, A.5.15 access control | Clients need a gateway key per application or team. Upstream provider keys never leave the gateway. Operators sign in with OIDC or SAML and get roles scoped to an organization or team ([roles.md](roles.md)). | Key list with owners and expiry, role mappings, `GET /admin/v1/me` |
| CC6.2, A.5.16 identity lifecycle | SCIM provisioning creates and removes operators from your directory, and roles are resolved again on every request, so removing someone from a group takes effect at once ([scim.md](scim.md)). | `scim.*` audit entries, the **Directory** page |
| CC6.3, A.5.18 least privilege | Custom and per-team roles, model and MCP server allow lists per organization, team and key, and Cedar policies for rules lists cannot express ([cedar.md](cedar.md)). | Policy export (`gogctl export`), Cedar versions |
| CC6.6, A.8.20 network boundaries | The gateway only calls URLs from its configuration; no endpoint fetches a client-supplied URL ([security.md](security.md)). | Configuration file under change control |
| CC6.7, A.8.24 cryptography | Logged bodies and users' MCP tokens are sealed with AES-256-GCM, with the key in KMS if you choose ([secrets.md](secrets.md)). TLS at the gateway or the load balancer. | `secrets` configuration, KMS key policy |
| CC7.2, A.8.15 logging | The audit log covers every admin change, and streams copy audit and usage records to your SIEM in near real time ([streams.md](streams.md)). | SIEM index, stream delivery metrics |
| CC7.3, A.8.16 monitoring | Prometheus metrics, OpenTelemetry traces, and alerts for budget thresholds, expiring keys and pending MCP tool changes ([observability.md](observability.md), [alerts.md](alerts.md)). | Dashboards, alert history |
| CC8.1, A.8.32 change management | Configuration can be managed as code with `gogctl import` or the Terraform provider and reviewed in pull requests ([gogctl.md](gogctl.md#configuration-as-code), [terraform.md](terraform.md)). Reloads are logged with a content hash, and changes made in the UI are audited. | Repository history, plan output, audit entries |
| CC9.2, A.5.19 to A.5.22 supplier risk | Every request names a virtual model that maps to approved providers; residency labels keep traffic with providers in a region ([routing.md](routing.md#data-residency)). The MCP catalog lists approved tool servers, and teams request access through an approval flow ([mcp-catalog.md](mcp-catalog.md)). | Provider list, residency settings, `mcp_request.*` audit entries |
| A1.2 availability | Health checks, retries, failover and circuit breakers across providers; the request path keeps serving from memory if the store is down. | Routing configuration, [runbook.md](runbook.md) |
| A.8.8 vulnerabilities, A.5.21 supply chain | CI runs `govulncheck`. Releases are signed with cosign and ship an SBOM and SLSA provenance ([releasing.md](releasing.md)). | Signature verification output, SBOM |

### GDPR and other privacy laws

| Article | What the gateway does |
| --- | --- |
| Art. 5(1)(c) data minimisation | Content is not stored unless a policy asks for it. The PII guardrail can redact email addresses, phone numbers, card numbers, IBANs, US SSNs and secrets before a prompt leaves your network ([guardrails.md](guardrails.md)). |
| Art. 5(1)(e) storage limitation | Bodies expire after `body_logging.retention`; usage, MCP call and guardrail rows after `store.usage_retention`. |
| Art. 17 erasure | `DELETE /admin/v1/requests?org_id=…&before=…` removes logged bodies early, and the deletion is audited ([body-logging.md](body-logging.md)). Usage rows hold key and team identifiers, not personal data, unless your applications pass a user identifier. |
| Art. 25 privacy by design | Body logging is off by default and refuses to start without an encryption key. Redaction runs before anything is written. |
| Art. 28 processors | The gateway gives you one list of the model and tool providers that receive data, which is the list your records of processing and data processing agreements need to cover. |
| Art. 30 records of processing | Usage records show which providers received traffic from which teams, and when. |
| Art. 32 security of processing | Encryption at rest for content, access control, audit and the threat model in [security.md](security.md). |
| Art. 44 to 49 transfers | Residency labels on providers, and `residency` on an organization, team or key, keep traffic in a region. Requests that cannot be served there are refused with `403 residency_not_allowed` before anything is sent. |

### EU AI Act

The gateway is infrastructure between your applications and general-purpose AI models. It is not itself an AI system under the Act, but it helps deployers meet their obligations:

- **Logging (Art. 12 and 26(6)).** Usage records show which model served each request and when. Where you need content too, turn on body logging for the teams that run the affected system and set a retention that matches your obligation.
- **Human oversight (Art. 14).** MCP tool calls can be restricted per team and per tool, Cedar policies can deny tools outside working hours or from unknown networks, and new or changed tool definitions wait for an operator's approval before agents can call them ([mcp.md](mcp.md#security)).
- **An inventory of AI use.** The usage page answers which teams use which models, through which providers, and at what volume.

### Other regimes

- **HIPAA.** Keep traffic with providers you have a business associate agreement with: give those providers a residency label such as `baa` and set `residency: [baa]` on the teams that handle protected health information. Leave body logging off for those teams, or set a retention that fits your policy. The gateway does not store PHI unless body logging is on.
- **PCI DSS.** The PII guardrail with `credit_card` set to `block` on the `input` stage stops card numbers from reaching a provider. Redaction for body logging covers card numbers by default.

## Hardening checklist

Before an audit, check that a production gateway has:

1. **Operator SSO.** OIDC or SAML sign-in with role mappings from directory groups, and the admin token kept in a secrets manager for break-glass use only ([admin-ui.md](admin-ui.md), [saml.md](saml.md)).
2. **SCIM** provisioning, so leavers lose access when the directory removes them.
3. **Keys with owners and expiry.** One key per application, each with an expiry date and the `key.expiring` alert on.
4. **Allow lists** for models and MCP servers on every organization, and Cedar policies for anything time or network based.
5. **Guardrails.** A PII guardrail on the input stage for teams that handle personal data.
6. **Residency** labels on every provider, and `residency` on organizations with transfer restrictions.
7. **Encryption.** `secrets.encryption_key` from KMS or a secrets manager, rotated on a schedule ([secrets.md](secrets.md)).
8. **Retention.** `store.usage_retention` and `body_logging.retention` set to your retention policy.
9. **A SIEM stream** with `records: [audit, usage]`, so records live outside the gateway's own database.
10. **Configuration as code.** Organizations, teams and policies kept in a repository and applied with `gogctl import` or Terraform, so every change has a review.
11. **Verified images.** Releases checked with `cosign verify` before deployment, and the SBOM fed to your vulnerability scanner.
12. **Postgres with backups** for the store, and the store's credentials limited to the gateway.

## Answering common questionnaire items

- **Is customer data used to train models?** The gateway does not train anything. Whether a provider trains on your data depends on your contract with that provider.
- **Where is data stored?** In the database you run the gateway with, in the region you deploy it. Content is stored only when body logging is on.
- **Who can see prompts?** Nobody, unless body logging is on for that scope. Then only owners and operators with `bodies.read` for that organization, and each read is in the audit log.
- **How are provider keys protected?** They are read from the environment or a secrets manager, never returned by the API or written to logs, and never sent to clients.
- **How fast is access revoked?** Key revocation reaches every replica within the store's `refresh_interval` (5 seconds by default). Operator roles are checked on every request.
