# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow [semantic versioning](docs/releasing.md).

## [Unreleased]

### Added

- A Docker Compose setup for local use in `deploy/compose`: the gateway with Postgres, Valkey and the MCP reference server, plus optional profiles for a local model through Ollama and for Jaeger and Prometheus.

## [0.2.0] - 2026-10-23

The MCP gateway: one governed endpoint for the MCP servers a company uses.

### Added

- MCP gateway: virtual MCP servers at `/mcp/{name}` that expose chosen tools, prompts and resources from upstream MCP servers over Streamable HTTP, with per-upstream name prefixes, tool allow lists, static upstream credentials, `allowed_mcp_servers` policies, rate limits on calls, and MCP metrics, logs and traces. Clients on MCP 2024-11-05 through 2026-07-28 are supported.
- MCP tool pinning against silent definition changes (`pin_tools: true | strict | false`), an audit record per MCP call, and admin endpoints for upstream status, tool approval and call records.
- OAuth 2.1 on MCP endpoints: the gateway validates JWT access tokens from the company's identity provider (JWKS discovery, audience per virtual server, required scopes), maps them to an organization and team, records the token subject, and publishes RFC 9728 protected resource metadata.

## [0.1.0] - 2026-10-22

The first release: an OpenAI-compatible gateway with tenants, keys, limits, budgets, usage and observability.

### Model API

- `/v1/chat/completions`, `/v1/embeddings`, `/v1/responses` and `/v1/models`, compatible with the OpenAI SDKs, streaming included.
- `/v1/messages` passthrough for the Anthropic SDKs.
- Virtual models with ordered fallback across targets on 429, 5xx and connection errors.
- Providers: OpenAI or any OpenAI-compatible API (such as vLLM or Ollama), and Anthropic, which also serves chat completions by translation, tool calls and streaming included.

### Tenancy and control

- Organizations, teams and keys over an admin REST API, with an audit log of every change.
- Model allow lists, requests and tokens per minute, and daily, monthly or total USD budgets at each level.
- Limits held in memory for one replica or in Valkey across replicas. Rate limits fail open and budgets fail closed by default.
- Keys are stored as SHA-256 hashes and served from memory, so the database is off the request path.
- Static keys in config for setups without a database.

### Usage and observability

- Usage and cost per request in SQLite or Postgres, queryable by org, team, key and model, with a retention period.
- Prometheus metrics, OpenTelemetry traces over OTLP with GenAI semantic conventions, and one JSON log line per request.

### Operations

- One static binary or a distroless container image for linux, darwin and windows on amd64 and arm64.
- Config reload on `SIGHUP` and graceful shutdown.
- Signed binaries and images with SBOMs.

[Unreleased]: https://github.com/rnikadi/genaiopengateway/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/rnikadi/genaiopengateway/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/rnikadi/genaiopengateway/releases/tag/v0.1.0
