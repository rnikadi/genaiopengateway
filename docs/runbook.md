# Runbook

What to do when the gateway alerts. The rules are in [`deploy/helm/genaiopengateway/files/alerts.yaml`](../deploy/helm/genaiopengateway/files/alerts.yaml), and each one links to its section here. The Grafana dashboard ([`files/dashboard.json`](../deploy/helm/genaiopengateway/files/dashboard.json)) has a panel for each signal.

## Installing the rules and the dashboard

- **Helm with the Prometheus Operator**: set `prometheusRule.enabled: true` for the rules, and `grafanaDashboard.enabled: true` for a ConfigMap that the Grafana sidecar loads. That ConfigMap is labelled `grafana_dashboard: "1"`.
- **Docker Compose**: `docker compose --profile observability up -d` starts Prometheus with the rules and Grafana with the dashboard at `localhost:3000`.
- **Anything else**: add `alerts.yaml` to Prometheus `rule_files`, and import `dashboard.json` into Grafana. The dashboard asks for a Prometheus data source.

## Backups and upgrades

Back up before every upgrade, and restore to recover from a bad change or a lost database: see [backup.md](backup.md).

## First steps for any alert

1. Open the dashboard and narrow it to the time of the alert.
2. Read the gateway's logs for the same window. Every request is logged as one JSON line with `request_id`, `model`, `target`, `status`, `error_code` and `fallbacks`. A failed try on a target is logged as `target failed` with its status and error.
3. Check `/readyz` on each replica. A replica that cannot reach its store reports not ready.

## GatewayHighErrorRate

More than 5% of model requests have failed with a 5xx for 10 minutes.

- Look at **Errors by code**:
  - `all_targets_failed` means the providers are failing: go to [GatewayAllTargetsFailing](#gatewayalltargetsfailing).
  - 5xx responses without an `error_code` come from the upstream. A target's own errors reach the client when it is the last target tried, so check that target's provider.
- If one model accounts for the errors, check that model's targets on the **Models** page of the admin UI.
- If the errors started with a configuration change, roll it back. Find the change in the audit log (`GET /admin/v1/audit`, or the Audit page).

## GatewayAllTargetsFailing

Every target of a model is failing, so its requests get 502 or the last upstream error.

- Check the providers' status pages, and **Fallbacks by target and reason**:
  - `rate_limited` means the account's quota is spent.
  - `server_error` means the provider is having an outage.
  - `connection_error` means DNS, the network or egress rules are failing.
- Test one target directly from a replica, with its key, to tell a gateway problem from a provider one.
- To restore service, add a target on another provider to the model. Owners can do this on the Models page, and every replica picks it up within `store.refresh_interval`. See [routing](routing.md).
- If a provider key was revoked or rotated, update it on the provider's form.

## GatewayCircuitOpen

A target's [circuit breaker](routing.md#circuit-breaker) has been open for 15 minutes. Requests skip the target and go to its fallbacks, so users are still served, but by other targets, and possibly at another price or latency.

- Look at the target on the **Models** page: *Circuit open* shows how many failures in a row opened it, and when it will be probed again.
- Find the cause as for [GatewayAllTargetsFailing](#gatewayalltargetsfailing).
- When the provider recovers, the next probe closes the breaker by itself. Nothing needs to be reset.
- If the target is gone for good, remove it from the model so its failures stop costing probes.

## GatewayHighLatency

A target's p95 time to first byte has been over 10 seconds for 15 minutes.

- Compare it with other targets on **Upstream time to first byte**. If only one provider is slow, it is the provider.
- Long prompts and large `max_tokens` raise the time to first byte. The request spans carry `gen_ai.request.max_tokens` and `gen_ai.usage.input_tokens`, so you can see whether the traffic changed.
- To serve users from faster targets meanwhile:
  - Set a [per-target `timeout`](routing.md#when-the-gateway-moves-on) so slow tries move on.
  - Or use the `least_latency` strategy for the model.

## GatewayStaleSnapshot

A replica has not reloaded keys and configuration from the store for 5 minutes. It keeps serving with what it last loaded, so new keys, revocations and configuration changes do not reach it.

- Check the store: Postgres must be up and reachable from the replica, and its disk must not be full.
- Look for `loading keys from the store failed; serving the previous snapshot` in that replica's logs. The error after it says why.
- Restarting the replica forces a full load, but only once the store is reachable again.

## GatewayUsageWriteErrors

Usage records have been lost. Requests are still served, but their spend is missing from usage reports and budgets.

- Look for `writing records failed` in the logs, then check the store, as for [GatewayStaleSnapshot](#gatewaystalesnapshot).
- A failed write is retried every flush, holding up to ten batches in memory. Past that, the oldest records are dropped and counted here. Records still held when a replica stops are lost too, so fix the store before you restart replicas.

## GatewayUnpricedTraffic

A target served traffic for an hour without a price. Its spend counts as zero, so budgets and alerts miss it.

- Add the target's price on the model's form. [Provider presets and prices](catalog.md) has list prices for common models.

## GatewayMCPUpstreamDown

An upstream MCP server has failed its catalog refresh for 10 minutes. Its tools stay listed from the last good catalog, and calls to them fail.

- Check the server's URL and credentials on the MCP page, **Servers** tab.
- For servers with per-user sign-in, check that the server's OAuth endpoints are reachable.

## GatewayBodiesDropped

Request bodies chosen for [body logging](body-logging.md) are being dropped. Either the write queue is full, or writes to the store fail.

- When writes fail, check the store.
- When the queue is full, lower `body_logging.max_body_bytes`, or turn body logging off for high-volume teams.

## Chaos test

`test/chaos` checks the failover paths against fake upstreams that fail on purpose. Run it before changing routing, and after upgrading:

```sh
go test ./test/chaos -v
```

It covers four cases:

- An upstream that refuses connections.
- An upstream that answers 500, 429 or 503 with `Retry-After`.
- An upstream that stalls before sending headers.
- An upstream that recovers while its breaker is open.

For each one, it checks that clients are still served and that the circuit breaker opens and closes as documented.
