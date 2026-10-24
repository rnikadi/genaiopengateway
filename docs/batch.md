# Batch and files

Teams send large offline workloads through OpenAI's Batch API: upload a JSONL file of requests, start a batch, poll it, and download the results within 24 hours at about half the price. Without the gateway in front of it, a batch skips keys, model allow lists, guardrails, budgets and usage records. This design puts `/v1/files` and `/v1/batches` behind the gateway, in the shape the OpenAI SDKs already use, and shows batches and video jobs to operators in the admin UI. It is tracked in issue #35, and the decision is ADR 0025.

## What clients call

The OpenAI API paths, unchanged, so `client.files.create(...)` and `client.batches.create(...)` work with a gateway key:

| Call | What the gateway does |
| --- | --- |
| `POST /v1/files` (multipart, `purpose=batch`) | Reads the JSONL, checks it, picks a target from the model it names, rewrites each line's model to the upstream model, uploads it there and records the file. |
| `GET /v1/files`, `GET /v1/files/{id}` | Lists or returns the caller's files from the store. |
| `GET /v1/files/{id}/content` | Streams the file from the target that holds it. Batch output and error files are recorded when a batch reports them, so their owners can download them. |
| `DELETE /v1/files/{id}` | Deletes the file at the target and its record. |
| `POST /v1/batches` | Checks that the caller owns the input file and starts the batch at the file's target. Counts as a request against rate limits and is refused when a budget is spent. |
| `GET /v1/batches/{id}`, `POST /v1/batches/{id}/cancel` | Forwarded to the batch's target. The stored batch object is refreshed. |
| `GET /v1/batches` | Lists the caller's batches from the store, newest first, with `limit` and `after`. |

Only `purpose=batch` is accepted for now. Other purposes (`assistants`, `user_data`, `vision`, `fine-tune`) answer `400` until a feature needs them, because a file without a model gives the gateway nothing to route on.

## Checking the input file

The gateway reads the upload into memory, up to `limits.max_batch_file_bytes` (default 100 MiB; OpenAI accepts 200 MB), and checks every line before anything reaches an upstream:

- Each line is a JSON object with `custom_id`, `method: POST`, `url` and a `body` object with a `model`.
- All lines use one `url` and one `model`, as OpenAI requires. The url is one of `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, `/v1/completions` or `/v1/moderations`.
- The caller may use the model, and the model has a target that serves batches.
- Input guardrails run on each line's body as they run on a single request. A blocked line rejects the file with its line number and `custom_id`. Redactions are applied to the line that is uploaded.
- When an output guardrail applies to the caller, the upload is refused: the results come back as a file the gateway cannot check line by line before the client reads it.

Errors are `400` with the line number, so a client fixes the file before it spends anything.

## Routing

A virtual model's targets are tried in order, as for any request, among those whose provider serves batches. The gateway rewrites `body.model` in every line to the target's upstream model, so the same file works whichever target takes it, and uploads to the first that accepts it. The file, and every batch made from it, stays on that target: output, errors and cancels go back to where the batch runs.

Batches are served by `openai` providers (OpenAI and Azure OpenAI's v1 API). Native Vertex AI and Bedrock batch jobs read from and write to cloud storage buckets, not uploaded files, and are left for later.

## Ownership

Files and batches are recorded in the store (`batch_files` and `batches` tables, migration 0011) with the organization, team and key that made them, the virtual model, the target and the last object the upstream returned. As for video jobs, a caller owns a record in the same organization and team; static keys from the config file own only their own. Other callers get `404`, so ids do not leak between teams. Without a database these endpoints answer `501`.

## Metering

A batch is charged once, when it first reaches a terminal status (`completed`, `failed`, `expired` or `cancelled`), from the `usage` the batch object reports: input, cached input, output and reasoning tokens. The cost uses the target's prices, less the target's `batch_discount`, a fraction such as `0.5` for OpenAI's 50% batch price. Without it, batches are priced like single requests, which overstates rather than understates spend. Upstreams that report no batch usage are recorded with zero tokens and flagged unpriced.

The charge writes one usage record (endpoint `batches`, request id the batch id), counts toward budgets and token limits, and can fire budget alerts. A batch is marked charged in the same update that claims it, so two replicas, or a poll and a client retrieve at the same time, never charge it twice.

Clients do not have to poll for a batch to be charged. Every minute, the gateway refreshes batches that are not finished and were last refreshed more than five minutes ago, up to 100 at a time. Each replica claims the batches it refreshes in the store, so replicas do not repeat each other's work. Budgets are checked when a batch starts, not while it runs: a running batch can take a team past its budget, as a long streamed answer can.

## Admin API and UI

A new **Jobs** page lists batches and video jobs for an organization, with team, model, target, status, request counts, created and finished times, and cost. Operators who can write to the organization (owners and its admins) can cancel a batch, and the cancel is audited as `batch.cancel`. The page shows metadata only: file contents and results stay with the team, as logged bodies do.

- `GET /admin/v1/batches?org_id=&team_id=&status=` and `GET /admin/v1/videos?org_id=&team_id=`, scoped like other admin lists.
- `POST /admin/v1/batches/{id}/cancel`, which sends the cancel to the batch's target.

## Configuration

```yaml
limits:
  max_batch_file_bytes: 104857600 # 100 MiB
models:
  gpt-4o-mini:
    targets:
      - provider: openai
        model: gpt-4o-mini
        price: {input_per_mtok: 0.15, output_per_mtok: 0.6, batch_discount: 0.5}
```

`batch_discount` is between 0 and 1, and is also editable in the admin UI's model form.

## Testing

- Store tests on SQLite and Postgres: records, ownership listing, and the charge claim taking effect once.
- Server tests against a fake upstream: upload checks and line errors, model rewriting and fallback, input guardrails with redaction, refusal when output guardrails apply, ownership across teams, output file download, charging once with the discount, the refresh loop, and `501` without a database.
- Admin tests: scoping by role, cancel and its audit record.
- The OpenAI SDK conformance run uploads a file, creates, retrieves, lists and cancels a batch, and downloads its output.
- The browser test opens the Jobs page.

## Not in this design

- Native Vertex AI and Bedrock batch jobs, which work from cloud storage.
- Files for other purposes, such as Responses `input_file` or fine-tuning.
- Checking batch output with output guardrails.
