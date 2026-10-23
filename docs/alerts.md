# Budget alerts and webhooks

Budgets in v0.1 stop requests once spent, which is the first moment anyone hears about them. Finance and team leads want a warning at 50% or 80% while there is still time to act. This design adds alerts at budget thresholds, plus a few other events worth knowing about, delivered to webhooks and Slack. Decisions are recorded in [ADR 0019](adr/0019-alerts.md).

## Events

| Event | When |
| --- | --- |
| `budget.threshold` | An organization's, team's or key's spend in a budget period first reaches a configured share of its budget, including 100% (requests are now refused). |
| `mcp.tool_pending` | A pinned MCP tool is new or its definition changed, so it is hidden until an owner approves it. |
| `key.expiring` | A key expires within 7 days. Sent once per key. |

That is all for v0.3. Each event is something a person must act on, which keeps alerts from becoming noise. Error rates and latency belong in Prometheus alerting, which already has the metrics.

## Config

```yaml
alerts:
  budget_thresholds: [0.5, 0.8, 1.0]     # default
  webhooks:
    - name: finance
      url: https://hooks.example.com/genai
      secret: ${ALERT_WEBHOOK_SECRET}    # signs the body; optional but recommended
      events: [budget.threshold]
    - name: platform-slack
      url: ${SLACK_WEBHOOK_URL}
      format: slack                      # json (default) or slack
      events: [budget.threshold, mcp.tool_pending, key.expiring]
      org: acme                          # only events about this organization; omit for all
```

A budget can override the thresholds: `"budget": {"amount_usd": 500, "period": "monthly", "alert_at": [0.9]}`. An empty list turns its alerts off.

## Detecting a threshold

Spend counters already live in the limiter (memory or Valkey), and `Charge` adds each request's cost with an atomic add that returns the new total. After the add, the gateway compares the total before and after with each threshold. When a threshold is crossed (`before < t × amount ≤ after`), it records an alert.

This costs nothing extra on the request path: no reads, and the comparison is in memory. Because the add is atomic, exactly one request crosses each threshold, even across replicas sharing Valkey.

Counters can be re-seeded after a restart or a Valkey flush, which could cross a threshold again. The store has the final say, so an alert fires once: a new table `alerts_sent` has a unique key on (event, scope id, period start, threshold), and only the replica whose insert succeeds delivers the alert.

## Delivery

- Alerts go into a bounded in-memory queue and are delivered by a background worker, never on the request path.
- `POST` to the webhook URL with a 10 second timeout. Any `2xx` is success. On timeout or a `429` or `5xx` response, delivery is retried at 10 seconds, 1 minute and 5 minutes, then dropped and counted. A `4xx` other than `429` is not retried.
- Delivery is at most once per replica restart: alerts queued in memory are lost if the process dies. The `alerts_sent` row and the audit entry remain, so nothing is silently missing from the record. A durable outbox is not worth its cost for warnings.
- Each delivery attempt is logged with the webhook name, event and status code. URLs are not logged, because Slack URLs are secrets.

### Payload (`format: json`)

```json
{
  "id": "alert_01J…",
  "event": "budget.threshold",
  "ts": "2026-10-23T09:00:00Z",
  "org": {"id": "org_…", "name": "acme"},
  "team": {"id": "team_…", "name": "search"},
  "data": {
    "scope": "team",
    "threshold": 0.8,
    "spend_usd": 401.27,
    "budget_usd": 500,
    "period": "monthly",
    "period_start": "2026-10-01T00:00:00Z"
  }
}
```

Headers: `Content-Type: application/json`, `User-Agent: GenAIOpenGateway/<version>`, and, when a secret is set, `X-GOG-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">`. This is the Stripe scheme, so receivers can reject replays older than five minutes. The docs include a verification snippet.

### Slack (`format: slack`)

A Slack incoming-webhook body with a one-line `text` and a section block. For example: "*acme / search* has spent $401.27 of its $500 monthly budget (80%)." Teams and Google Chat accept the same `{"text": …}` shape.

## Admin API and UI

- `GET /admin/v1/alerts?org_id=&event=` lists sent alerts from `alerts_sent`, so they can be seen without a receiver.
- `POST /admin/v1/alerts/test` (owners only) sends a test event to a named webhook and returns the receiver's status code. This catches a wrong URL or secret during setup.
- The UI's overview lists recent alerts, and the budget form takes `alert_at` as percentages.
- Every alert also writes an audit entry (`alert.sent`), so it shows up in the audit log next to the changes that followed it.

## Security

| Threat | Mitigation |
| --- | --- |
| Forged alerts sent to the receiver | HMAC signature with a timestamp. |
| Webhook URL leaks (Slack URLs are bearer secrets) | URLs come from config, typically from the environment, and are never logged or returned by the API. |
| SSRF through webhook URLs | URLs are set only by whoever controls the config file, not through the API. Only `https` is allowed, except `http` to `localhost` for development. |
| Alert storms | Each (event, scope, period, threshold) fires once. `mcp.tool_pending` fires once per tool definition hash. |

## Metrics

- `gog_alerts_total` (by event)
- `gog_alert_deliveries_total` (by webhook and result: `ok`, `retried`, `failed` or `dropped`)

## Testing

- Unit tests: threshold crossing, including one request that crosses several thresholds and requests that cross none; per-budget overrides; signature generation against a fixed vector.
- Integration tests: alerts fire once across two gateways sharing Valkey and a store; retries on `500`, none on `400`; Slack formatting; `mcp.tool_pending` on a definition change; the test endpoint.
