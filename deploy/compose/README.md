# Local development with Docker Compose

The local development setup. It runs GenAIOpenGateway, built from your checkout, on one machine with everything it can use: Postgres for tenants and usage, Valkey for shared rate limits, and the MCP reference server behind the MCP gateway. Optional profiles add a local model and observability. Not for production: passwords and tokens are fixed defaults.

## Start

From this directory:

```
cp .env.example .env        # optional: add provider keys
docker compose up -d        # builds the gateway from this checkout and starts everything
```

| Command | Adds |
|---|---|
| `docker compose up -d` | gateway on `localhost:8080`, Postgres, Valkey, MCP reference server |
| `docker compose --profile ollama up -d` | Ollama with a small model (`qwen2.5:0.5b`), served as `local-chat`, so no provider keys are needed |
| `docker compose --profile observability up -d` | Jaeger at `localhost:16686` and Prometheus at `localhost:9090` |

Profiles combine: `docker compose --profile ollama --profile observability up -d`. For traces, also set `OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318` in `.env`.

The gateway image is always built locally from this checkout and never pulled; Go module and build caches keep rebuilds to seconds. The first start downloads the other images, and the MCP server downloads its package from npm, so give it a minute. The Ollama profile downloads the model once (about 400 MB) into a volume.

## Try it

The defaults below match `.env.example`.

```
KEY="Authorization: Bearer gog-local-dev-key"

# A model: chat-default needs OPENAI_API_KEY or ANTHROPIC_API_KEY; local-chat needs the ollama profile.
curl -s localhost:8080/v1/chat/completions -H "$KEY" -H "Content-Type: application/json" \
  -d '{"model":"local-chat","messages":[{"role":"user","content":"Hello"}]}'

# The MCP gateway: the virtual server "demo" exposes three tools of the reference server.
curl -s localhost:8080/mcp/demo -H "$KEY" -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" -H "Mcp-Protocol-Version: 2025-06-18" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"everything_get-sum","arguments":{"a":2,"b":3}}}'

# The admin API.
ADMIN="Authorization: Bearer local-admin-token-change-me-0000000000"
curl -s localhost:8080/admin/v1/orgs -H "$ADMIN" -d '{"name":"acme"}'
curl -s localhost:8080/admin/v1/mcp/calls -H "$ADMIN"
```

Any MCP client can connect to `http://localhost:8080/mcp/demo` with the header `Authorization: Bearer gog-local-dev-key`.

## Admin UI and sign-in

Open `http://localhost:8080/ui/` and sign in with the admin token (`local-admin-token-change-me-0000000000` by default).

To try single sign-on and roles, add Dex, a local OpenID Connect provider:

```
docker compose -f compose.yaml -f compose.sso.yaml up -d
```

The gateway then uses [config.sso.yaml](config.sso.yaml), which is `config.yaml` plus an `sso` section, so copy any change to `config.yaml` there too. Sign in at `http://localhost:8080/ui/` as one of these users, all with the password `password`:

| User | Role |
|---|---|
| `owner@example.com` | owner: everything |
| `admin@example.com` | admin of the organization `acme`; create `acme` first, as owner |
| `viewer@example.com` | viewer of every organization |

Dex is at `http://dex.localhost:5556/dex`; browsers resolve `*.localhost` to your machine. The sign-in redirect uses port 8080, so keep `GATEWAY_PORT` at its default with this file.

The browser test in [test/e2e](../../test/e2e) runs against this setup.

## Change it

- **Config:** edit [config.yaml](config.yaml) (models, providers, MCP servers; see [docs/config.md](../../docs/config.md)), then `docker compose restart gateway`.
- **Code:** after a change, `docker compose up -d` rebuilds the gateway and restarts it.
- **Logs:** `docker compose logs -f gateway`.
- **Reset:** `docker compose down -v` removes containers and the Postgres and Ollama volumes.
