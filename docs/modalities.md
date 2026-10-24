# Images, audio, video, realtime, moderation and rerank

Design for the v0.8 modalities (issue #35, [ADR 0024](adr/0024-modalities.md)). The gateway serves chat, Responses, Anthropic Messages, Gemini generateContent and embeddings today. This adds the other kinds of model a GenAI platform runs, with the same keys, policies, routing, fallback, limits, budgets, guardrails, body logging, usage, metrics and admin UI.

## Endpoints

All of them use the OpenAI API shape, the one SDKs and most self-hosted servers speak, so an OpenAI SDK works unchanged through the gateway.

| Endpoint | Client path | Body | Response | Priced by |
| --- | --- | --- | --- | --- |
| `images.generations` | `POST /v1/images/generations` | JSON | JSON, or SSE with `stream` | image, and tokens when reported |
| `images.edits` | `POST /v1/images/edits` | multipart | JSON, or SSE with `stream` | image, and tokens when reported |
| `audio.speech` | `POST /v1/audio/speech` | JSON | audio bytes, or SSE with `stream_format: sse` | million characters, and tokens when reported |
| `audio.transcriptions` | `POST /v1/audio/transcriptions` | multipart | JSON, text, or SSE with `stream` | audio minute, and tokens when reported |
| `audio.translations` | `POST /v1/audio/translations` | multipart | JSON or text | audio minute |
| `moderations` | `POST /v1/moderations` | JSON | JSON | request |
| `rerank` | `POST /v1/rerank` | JSON | JSON | request (one search), and tokens when reported |
| `videos` | `POST /v1/videos`, `GET /v1/videos`, `GET`/`DELETE /v1/videos/{id}`, `GET /v1/videos/{id}/content`, `POST /v1/videos/{id}/remix` | JSON or multipart | JSON; content is the video file | video second |
| `realtime` | `GET /v1/realtime?model=` (WebSocket) | events | events | text and audio tokens, from each `response.done` |

`model` names a virtual model as everywhere else, in the JSON body, the multipart form or, for realtime, the query string. A model whose targets cannot serve an endpoint answers `404 model_not_found`, as today.

## Providers

The `openai` provider type serves every endpoint above, so it covers OpenAI, Azure OpenAI's v1 API, and OpenAI-compatible servers: vLLM (transcriptions, rerank), Speaches and Kokoro (speech and transcription), LocalAI, and rerank services with the Cohere and Jina request shape (`/v1/rerank` with `query` and `documents`). Each path is the client's path below the provider's `base_url`.

Native translations come after, each in the adapter that already serves the cloud, so a virtual model can mix OpenAI and cloud targets and fall back between them:

| Endpoint | Vertex AI | Gemini API | Bedrock |
| --- | --- | --- | --- |
| `images.generations` | Imagen (`imagen-*`) through `:predict` | Imagen through `:predict` | Nova Canvas and Titan Image (`amazon.nova-canvas-*`, `amazon.titan-image-*`) through InvokeModel |
| `audio.speech` | Gemini TTS (`*-tts`) through `generateContent` | Gemini TTS through `generateContent` | not offered |

- **Images**: `prompt` and `n` carry over. `size` becomes Imagen's nearest `aspectRatio`, or Bedrock's `width` and `height`; `auto` uses the model's default. The answer is OpenAI's `data[].b64_json`, so images are counted as for `openai` targets. Image edits stay on `openai` targets.
- **Speech**: `input` is the text and `voice` is passed through as the Gemini voice name, such as `Kore`; OpenAI voice names are not mapped, since they sound different. Gemini returns 24 kHz PCM, which the gateway answers as WAV, or raw with `response_format: pcm`; other formats get `400`. Characters are counted as for `openai` targets, and the tokens Gemini reports are recorded too, since Google prices TTS by token.
- Which endpoint a cloud target serves follows from its model id, as chat and embeddings already do.

Native video (Veo, Nova Reel) and cloud rerank stay in issue #35: their jobs and outputs (Google operations, S3 and Cloud Storage outputs) do not map onto the OpenAI shapes without a store of their own.

Realtime APIs other than OpenAI's (Gemini Live, Nova Sonic) and WebRTC are not in this design: WebRTC media goes from the client straight to the provider, so the gateway could neither meter nor guard it.

## Request handling

The model handler stays the one path for every endpoint. Multipart requests are read into the same shape: their text fields become the JSON body that routing, guardrails and body logging already use, and their files ride beside it. The adapter writes them back as multipart with the target's model name. Files count toward `limits.max_request_bytes` like any body; operators who take audio or images raise it.

- **Routing and fallback**: unchanged. A video job is created on one target; later calls for that job go to the provider that created it.
- **Limits and budgets**: requests per minute and budgets apply to every endpoint. Tokens per minute count reported tokens only.
- **Guardrails**: `input` guardrails check the text a request carries: image prompts, speech input, moderation input, rerank query and documents, and video prompts. `output` guardrails check text responses: transcriptions, translations and JSON results. Audio, image and video bytes are not text and are not checked. Realtime audio cannot be checked, so keys with guardrails are refused realtime.
- **Body logging**: text bodies are logged as today. Files and binary responses are replaced by a note with their type and size.
- **Streaming**: SSE responses stream through, and usage is read from the final event.

## Usage and pricing

Usage records gain four counts: `images`, `characters` (speech input), `audio_seconds` (transcription and translation input) and `video_seconds`. Prices gain matching fields on a target:

```yaml
models:
  image:
    targets:
      - provider: openai
        model: gpt-image-1
        price: {per_image: 0.04, input_per_mtok: 5, output_per_mtok: 40}
  speech:
    targets:
      - provider: openai
        model: tts-1
        price: {per_mchars: 15}
  transcribe:
    targets:
      - provider: openai
        model: whisper-1
        price: {per_audio_minute: 0.006}
  video:
    targets:
      - provider: openai
        model: sora-2
        price: {per_video_second: 0.10}
  rerank:
    targets:
      - provider: cohere
        model: rerank-v3.5
        price: {per_request: 0.002}
```

The cost of a request is the sum of each count times its price, plus tokens times the token prices as today. Where the counts come from:

| Count | Source |
| --- | --- |
| images | entries in the response's `data`, or completed events of a stream |
| characters | characters of the request's `input` |
| audio seconds | the response's `usage.seconds` or `duration`; the `text`, `srt` and `vtt` formats carry neither, so such responses record no seconds |
| video seconds | the job's `seconds`, charged when the job is created |
| tokens | `usage` in the response or final event, as for chat |
| requests | every successful request, for `per_request` |

Usage by organization, team, key, model, provider and endpoint shows the new counts beside tokens, in the API, the Usage page and the CSV export. Metrics gain `gog_media_units_total{endpoint,model,unit}`.

## Video jobs

Videos are made by jobs that run for minutes, so the gateway keeps a record of each job in the store (`video_jobs` table): the upstream's job id, the organization, team and key that created it, the virtual model, the target, and the last job object the upstream returned.

- `POST /v1/videos` (JSON, or multipart with an `input_reference` image) goes through the model handler like every model endpoint: policies, limits, fallback, input guardrails on the prompt. The job is created on the first target that accepts it and recorded. It is charged when it is created, by the `seconds` the job object reports, so budgets hold back work before it runs. A job that later fails is not refunded.
- `POST /v1/videos/{id}/remix` is a new job, made on the same target as the original, and handled and charged like a created one.
- `GET /v1/videos/{id}`, `DELETE /v1/videos/{id}` and `GET /v1/videos/{id}/content` (with its `variant` query) look the job up, check that the caller owns it, and go to the target that created it. They are not charged and do not count toward request limits, since clients poll. A retrieve updates the stored job object; a delete removes the record.
- `GET /v1/videos` lists the caller's jobs from the store, newest first, as their last stored objects, with OpenAI's `limit` and `after` paging.

A caller owns a job when it is in the same organization and team. Static keys from the file share no team, so each owns only the jobs it created. Other callers get `404`, so job ids do not leak across teams. Without a database the video endpoints answer `501`.

## Realtime

`GET /v1/realtime?model=` upgrades to a WebSocket and relays OpenAI's realtime events between the client and one target.

- **Key**: the `Authorization` header or, for browsers, the `openai-insecure-api-key.<key>` subprotocol, which the gateway removes before dialing upstream.
- **Opening**: the model allow list, request limits and budgets are checked once, when the session opens. Targets are dialed in order and the next is tried when a dial fails, is refused with 429 or 5xx, or times out; once a session is open it stays on its target. The upstream gets the target's model in the query and the provider's key; the client's `OpenAI-Beta` header is passed on.
- **Model**: a `session.update` event that names a `model` has it replaced by the target's model, so a client cannot leave the virtual model.
- **Charging**: each `response.done` event is charged with its `usage` and written as one usage record (endpoint `realtime`), so long sessions show up as they go. Audio tokens are priced by `audio_input_per_mtok` and `audio_output_per_mtok` when set, otherwise as text tokens.
- **Budgets**: when a charge spends a budget, the gateway sends the client an `error` event with code `budget_exceeded` and closes the session.
- **Lifetime**: sessions close after `limits.max_realtime_session` (default `30m`), with an `error` event saying so.
- **Guardrails**: realtime audio cannot be checked, so a key with any guardrail is refused realtime with `400 invalid_request`, as streamed transcripts are. Bodies are not logged.
- **Log**: one request log line per session (endpoint `realtime`, status 101), written when it closes, with its duration and its responses' tokens and cost.

## Admin UI

- **Models page**: the new endpoints show as badges beside chat and embeddings.
- **Configuration page**: the model form gets the new price fields.
- **Usage page**: the new counts are shown.
- **Playground**: an image tab (prompt, generated images) and a speech tab (text to audio, audio to text). It runs as a team with the team's policies, as chat does.

## Rollout

Each step is one pull request with its tests, docs and conformance cases. The order is:

1. The request path for multipart and binary bodies, the usage counts and prices (migration, store, API, UI), and images, speech, transcription, translation, moderation and rerank on the `openai` type.
2. Video jobs.
3. Realtime over WebSocket.
4. Playground tabs.
5. Native Vertex AI, Bedrock and Gemini images and speech.

## Testing

- Adapter tests against a fake upstream for each endpoint:
  - multipart round trips with the model rewritten
  - binary and SSE responses
  - usage and counts for each response format
- Server tests per endpoint:
  - policies and model allow lists
  - fallback
  - guardrails on prompts and transcripts
  - body logging with files elided
  - cost from each price field
- Video job ownership across teams; realtime relay, charging per response and budget cut-off; store migrations on SQLite and Postgres.
- A conformance run with the official OpenAI Python SDK against the gateway in front of a fake upstream, one call per endpoint, in CI.

## Not in this design

- Batch (`/v1/batches`, `/v1/files`) has its own design in [batch.md](batch.md). Semantic cache and WebRTC stay in issue #35 for later.
