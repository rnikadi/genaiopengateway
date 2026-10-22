# Product scope

## Problem

Teams that use generative AI in production need one place to control access, spend, safety and visibility across many model providers and MCP servers. Existing gateways either cover only text LLMs well, or keep the controls enterprises need (SSO, RBAC, audit, advanced limits) in a paid edition.

GenAIOpenGateway is a single Apache-2.0 gateway for every GenAI modality and for MCP, with every feature in the open-source build.

## Users

| User | Needs |
| --- | --- |
| Application developer | One OpenAI-compatible endpoint and key; works with existing SDKs; streaming; no code changes when the provider changes |
| Platform / ML platform team | Run the gateway, add providers, define virtual models and fallbacks, issue keys to teams, see usage |
| Finance / team lead | Budgets per team, cost per team and model, alerts before limits are hit |
| Security and compliance | Keys never leave the gateway, audit trail, data retention control, guardrails, SSO |
| Agent builder | Governed access to MCP servers and tools with per-user credentials |

## Use cases for v0.1

1. A developer points the OpenAI SDK at the gateway and calls chat, embeddings or responses with a gateway key.
2. A platform team defines a virtual model `chat-default` that uses one provider and falls back to another on 429 or 5xx.
3. A platform team creates an organization and teams, issues keys per team, and limits which models each key may use.
4. A team lead sets a monthly USD budget and requests-per-minute and tokens-per-minute limits on a team or key; requests beyond them are rejected with a clear error.
5. An operator sees per-key, per-team and per-model usage and cost, Prometheus metrics, and OpenTelemetry traces.
6. Claude models are reachable through the same OpenAI-compatible API, and also through native `/v1/messages` passthrough.

## Non-goals for v0.1

- Image, audio, video and realtime endpoints (v0.8).
- SSO, SCIM, custom roles, admin UI (v0.5). v0.1 has a bootstrap admin token and an admin REST API.
- MCP gateway (v0.5).
- Guardrails beyond request size limits (v0.5).
- Semantic caching (v0.8).
- Prompt management, evaluations, fine-tuning orchestration. These belong in other tools.

## Quality bar

- Added latency under 5 ms at p50 for non-streaming requests, excluding upstream time.
- First streamed byte forwarded without buffering.
- No request-path dependency on the database: keys and limits are served from memory or Valkey.
- A gateway restart or rolling upgrade drops no in-flight request that finishes within the 30 second shutdown grace period.
- Every exported package has tests; CI runs gofmt, vet, staticcheck and race-enabled tests.

## Success for v0.1

- The OpenAI Python and Node SDKs work unchanged for chat (streaming and non-streaming), embeddings and responses.
- An Anthropic model works through `/v1/chat/completions`, including streaming and tool calls.
- Budgets and rate limits hold under concurrent load across two gateway replicas sharing Valkey.
- Install to first successful request in under five minutes with Docker.
