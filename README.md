# GenAIOpenGateway

**The GenAI gateway with no enterprise edition.** One open-source gateway for every model type and every MCP server, with SSO, budgets, guardrails and audit included, under Apache-2.0, with no paid tier and no license keys ([charter](CHARTER.md)).

[Documentation](https://rnikadi.github.io/genaiopengateway/) · [Releases](https://github.com/rnikadi/genaiopengateway/releases) · [Benchmarks](docs/benchmarks.md) · [Roadmap](docs/roadmap.md)

| | What you get |
| --- | --- |
| **Every model type** | Chat, Responses and embeddings in OpenAI's shape, Anthropic `/v1/messages` and Gemini `generateContent` natively; images, text to speech, speech to text, moderation, rerank, video jobs, batches and realtime audio over WebSocket |
| **Every provider** | OpenAI and anything OpenAI-compatible (Azure OpenAI, vLLM, Ollama, LocalAI and more), Anthropic, Gemini, Vertex AI and Amazon Bedrock, with virtual models and fallback |
| **MCP gateway** | Virtual MCP servers with curated tools, tool pinning against silent changes, OAuth 2.1, per-user upstream credentials and a record of every call |
| **Governance** | Organizations, teams and virtual keys with rotation; model allow lists, RPM, TPM and budgets at every level; guardrails for PII, patterns, classifiers and webhooks; redacted, encrypted body logging; an audit log; alerts |
| **Cost** | Usage and cost per key, team, model and provider for tokens, images, audio and video; exact and semantic response caching |
| **Admin UI** | Every setting and object editable in the browser, a playground, OIDC single sign-on with owner, admin and viewer roles |
| **Operations** | One Go binary, SQLite or Postgres, Valkey for shared limits; Prometheus, OpenTelemetry, a hardened Helm chart, signed releases with SBOMs and SLSA provenance |
| **Fast** | About 2 ms added at p50 and 12,800 requests a second on 4 vCPUs ([benchmarks](docs/benchmarks.md)) |

> **Status:** v0.8. Everything above works today and is covered by tests, an OpenAI SDK conformance run, a browser test of the admin UI and a Kubernetes install in CI. Next on the [roadmap](docs/roadmap.md): a zero-config start with a setup wizard, load balancing and circuit breakers, SAML, SCIM and Cedar policies, and more native media providers on the way to 1.0.

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
  -e OPENAI_API_KEY -e ANTHROPIC_API_KEY -e GATEWAY_DEV_KEY ghcr.io/rnikadi/genaiopengateway:0.8
```

For local development, Docker Compose builds the gateway from your checkout and runs it with Postgres, Valkey, an MCP server and optionally a local model, Jaeger and Prometheus:

```
cd deploy/compose && docker compose up -d
```

See [deploy/compose](deploy/compose/README.md).

On Kubernetes, install the Helm chart with your config; it runs two hardened replicas against your own Postgres and Valkey:

```
helm install gateway oci://ghcr.io/rnikadi/charts/genaiopengateway -f values.yaml
```

See [Deploying to Kubernetes](docs/deploy-kubernetes.md).

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
