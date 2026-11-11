# gogctl

`gogctl` manages a gateway from the command line over the [admin API](api.md#admin-api): organizations, teams and keys, usage, the provider catalog, and the configuration objects stored through the admin UI. It ships in every release archive beside the gateway, and the [install script](quickstart.md) installs both.

```
export GOG_URL=https://gateway.example.com GOG_ADMIN_TOKEN=...
gogctl orgs create acme
gogctl teams create <org-id> search
gogctl keys create <team-id> prod          # prints the key once
gogctl keys rotate <key-id> 24h            # the old key works for 24 hours
gogctl usage -group-by team
```

`-o json` prints the API's JSON instead of a table, for scripts.

## Configuration as code

`gogctl config export` writes the objects stored through the admin UI or API (providers, models, guardrails, MCP servers, virtual MCP servers and alert webhooks) as YAML, and `gogctl config import FILE` creates or replaces each of them, providers first so models can name them. Keep the file in Git and import it from CI to manage the gateway declaratively:

```
gogctl config export > gateway-objects.yaml
gogctl config import gateway-objects.yaml -dry-run
gogctl config import gateway-objects.yaml
```

```yaml
objects:
  providers:
    openai: {type: openai, base_url: "https://api.openai.com/v1", api_key: set}
  models:
    gpt-5-mini:
      targets: [{provider: openai, model: gpt-5-mini, price: {input_per_mtok: 0.25, output_per_mtok: 2}}]
```

Secrets are never exported: they read `set`. Importing `set` into the same gateway keeps the stored secret; for another gateway, replace it with the real value before importing. `-all` also exports objects that come from the config file.

Every change is checked like one from the admin UI, needs the owner role, and is recorded in the audit log.
