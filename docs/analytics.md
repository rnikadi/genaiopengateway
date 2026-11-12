# Usage rollups and analytics

The gateway's own pages answer "who spent what this month". Two things need more: reports that reach back further than the usage rows are kept, and analysis in the warehouse or BI tool the company already has. This page covers both.

## Daily rollups

Every hour the gateway sums usage into `usage_daily`: one row per UTC day, organization, team, key, endpoint, model and upstream target, with requests, errors, tokens, media amounts, cost, cache hits and savings, and total latency. The current day and the day before are redone each time, because data planes deliver usage up to a day late.

Rollups are small. A key that calls three models on two endpoints adds six rows a day however many requests it makes, so a year of history for a few hundred keys is a few hundred thousand rows.

**They outlive the usage rows.** `store.usage_retention` prunes usage rows, by whole UTC days, after their day is summed. Reports then read the rows for the days that are kept and the rollups for the days before:

- **Usage and cost**, `GET /admin/v1/usage`, the chargeback and FOCUS exports ([chargeback.md](chargeback.md)) all reach back past the retention, with no change to how they are called.
- For days read from rollups, a range that starts in the middle of a day includes the whole day.
- Per-request detail (the Requests page, latency of one call, `request_id`) is gone with the rows, as before.

```yaml
store:
  usage_retention: 90d      # per-request rows; omit to keep forever
  rollup_retention: 1095d   # daily totals (3 years); omit to keep forever. At least usage_retention.
```

Both can be changed under **Settings, Logging and data**. With no `usage_retention` nothing is pruned and reports read the rows alone; rollups are still built, for the export below and for the day a retention is set.

After an upgrade to v1.3 the first rollup sums every day that still has usage rows. Days pruned by an earlier version are gone: nothing summed them.

Rollups hold ids, model names and amounts, never prompts or responses. They are part of [backups](backup.md). To erase an organization's history, delete its rows from `usage` and `usage_daily`.

## Loading a warehouse

There are two feeds. Use one or both.

### Daily totals, pulled

`GET /admin/v1/usage/rollups` streams the rollup rows, which is the simplest thing to load on a schedule: the schema is fixed, a day is a few rows per key, and loading a day again replaces it cleanly.

```sh
curl -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  "https://gateway.example/admin/v1/usage/rollups?from=2026-03-01T00:00:00Z&to=2026-03-08T00:00:00Z" > usage-daily.jsonl
```

| Parameter | Meaning |
| --- | --- |
| `from`, `to` | RFC 3339. Whole UTC days from `from`'s day to the day `to` falls in. Default: this month. |
| `org_id`, `team_id`, `key_id` | Limit the rows. Needs `usage.read` for what is asked; without `org_id`, for everything. |
| `format` | `ndjson` (default), one JSON object per line, or `csv` |
| `refresh=true` | Sum today's usage first, instead of reading what the last hourly run left |

Each row:

```json
{"day":"2026-03-07","org_id":"org_…","team_id":"team_…","key_id":"key_…","endpoint":"chat.completions","model":"chat-default",
 "provider":"openai","upstream_model":"gpt-4.1-mini","requests":1840,"errors":12,"input_tokens":2210344,"output_tokens":401122,
 "cached_input_tokens":880000,"reasoning_tokens":0,"images":0,"characters":0,"audio_seconds":0,"video_seconds":0,
 "cost_usd":1.5259,"cache_hits":40,"saved_usd":0.031,"latency_ms_sum":1612400}
```

`errors` counts answers with a status of 400 or above, and `latency_ms_sum / requests` is the mean latency. The primary key is every field up to `upstream_model`.

A nightly load that is safe to repeat: fetch yesterday and the day before, delete those two days in the warehouse table, insert the rows. For example with DuckDB:

```sql
CREATE TABLE IF NOT EXISTS usage_daily AS SELECT * FROM read_json_auto('usage-daily.jsonl') LIMIT 0;
DELETE FROM usage_daily WHERE day IN (SELECT DISTINCT day FROM read_json_auto('usage-daily.jsonl'));
INSERT INTO usage_daily SELECT * FROM read_json_auto('usage-daily.jsonl');
```

BigQuery (`bq load --source_format=NEWLINE_DELIMITED_JSON`), Snowflake (`COPY INTO … FILE_FORMAT = (TYPE = JSON)`) and ClickHouse (`INSERT … FORMAT JSONEachRow`) read the same file. Join `org_id`, `team_id` and `key_id` to names with `GET /admin/v1/orgs`, the teams of each and `GET /admin/v1/keys`.

### Every request, pushed

For per-request analysis (latency percentiles, one user's calls, joining with application logs), a [stream](streams.md) sends each usage record as it is written, to a webhook or a file that a shipper such as Vector or Fluent Bit forwards to Kafka, S3, ClickHouse or BigQuery. Streams also carry audit entries, MCP calls, guardrail events and denials. Delivery is at least once and a receiver that is down for long loses records, so treat the stream as the detailed feed and the daily totals as the numbers to reconcile against.

## Which numbers to trust

| Question | Source |
| --- | --- |
| What did a team spend last quarter? | Rollups: the Usage page, chargeback export or `usage/rollups` |
| What was p95 latency yesterday? | Prometheus ([observability.md](observability.md)), or the stream in the warehouse |
| Which request cost $40? | The Requests page while the row is kept, or the stream |
| Does the provider's invoice agree? | Compare the FOCUS export with the bill, per upstream model |
