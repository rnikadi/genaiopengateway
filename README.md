# GenAIOpenGateway

One open-source gateway for every GenAI model type and every MCP server, with enterprise features included and nothing behind a paid tier.

- **Models:** LLMs, embeddings, text-to-speech, speech-to-text, realtime audio, image and video generation, behind one OpenAI-compatible API plus native passthrough.
- **MCP and agents:** registry, virtual servers, OAuth 2.1, tool-level RBAC, audit.
- **Enterprise:** SSO, SCIM, RBAC, multi-tenancy, budgets, rate limits, guardrails, observability.

> **Status:** v0.3. Working today: the OpenAI-compatible API (chat completions, embeddings, responses) for OpenAI-compatible providers and for Claude by translation, plus native Anthropic `/v1/messages`; virtual models with fallback and streaming; organizations, teams and keys with model allow lists, rate limits and budgets; usage and cost in SQLite or Postgres; an audit log; Prometheus metrics and OpenTelemetry traces; an MCP gateway with virtual servers, tool pinning, call audit, OAuth 2.1 and per-user upstream credentials; an admin UI with single sign-on and per-organization roles; budget alerts and webhooks; and opt-in request body logging with redaction and encryption. Guardrails, more providers and a Helm chart follow on the [roadmap](docs/roadmap.md). Design docs are in [docs/](docs/README.md).

## Quick start

Download a binary from [releases](https://github.com/rnikadi/genaiopengateway/releases), or build from source with Go 1.26:

```
cp config.example.yaml config.yaml          # edit providers, models and prices
export OPENAI_API_KEY=sk-... ANTHROPIC_API_KEY=sk-ant-... GATEWAY_DEV_KEY=$(openssl rand -hex 24)
make run                                    # or: genaiopengateway -config config.yaml
```

Point any OpenAI SDK at `http://localhost:8080/v1`, or the Anthropic SDK at `http://localhost:8080`, with the gateway key:

```
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_DEV_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"chat-default","messages":[{"role":"user","content":"Hello"}]}'
```

With Docker:

```
docker run -p 8080:8080 -v $PWD/config.yaml:/etc/genaiopengateway/config.yaml:ro \
  -e OPENAI_API_KEY -e ANTHROPIC_API_KEY -e GATEWAY_DEV_KEY ghcr.io/rnikadi/genaiopengateway:0.3
```

For local development, Docker Compose builds the gateway from your checkout and runs it with Postgres, Valkey, an MCP server and optionally a local model, Jaeger and Prometheus:

```
cd deploy/compose && docker compose up -d
```

See [deploy/compose](deploy/compose/README.md).

Send `SIGHUP` to reload the config without dropping requests.

## Organizations, teams and keys

Enable `store` and `admin` in the config, then manage tenants over the [admin API](docs/api.md#admin-api):

```
ADMIN="Authorization: Bearer $GATEWAY_ADMIN_TOKEN"
ORG=$(curl -s -H "$ADMIN" localhost:8080/admin/v1/orgs -d '{"name":"acme"}' | jq -r .id)
TEAM=$(curl -s -H "$ADMIN" localhost:8080/admin/v1/orgs/$ORG/teams \
  -d '{"name":"search","allowed_models":["chat-default"],"budget":{"amount_usd":500,"period":"monthly"}}' | jq -r .id)
curl -s -H "$ADMIN" localhost:8080/admin/v1/teams/$TEAM/keys -d '{"name":"prod"}'   # returns the key once
curl -s -H "$ADMIN" "localhost:8080/admin/v1/usage?org_id=$ORG&group_by=team"
```

Every change is recorded in the audit log (`GET /admin/v1/audit`).

The same is available in the browser at `http://localhost:8080/ui/`: organizations, teams and keys, usage and spend, MCP tool approvals and the audit log. People sign in with your identity provider over OIDC, and roles (owner, organization admin, viewer) come from their groups; without SSO, the admin token signs in. See [docs/admin-ui.md](docs/admin-ui.md).

## MCP gateway

Put remote MCP servers behind the gateway and give each team a curated **virtual server**: chosen tools from one or more upstreams, upstream credentials held by the gateway, and the same keys, policies, rate limits and telemetry as models.

```yaml
mcp:
  servers:
    github:
      url: https://api.githubcopilot.com/mcp/
      headers: {Authorization: "Bearer ${GITHUB_MCP_TOKEN}"}
  virtual_servers:
    engineering:
      servers:
        - server: github
          tools: [search_code, get_file_contents]
```

Clients connect to `http://localhost:8080/mcp/engineering` with a gateway key, or with an access token from your identity provider when `mcp.oauth` is set. Tool definitions are pinned: a tool whose description changes after approval is hidden until an admin approves it again. Every call is recorded (`GET /admin/v1/mcp/calls`). Servers that act for a person, such as GitHub or Linear, can use each user's own account instead of a shared credential: the user connects it once from their MCP client, and the gateway keeps their token encrypted ([docs/mcp-user-credentials.md](docs/mcp-user-credentials.md)). See [docs/mcp.md](docs/mcp.md).

## Observability

Prometheus metrics are at `/metrics` (or on `metrics.listen`), one JSON log line is written per request, and traces are exported over OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. See [observability](docs/observability.md).

Licensed under Apache-2.0 with no enterprise edition: see the [charter](CHARTER.md). Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
