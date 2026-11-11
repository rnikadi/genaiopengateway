# Quick start

The gateway runs without a config file. It starts on SQLite with in-memory limits, generates an admin token and an encryption key, and prints where to sign in. A setup wizard in the admin UI then adds a provider, creates a first key and sends a test request, so the first completion takes a few minutes and no YAML.

## Start it

With Docker:

```
docker run -p 8080:8080 -v gog-data:/var/lib/genaiopengateway ghcr.io/rnikadi/genaiopengateway:latest
```

With the install script (Linux and macOS), which checks the download against the release's checksums:

```
curl -fsSL https://raw.githubusercontent.com/rnikadi/genaiopengateway/main/scripts/install.sh | sh
genaiopengateway
```

Or from source with Go 1.26: `go run ./cmd/gateway`.

The first start prints:

```
GenAIOpenGateway 0.9.0 is running with no config file.

  Admin UI:     http://localhost:8080/ui/
  Admin token:  3f9c…  (also saved in data/bootstrap.json)
  Data:         data (SQLite, in-memory limits)
```

The token is printed only on the start that generated it. Later starts reuse it from `bootstrap.json`; in Docker it lives in the `gog-data` volume (`docker logs` shows the first start's banner).

## Set it up

Open the Admin UI, sign in with the token, and follow **Set up your gateway** on the Overview (or **Setup wizard** in the navigation):

1. **Provider.** Pick a preset: OpenAI, Anthropic, Gemini, Mistral, Groq, DeepSeek, OpenRouter, Azure OpenAI, Bedrock, Vertex AI, Ollama or vLLM.
2. **Connect.** Paste an API key (stored encrypted, never shown again) and choose models. Presets fill in the endpoint and each model's list price from the [catalog](catalog.md); other model ids, such as Azure deployments or Ollama models, can be typed in.
3. **Team and key.** Creates an organization, a team and a virtual key.
4. **Test.** Sends a chat completion as the team, through the same path as an application.
5. **Use it.** Shows the key once, with snippets for curl, the OpenAI Python and Node SDKs, and MCP clients.

Everything the wizard creates is ordinary configuration: providers and models on the Configuration page, the organization, team and key on the Organizations and Virtual keys pages.

CI runs this whole flow in a browser on every pull request (`test/e2e/quickstart.sh`).

## Where the data lives

| Setting | Default | Change with |
| --- | --- | --- |
| Data directory | `./data`, or `/var/lib/genaiopengateway` in the image | `-data` or `GOG_DATA_DIR` |
| Database | SQLite in the data directory | a config file with `store` |
| Admin token and encryption key | generated into `bootstrap.json` (mode 0600) | a config file with `admin.token` and `secrets.encryption_key` |
| Rate limits and budgets | in memory, so one replica | a config file with `limiter: {driver: valkey}` |

Back up `bootstrap.json` with the database: the encryption key in it decrypts the provider keys stored there.

## Moving to production

A zero-config start is for one instance. For several replicas, single sign-on, Postgres or Valkey, write a config file (start from [`config.example.yaml`](../config.example.yaml), reference: [Configuration](config.md)) and start with `-config`. To keep what the wizard created, point the config file at the same database and copy the `admin_token` and `encryption_key` from `bootstrap.json` into it:

```yaml
admin: {token: ${GATEWAY_ADMIN_TOKEN}}
store: {driver: sqlite, dsn: "file:data/gateway.db"}
secrets: {encryption_key: ${GATEWAY_ENCRYPTION_KEY}}
```

When the file named by `-config` (default `config.yaml`) does not exist, the gateway logs a warning and starts with zero config, so check the path if you expected your file to be read.
