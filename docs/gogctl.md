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

`gogctl config export` writes the gateway's declarative state as YAML:

- `objects`: what is stored through the admin UI or API (providers, models, guardrails, MCP servers, virtual MCP servers, alert webhooks and streams);
- `orgs`: every organization by name, with its policy, its [Cedar policies](cedar.md) and its teams by name with their policies.

`gogctl config import FILE` makes the gateway match the file: it creates what is missing and updates what differs, providers before the models that name them and objects before the organizations whose policies name them. Keep the file in Git and import it from CI:

```
gogctl config export > gateway.yaml
gogctl config import gateway.yaml -dry-run    # prints what would change
gogctl config import gateway.yaml
```

```yaml
objects:
  providers:
    openai: {type: openai, base_url: "https://api.openai.com/v1", api_key: set}
  models:
    gpt-5-mini:
      targets: [{provider: openai, model: gpt-5-mini, price: {input_per_mtok: 0.25, output_per_mtok: 2}}]
orgs:
  acme:
    budget: {amount_usd: 500, period: monthly}
    cedar: |
      @id("allow-by-default")
      permit (principal, action, resource);
    teams:
      search: {allowed_models: [gpt-5-mini], rpm: 600}
      ads: {}
```

An organization or team takes the policy settings of the [admin API](api.md#admin-api): `allowed_models`, `allowed_mcp_servers`, `allowed_agents`, `rpm`, `tpm`, `budget`, `log_bodies`, `guardrails`, `cache` and `residency`. A setting the file leaves out is cleared, so the file is the whole policy. An organization without a `cedar` entry keeps its Cedar policies; an empty `cedar` removes them. Names match organizations without case, and teams within their organization exactly.

Keys are not part of the file: their secrets are shown once, when created. Create them with `gogctl keys create` or the admin UI.

Importing the same file twice changes nothing, except objects whose secrets are written out in the file: the gateway cannot compare those, so they are put again.

### Pruning

`-prune` also deletes what the file leaves out, in the sections the file has: stored objects (not those from the config file) when it has `objects`, and organizations, teams and Cedar policies when it has `orgs`. A file without an `orgs` section never deletes an organization. A team with active keys cannot be deleted, so revoke its keys first; the import stops at the first error. Run with `-dry-run` first.

Secrets are never exported: they read `set`. Importing `set` into the same gateway keeps the stored secret; for another gateway, replace it with the real value before importing. `-all` also exports objects that come from the config file.

Every change is checked like one from the admin UI, needs the owner role, and is recorded in the audit log.
