# Control and data planes

By default every replica is both control plane and data plane: it holds the database credentials, serves the admin API and UI, and serves model traffic. From v1.0 you can split them ([ADR 0034](adr/0034-control-and-data-planes.md)):

- **Control plane** replicas have the store, serve the admin API and UI, and hand out snapshots to data planes.
- **Data plane** replicas have no store and no admin API. They serve model, MCP, realtime and A2A traffic from a snapshot they read from a control plane, and send their usage records back to it.

Use the split to run replicas in other regions or at the edge without database credentials, or to keep serving through a database or control plane outage, restarts included.

## Configuration

On the control plane, add a token for data planes:

```yaml
store: {driver: postgres, dsn: "${DATABASE_URL}"}
secrets: {encryption_key: "${GATEWAY_ENCRYPTION_KEY}"}
data_planes:
  token: ${GATEWAY_DATA_PLANE_TOKEN}   # at least 32 characters
```

On each data plane, point at the control plane instead of a store:

```yaml
control_plane:
  url: https://gateway-cp.internal.example.com
  token: ${GATEWAY_DATA_PLANE_TOKEN}
  cache_file: /var/lib/gateway/snapshot.json   # keeps serving across restarts while the control plane is down
secrets: {encryption_key: "${GATEWAY_ENCRYPTION_KEY}"}   # the same key as the control plane
limiter: {driver: valkey, address: "valkey:6379"}        # share limits and budgets with every plane
providers: { ... }                                      # as in the control plane's file
models: { ... }
```

The data plane's file holds providers, models and gateway-wide settings like any config file. Objects stored through the admin API or UI (providers, models, guardrails, MCP servers, A2A agents and the rest) come from the control plane and win over the file, as on any replica. Gateway-wide settings changed in the UI apply only to replicas with a store; set them in each data plane's file.

## How it works

- Every `store.refresh_interval` (5 seconds by default) the data plane asks `GET /cp/v1/snapshot` for a new version. An unchanged snapshot is a `304`.
- The snapshot holds stored configuration objects, organizations, teams, unrevoked keys (as hashes), teams' provider credentials and Cedar policies. Secrets in it stay encrypted with `secrets.encryption_key`, so both planes need the same key. Key secrets are never in it, only their hashes.
- Each new snapshot is written to `cache_file` with mode 0600. A data plane that starts while the control plane is unreachable loads it and serves; `/readyz` reports not ready until the data plane holds a snapshot from one or the other.
- Usage, MCP call and guardrail records are batched and posted to `POST /cp/v1/records/{kind}`, which stores them and publishes them to [streams](streams.md). Failed batches are retried; while the control plane is down, records queue up to the writer's bound and then the oldest are dropped, counted in `gog_usage_write_errors_total`.
- Key revocation and policy changes reach data planes within one refresh interval.

## What needs a store

These features read or write the store while serving a request, so data planes refuse them as a replica without a database does. Route them to replicas with a store:

- Video jobs, files and batches (`/v1/videos`, `/v1/files`, `/v1/batches`)
- A2A calls that name a task or context
- Body logging
- MCP servers with `user_auth`, MCP OAuth callers and `pin_tools: strict`
- The admin API, UI, SSO and SCIM

## Security

The data plane token is a shared secret with read access to every key hash and sealed secret in the store, and write access to usage records. Keep it in a secrets manager, use HTTPS between planes, and rotate it by setting the new token on the control plane and the data planes together.
