# Streams

A stream copies records to another system as the gateway writes them. You can send them to a SIEM, to a data warehouse loader, or to a log shipper such as Vector or Fluent Bit, which can forward to Kafka, S3 or Splunk.

| Record type | What it is |
|---|---|
| `usage` | One per model request: organization, team, key, model, target, status, latency, tokens, cost |
| `audit` | One per entry of the audit log: changes through the admin API or UI, tool approvals, and the other actions the log records |
| `mcp_calls` | One per MCP tool call |
| `guardrail_events` | One per guardrail match |

```yaml
streams:
  siem:
    type: webhook
    url: https://siem.example.com/ingest/gateway
    secret: ${SIEM_SIGNING_SECRET}             # optional: signs each batch
    headers: {Authorization: "Bearer ${SIEM_TOKEN}"}
    records: [audit, guardrail_events]
  usage-to-lake:
    type: file
    path: /var/log/genaiopengateway/usage.jsonl
    records: [usage]
    batch_size: 1000                           # default 500, at most 10,000
    flush_interval: 10s                        # default 5s
```

Owners can also add streams in the admin UI, under **Settings → Notifications**. They can also use the admin API at `PUT /admin/v1/config/streams/{name}`. The URL, secret and header values are encrypted in the database. Every replica applies a change within `store.refresh_interval`.

## Format

Each record is one JSON line:

```json
{"type":"usage","time":"2026-11-11T17:00:00Z","data":{"request_id":"req_…","ts":"…","org_id":"org_…","model":"chat","tokens":{"input_tokens":10,"output_tokens":5},"cost_usd":0.0001,…}}
```

`data` is the record as stored, with snake_case field names. Audit, MCP call and guardrail records match what the admin API lists. `time` is when the gateway queued the line.

- **Webhook**:
  - Each batch is one `POST` with `Content-Type: application/x-ndjson`.
  - With a secret, the batch carries `X-GOG-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">`. Alert webhooks are signed the same way ([alerts](alerts.md)).
  - Any 2xx response counts as delivered. Redirects are not followed.
- **File**: lines are appended to `path` on each replica. Point a log shipper at the file and rotate it with its usual tooling.

## Delivery

A record goes to streams only after it is written to the database. The database stays the complete record: usage reports and budgets read it, and streams are a copy.

- Each replica sends the records it writes. With several replicas, the receiver gets batches from each one.
- A failed batch is retried after 1, 5 and 30 seconds, then dropped. Each stream queues up to 50,000 records on each replica. Past that, new records are dropped rather than slowing requests.
- When a replica shuts down, it sends what its streams hold for as long as the shutdown grace period allows.
- Delivery is at least once in practice: a receiver that times out after storing a batch gets it again on the retry. Deduplicate on `data.id`, or on `data.request_id` for usage.

`gog_stream_records_total{stream, result}` counts records `delivered` and `dropped`. Alert on dropped records if the receiver needs everything. To fill a gap, export the records from the database: the `usage`, `audit_log`, `mcp_calls` and `guardrail_events` tables.
