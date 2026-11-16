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

Batches are served by `openai` providers (OpenAI and Azure OpenAI's v1 API), and by `bedrock` and `vertex` providers through their own batch jobs, described next.

## Bedrock and Vertex AI {#native}

Bedrock and Vertex AI run batches as jobs that read a file from cloud storage and write results back there, in their own request formats. The gateway hides that: clients upload the same OpenAI file and read the same OpenAI results, and the gateway translates in both directions.

| | `bedrock` | `vertex` |
| --- | --- | --- |
| Turned on by | `s3_output_uri` and `batch_role_arn` | `gcs_output_uri` |
| Models | every chat model | Gemini models |
| Job | a model invocation job of type Converse | a batch prediction job |
| Requests | `/v1/chat/completions` only | `/v1/chat/completions` only |

```yaml
providers:
  bedrock:
    type: bedrock
    region: us-east-1
    s3_output_uri: s3://acme-ai/gateway
    batch_role_arn: arn:aws:iam::123456789012:role/bedrock-batch
  vertex:
    type: vertex
    project: acme-ai-prod
    location: us-central1
    gcs_output_uri: gs://acme-ai/gateway
```

What happens on each call:

- **Upload.** Each line is translated as a single chat completion would be, to a Converse request on Bedrock or a `generateContent` request on Vertex AI. A line the target cannot serve rejects the file with its `custom_id`. The gateway writes three objects below `<folder>/batches/<file id>/`: `source.jsonl` (the file as uploaded, after guardrail redactions), `input.jsonl` (the translation) and `meta.json`.
- **Create.** The gateway starts the job on `input.jsonl`, with results below `out/` in the same folder. Bedrock runs the job as `batch_role_arn`.
- **Retrieve and cancel.** The job's state becomes an OpenAI status: `validating`, `in_progress`, `completed`, `failed`, `cancelling`, `cancelled` or `expired`. A job that finished with some failed requests is `completed`, with the failures in the error file.
- **Results.** The output file holds one line per answered request, as a chat completion under the client's `custom_id`; the error file holds the requests that failed. Both are translated while they are streamed. Neither cloud keeps the order of the input, so match results by `custom_id`.
- **Usage.** Bedrock reports a job's token totals in its manifest. Vertex AI reports them per request, so the gateway sums the results once when the job ends. A cancelled or expired job is charged for what it answered.
- **Delete.** Deleting the input file removes the three objects the gateway wrote. Result files stay in the bucket until a lifecycle rule or an operator removes them, and they cannot be read through the gateway once the input file is gone.

Ids name the job and the file (`file_br_…`, `batch_br_…`, `file_vx_…`, `batch_vx_…`), so the gateway stores nothing beyond the usual records, and it reads only objects below the provider's folder.

Permissions:

- **Bedrock.** The gateway's AWS credentials need `bedrock:CreateModelInvocationJob`, `GetModelInvocationJob` and `StopModelInvocationJob`, `iam:PassRole` on the batch role, and `s3:GetObject`, `s3:PutObject` and `s3:DeleteObject` on the folder. S3 takes no Bedrock API key, so batches need credentials from the AWS default chain. The batch role must trust `bedrock.amazonaws.com` and be able to read and write the folder.
- **Vertex AI.** The gateway's credentials need `aiplatform.batchPredictionJobs.create`, `get` and `cancel`, and `storage.objects.create`, `get`, `list` and `delete` on the folder. Vertex AI's service agent must be able to read and write it too.

Limits to know:

- Bedrock refuses a job with fewer records than its quota's minimum, which is 100 for most models. The refusal arrives when the batch is created, with Bedrock's message.
- Both clouds offer batch for some models and regions only. A model without it fails when the batch is created.
- Vertex AI does not return a request's id, so the gateway carries each request's row in a `gog_row` label and reads it back from the result. Labels on Vertex AI also appear in billing reports.
- Claude on Vertex AI, embeddings and the Responses API are not served in native batches.
- Set `batch_discount` on the target to the cloud's batch price cut, as for OpenAI.
- These adapters are tested against fakes built from the published API descriptions. They have not been run against live Bedrock or Vertex AI accounts yet.

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
- Adapter tests for Bedrock and Vertex AI against fake S3, Cloud Storage and job APIs: translation of the input, the job request, status mapping, results by `custom_id`, the error file, usage, and ids that name objects outside the folder.
- Server tests against a fake upstream: upload checks and line errors, model rewriting and fallback, input guardrails with redaction, refusal when output guardrails apply, ownership across teams, output file download, charging once with the discount, the refresh loop, and `501` without a database.
- Admin tests: scoping by role, cancel and its audit record.
- The OpenAI SDK conformance run uploads a file, creates, retrieves, lists and cancels a batch, and downloads its output.
- The browser test opens the Jobs page.

## Not in this design

- Native batches for requests other than chat completions, and for Claude on Vertex AI.
- Files for other purposes, such as Responses `input_file` or fine-tuning.
- Checking batch output with output guardrails.
