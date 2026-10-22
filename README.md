# GenAIOpenGateway

One open-source gateway for every GenAI model type and every MCP server, with enterprise features included and nothing behind a paid tier.

- **Models:** LLMs, embeddings, text-to-speech, speech-to-text, realtime audio, image and video generation, behind one OpenAI-compatible API plus native passthrough.
- **MCP and agents:** registry, virtual servers, OAuth 2.1, tool-level RBAC, audit.
- **Enterprise:** SSO, SCIM, RBAC, multi-tenancy, budgets, rate limits, guardrails, observability.

> **Status:** v0.1 in development. Working today: OpenAI-compatible chat completions, embeddings and responses against any OpenAI-compatible provider, with gateway keys, per-key model allow lists, fallbacks, streaming and per-request usage and cost in logs. Design docs are in [docs/](docs/README.md); see the [roadmap](docs/roadmap.md).

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

Licensed under Apache-2.0 with no enterprise edition: see the [charter](CHARTER.md). Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
