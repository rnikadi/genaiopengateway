# Request body logging with redaction

Today the gateway records who called what and what it cost, but never the content. Enterprises need content for some teams: to debug an application, to review an incident, or to meet a retention rule. Others must never store it. This design adds opt-in logging of request and response bodies, for model calls and MCP tool calls, with redaction before anything is written. Decisions are recorded in [ADR 0018](adr/0018-body-logging.md).

## Principles

- **Off by default** at every level. Content is stored only where an admin turned it on for an organization, team or key.
- **Redact before storing.** Unredacted content is never written to disk, logs or traces.
- **Bounded.** Each body is truncated to a size limit, and records expire after a retention period.
- **Encrypted at rest**, with the key from [per-user credentials](mcp-user-credentials.md#storage).
- **Every read is audited.** Looking at content is a recorded action, like changing a policy.

## Not in scope

| Feature | Why |
| --- | --- |
| Exporting bodies to a SIEM or data lake | Comes with the plugin and export work in v0.5. The admin API can be polled meanwhile. |
| Named-entity redaction with an ML model (names, addresses) | Large dependency, slow on the request path and never complete. The guardrails work in v0.5 adds a hook for an external PII service. |
| Logging images, audio and files | Base64 payloads are replaced by a placeholder with their media type and size. |
| Replaying a logged request | Easy to add later from the stored body. Not needed to debug or review. |

## Turning it on

A new policy field, `log_bodies`, set at organization, team or key level through the admin API, the UI or static keys in config:

```json
PATCH /admin/v1/teams/{id}
{"log_bodies": true}
```

The most specific level that sets it wins: a key's `false` overrides its team's `true`. Unset means inherit, and unset everywhere means off. Only owners change it on an organization; owners and the organization's admins change it on teams and keys. Changes are audited.

Gateway-wide settings:

```yaml
body_logging:
  enabled: true              # master switch; false ignores every log_bodies policy
  retention: 30d             # default 30d, at most 365d
  max_body_bytes: 65536      # each of request and response, after redaction; default 64 KiB
  redact:
    builtin: [email, phone, credit_card, iban, us_ssn, secret]   # default: all
    patterns:                # extra regular expressions (RE2), replaced by [REDACTED:custom]
      - 'EMP-\d{6}'
```

Startup fails if `body_logging.enabled` is set without a store or without `secrets.encryption_key`.

## What is recorded

One record per request in a new table, `request_bodies`, keyed by `request_id`. The same id is in the usage record and the request log line, so content can be joined to cost and latency.

| Field | Content |
| --- | --- |
| `request_id`, `ts`, `org_id`, `team_id`, `key_id` | As in usage records. |
| `kind` | `model` or `mcp`. |
| `endpoint` | For model calls the endpoint as in usage records, for example `chat.completions`; for MCP the virtual server path, for example `/mcp/engineering`. |
| `request` | The request body as received, redacted and truncated. For multipart requests (audio and image uploads), the text fields and each file's name, type and size, never the file. |
| `response` | Audio, image and video responses are noted with their type and size, not kept. For other non-streaming responses, the body as returned. For streams, the assembled result: the text, tool calls and finish reason built from the events, not the raw event stream. Redacted and truncated. |
| `truncated` | Whether either body was cut. |
| `redactions` | Count per redaction type, for example `{"email": 2}`. Never the redacted values. |

For MCP, `tools/call`, `prompts/get` and `resources/read` are logged: `request` is the method and its params (for a tool, its name and arguments) and `response` the result, or the error. The MCP calls list in the UI links to the body.

Clients may set `X-Request-Id`, so one id can belong to requests of several organizations. A read returns only a record the operator's organizations may see.

Bodies are written asynchronously through a batched writer like usage records, so logging adds no latency to the response. Redaction and encryption run in that writer, off the request path. Its queue is short (256 records), because each record can hold up to four times `max_body_bytes` of request and of response before redaction; when it is full, body records are dropped while usage records still have room, and the drop is counted.

## Redaction

Redaction walks the JSON and applies to string values only, so the structure stays valid and field names are kept. Keys whose names suggest secrets (`api_key`, `password`, `authorization`, `token`, `secret`) have their whole value replaced.

| Builtin | Matches | Replaced by |
| --- | --- | --- |
| `email` | Email addresses | `[REDACTED:email]` |
| `phone` | E.164 and common national formats with at least 9 digits | `[REDACTED:phone]` |
| `credit_card` | 13 to 19 digit numbers that pass the Luhn check | `[REDACTED:credit_card]` |
| `iban` | IBANs that pass the mod-97 check | `[REDACTED:iban]` |
| `us_ssn` | `123-45-6789` shapes outside known invalid ranges | `[REDACTED:us_ssn]` |
| `secret` | Well-known key formats (`sk-…`, `sk-ant-…`, `AKIA…`, `ghp_…`, `xox…-`, JWTs, PEM private keys) and this gateway's own keys | `[REDACTED:secret]` |

Checksums keep false positives down for card numbers and IBANs. Redaction runs on a bounded prefix of each body (four times `max_body_bytes`), so a huge request cannot make it slow. Everything beyond that prefix is dropped anyway by truncation. Redaction is pattern based: it lowers risk, but it is not a guarantee that no personal data is stored. The docs say so plainly.

## Reading bodies

- `GET /admin/v1/requests?org_id=&team_id=&key_id=&from=&to=` lists recent requests from the usage records: model, status, tokens, cost, and whether a body is stored.
- `GET /admin/v1/requests/{request_id}` returns that record with its body. Each call writes an audit entry (`request_body.read`) naming the operator.
- Roles: owners and the organization's admins. Viewers see the list but not bodies, because content is more sensitive than spend.
- The UI gets a **Requests** page: a filterable list with a detail view of the redacted request and response.

## Retention and deletion

- An hourly job deletes bodies older than `retention`, in batches so it never holds long locks. It runs when `body_logging.enabled` is false too, so bodies logged before it was turned off still expire.
- Turning `log_bodies` off stops new records but keeps existing ones until they expire. `DELETE /admin/v1/requests?org_id=…&before=…` (owners only, audited) deletes early, for erasure requests.
- Deleting an organization, team or key does not delete its bodies; they expire with retention, like usage records.

## Security

| Threat | Mitigation |
| --- | --- |
| Personal data stored without need | Off by default, opt-in per scope, redaction before storage, bounded retention. |
| A database dump exposes content | Bodies are encrypted with AES-256-GCM, with the key outside the database. |
| An insider browses content | Only owners and the organization's admins can read bodies, and every read is audited with the operator's identity. |
| Secrets in prompts (pasted API keys) | The `secret` builtin and secret-named fields are always redacted when `builtin` is not set. |
| Logging slows or breaks requests | Writes are asynchronous and bounded. A failed write never fails a request. |

## Metrics

- `gog_request_bodies_written_total`, `gog_request_bodies_dropped_total`
- `gog_redactions_total` (by type)

## Testing

- Table tests for each builtin pattern, with true and false positives, including Luhn and mod-97 edge cases. A fuzz test checks that redaction always returns valid JSON.
- Stream assembly tests against recorded OpenAI and Anthropic streams, including tool calls.
- Integration tests: policy inheritance, the master switch, truncation, encryption at rest (the raw column never contains the plaintext), audit on read, the viewer gets `403`, and retention pruning.
