# GenAIOpenGateway

One open-source gateway for every GenAI model type and every MCP server, with enterprise features included and nothing behind a paid tier.

- **Models:** LLMs, embeddings, text-to-speech, speech-to-text, realtime audio, image and video generation, behind one OpenAI-compatible API plus native passthrough.
- **MCP and agents:** registry, virtual servers, OAuth 2.1, tool-level RBAC, audit.
- **Enterprise:** SSO, SCIM, RBAC, multi-tenancy, budgets, rate limits, guardrails, observability.

> **Status:** v0.1 in development. Working today: OpenAI-compatible chat completions, embeddings and responses against any OpenAI-compatible provider; fallbacks and streaming; organizations, teams and keys with model allow lists through an admin API; usage and cost per request in SQLite or Postgres; and an audit log. Design docs are in [docs/](docs/README.md); see the [roadmap](docs/roadmap.md).

## Quick start

```
cp config.example.yaml config.yaml          # edit providers, models and prices
export OPENAI_API_KEY=sk-... ANTHROPIC_API_KEY=sk-ant-... GATEWAY_DEV_KEY=$(openssl rand -hex 24)
make run
```

Point any OpenAI SDK at `http://localhost:8080/v1` with the gateway key:

```
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_DEV_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"chat-default","messages":[{"role":"user","content":"Hello"}]}'
```

With Docker:

```
docker build -t genaiopengateway .
docker run -p 8080:8080 -v $PWD/config.yaml:/etc/genaiopengateway/config.yaml:ro \
  -e OPENAI_API_KEY -e ANTHROPIC_API_KEY -e GATEWAY_DEV_KEY genaiopengateway
```

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

Licensed under Apache-2.0 with no enterprise edition: see the [charter](CHARTER.md). Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
