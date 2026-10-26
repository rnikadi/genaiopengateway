# Guardrails

Today the gateway controls who may call which model or tool, and how much. It does not look at what is sent. Enterprises need to stop personal data and secrets from reaching a provider, catch prompt injection in MCP tool output, and plug in the moderation or data-loss service they already pay for. This design adds a guardrail step to model calls and MCP calls. Decisions are recorded in [ADR 0020](adr/0020-guardrails.md).

## Principles

- **Opt in, by policy.** A guardrail runs only where an admin attached it to an organization, team, key or virtual MCP server.
- **Fast built-ins, external services for the rest.** The built-in checks are in-process and meet the v0.5 gate of under 10 ms at p50. Moderation and injection classifiers run in a service the operator chooses, called over HTTP.
- **Monitor before enforcing.** Every guardrail can run in `log` mode first, to see what it would block.
- **Never store the content.** A guardrail decision records which guardrail, what action and counts by kind, not the matched text.

## Not in scope

| Feature | Why |
| --- | --- |
| WebAssembly plugins (ADR 0008) | A webhook covers custom logic in any language without a host API the project must keep stable. Revisited when someone needs custom checks in-process. See ADR 0020. |
| Built-in ML classifiers for toxicity or injection | Large models do not belong in the gateway binary. Use the `model` guardrail with a safety model such as Llama Guard or ShieldGemma served through any configured model, or the `webhook` guardrail. |
| Checking images, audio and files | Text only. Media parts, including `data:` URLs, pass unchecked. |
| Rewriting prompts (adding a system prompt, templating) | A transform, not a guardrail. Can be added later on the same hook. |

## Guardrails in config

Guardrails are defined once in config and attached by name.

```yaml
guardrails:
  no-pii:
    type: pii                      # built-in detectors from body logging
    detect: [email, phone, credit_card, iban, us_ssn]   # default: all
    action: redact                 # block | redact | log
    stages: [input]
  no-secrets:
    type: pii
    detect: [secret]
    action: block
    stages: [input, tool_input]
  deny-terms:
    type: regex
    patterns: ['(?i)project\s+falcon', 'EMP-\d{6}']
    action: block
    stages: [input, output]
  injection:
    type: model                    # ask a classifier model
    model: llama-guard             # a virtual model from `models`
    prompt: ""                     # optional system prompt; Llama Guard needs none
    unsafe_if: '(?i)^\s*unsafe'    # pattern on the model's reply (this is the default)
    action: block
    stages: [tool_output]
    timeout: 2s
    on_error: allow                # allow | block when the check fails
  dlp:
    type: webhook                  # call the company's DLP service
    url: https://dlp.internal.example.com/check
    secret: ${DLP_WEBHOOK_SECRET}  # signs the body, as alert webhooks do
    action: block                  # default; the reply chooses block or redact. log only records.
    stages: [input, output]
    timeout: 1s
    on_error: block
```

### Stages

| Stage | What is checked | When |
| --- | --- | --- |
| `input` | The text of every message in a model request: system, user, assistant and tool messages, text parts only | After authentication and limits, before routing |
| `output` | The text and tool call arguments of the model's response | After the response, before it reaches the client. Only `200` responses are checked. |
| `tool_input` | The arguments of an MCP `tools/call` | Before the call is forwarded upstream |
| `tool_output` | The text content and structured content of an MCP tool result | Before the result reaches the client |

### Actions

| Action | Effect |
| --- | --- |
| `block` | The request is refused with `400 guardrail_blocked` naming the guardrail (an MCP tool error for MCP stages). For `output`, the tokens were spent, so usage and cost are still recorded. |
| `redact` | Matches are replaced with `[REDACTED:<kind>]` and the request continues. On `input` the provider never sees the original value. Available for `pii` and `regex`. A `webhook` redacts when its reply says so. |
| `log` | Nothing changes for the client. The decision is recorded and counted. |

### Types

| Type | Checks | Notes |
| --- | --- | --- |
| `pii` | Emails, phone numbers, card numbers with Luhn, IBANs with mod-97, US SSNs, well-known secrets | The detectors of [body logging](body-logging.md), already tested and fuzzed. |
| `regex` | RE2 patterns | Linear time, so a pattern cannot stall the gateway. |
| `model` | Sends the texts, joined, as one user message to a virtual model, with `prompt` as the system message when set, and matches `unsafe_if` against the reply | Uses the gateway's own routing, limits and metering, so the classifier's cost is recorded under the requesting key. Slower; meant for `tool_output` and selected keys. Cannot `redact`. |
| `webhook` | `POST` of a JSON document to the operator's service, which answers `allow`, `block` or `redact` with replacements | The way to plug in Presidio, a DLP product, Azure AI Content Safety, Lakera or an in-house service. |

The webhook request and response:

```json
{"guardrail": "dlp", "stage": "input", "request_id": "req_…", "org_id": "org_…", "team_id": "team_…", "key_id": "key_…",
 "texts": ["first message text", "second message text"]}
```

```json
{"action": "redact", "reason": "customer account number", "texts": ["first message text", "[REDACTED] text"]}
```

`action` is `allow`, `block` or `redact`. `texts` in the reply replaces the texts by position, must have as many entries as the request, and is only read for `redact`. A webhook configured with `action: log` only records what the reply asked for. Any status but `200`, or a reply that cannot be read, is a failed check and follows `on_error`. The body is signed with `X-GOG-Signature` like alert webhooks, redirects are not followed, and only `https` URLs are allowed outside `localhost`.

## Attaching guardrails

A new policy field, `guardrails`, lists guardrail names at organization, team and key level. Unlike other policy fields, the lists **add up**: a request runs every guardrail named at any of its three levels, so a team cannot drop a guardrail its organization requires. Only owners and the organization's admins can change the field, and the change is audited.

```json
PATCH /admin/v1/orgs/{id}
{"guardrails": ["no-secrets", "no-pii"]}
```

A virtual MCP server can name guardrails too, for its `tool_input` and `tool_output` stages, so a server that reads untrusted content (email, tickets, the web) always gets an injection check:

```yaml
mcp:
  virtual_servers:
    support:
      guardrails: [injection]
      servers: [{server: zendesk}]
```

A config `guardrails` entry that no policy or virtual server names does nothing. Startup fails on an unknown name in a virtual server, and the admin API rejects an unknown name in a policy. Removing a guardrail from config while a policy still names it skips it; the admin UI drops the stale name the next time the policy is saved.

## Order and cost

Guardrails at one stage run in a fixed order, by name within each group: `pii` and `regex` first (in-process, a few milliseconds at most), then `model` guardrails and webhooks that cannot redact in parallel, then webhooks that may redact one after another, each on the texts the previous one returned. The first `block` stops the stage; external checks run only when the in-process ones did not already block.

The gate for v0.5 is that the built-ins add under 10 ms at p50, measured on a 32 KiB chat request with every built-in on (about 2.5 ms on a CI runner). The detectors run their regular expressions only around the characters a match must contain, such as digits or `@`, which keeps long prose cheap. External checks are bounded by their `timeout` (default 2 s, at most 30 s) and their result follows `on_error` when they fail or time out.

## Streaming

`input` checks finish before the request is sent, so they do not affect streaming.

An `output` guardrail on a streamed response means the gateway must see the response before the client does. The gateway buffers the upstream stream, assembles it as body logging does, and checks it. If it passes, the original events are sent unchanged; if it is redacted, the gateway sends the assembled, redacted result as one short stream in the client's format (text deltas, tool calls, finish reason and usage). If a redacted stream cannot be rebuilt, or a response is over 16 MiB, it is blocked rather than sent unchecked. The client gets a valid stream, but the first token arrives only when the whole response is done, so `output` guardrails are meant for the keys that need them, not for every key.

Stream checks that release text as it arrives and cut the stream off on a match are possible later, but cannot take back text already sent.

## Media endpoints

On the media endpoints ([modalities.md](modalities.md)) guardrails check text only. `input` checks see image and video prompts, speech input, moderation input, and rerank queries and documents. `output` checks see transcripts and translations, as JSON or as text, SRT or WebVTT; a streamed transcript is refused while an `output` guardrail applies, since its text reaches the client as it is spoken. Images, audio and video are not text and pass unchecked. Realtime sessions carry audio that cannot be checked, so keys with any guardrail are refused realtime.

Batch input files ([batch.md](batch.md)) are checked line by line with `input` guardrails when they are uploaded: a block rejects the file and names the request, and a redaction changes the line that is uploaded. Batch results come back as a file, so keys with an `output` guardrail are refused batches.

## What is recorded

- **Response header** `X-Gog-Guardrails` on model calls, listing the guardrails that acted, for example `deny-terms=log, no-pii=redact`.
- **A table `guardrail_events`**: `request_id`, `ts`, `org_id`, `team_id`, `key_id`, `guardrail`, `stage`, `action`, `failed`, `reason` (from a webhook or classifier, or why the check failed, up to 200 characters), `counts` (JSON, by kind), `latency_ms`. Only guardrails that matched or failed are stored. Pruned with `usage_retention`.
- **Admin API** `GET /admin/v1/guardrail-events` with filters `org_id`, `team_id`, `key_id`, `guardrail`, `action`, `request_id`, `from` and `to`, scoped like usage. The UI has a Guardrails page, and a logged request body shows its events.
- **Metrics** `gog_guardrail_checks_total{guardrail, stage, result}` and `gog_guardrail_duration_seconds{guardrail, stage}`.
- **Alerts**: no new event. Blocks are visible in metrics, where rate alerts belong.

## Security

| Threat | Control |
| --- | --- |
| A team removes a guardrail its organization requires | Lists add up across levels, and only owners change an organization's list (ADR 0030); team and key lists need an admin role. Changes are audited. |
| A guardrail leaks the content it checks | Decisions store counts and the guardrail's reason, never the text. Webhook URLs come from config, use `https`, and requests are signed. |
| Prompt injection through MCP tool output | `tool_output` guardrails on the virtual server, with a classifier through `model` or `webhook`. Defense in depth, not a guarantee, as for every injection filter. |
| A slow or failing check service blocks all traffic, or lets everything through | A timeout per guardrail and an explicit `on_error`. `log` mode for rollout. Metrics show latency and failures per guardrail. |
| Expensive regular expressions | RE2 only, which runs in linear time. |
| The classifier model is itself prompt-injected | The classifier prompt is fixed and the verdict is read only by `unsafe_if`; a model that is talked out of flagging content fails open, so a `model` guardrail is one layer among several. |

## Testing

- Table tests per type and stage, including redaction inside nested JSON and tool calls.
- Policy tests: lists add up across levels, unknown names are rejected.
- Streaming tests for each client format: a passing stream is byte-identical, a redacted stream is valid and complete, a block becomes an error.
- A fake webhook and a fake classifier model for `allow`, `block`, `redact`, timeouts and `on_error`.
- An MCP test where tool output with an injection marker is blocked.
- The 10 ms gate runs in CI, without the race detector, and fails above 10 ms at p50.
