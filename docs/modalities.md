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

### Speech providers with their own APIs

Three provider types serve speech and transcription from providers that do not speak the OpenAI API. Each has a default `base_url` and takes `api_key`, which teams can replace with their own ([team-credentials.md](team-credentials.md)).

| Type | `audio.speech` | `audio.transcriptions` | Which model serves what |
| --- | --- | --- | --- |
| `elevenlabs` | Text to speech, `POST /v1/text-to-speech/{voice}` | Scribe, `POST /v1/speech-to-text` | `scribe*` models transcribe; the others, such as `eleven_flash_v2_5`, speak |
| `deepgram` | Aura, `POST /v1/speak` | Pre-recorded, `POST /v1/listen` | `aura*` models speak; the others, such as `nova-3`, transcribe |
| `assemblyai` | not offered | Upload, start a transcript and wait for it | every model, such as `universal` or `slam-1` |

```yaml
providers:
  eleven: {type: elevenlabs, api_key: ${ELEVENLABS_API_KEY}}
  deepgram: {type: deepgram, api_key: ${DEEPGRAM_API_KEY}}
  assemblyai: {type: assemblyai, api_key: ${ASSEMBLYAI_API_KEY}, timeout: 10m}
models:
  speech:
    targets:
      - {provider: eleven, model: eleven_flash_v2_5, price: {per_mchars: 50}}
      - {provider: deepgram, model: aura-2-thalia-en, price: {per_mchars: 30}}
  transcribe:
    targets:
      - {provider: deepgram, model: nova-3, price: {per_audio_minute: 0.0043}}
      - {provider: assemblyai, model: universal, price: {per_audio_minute: 0.0025}}
```

- **Speech.** `input` is the text. ElevenLabs takes an ElevenLabs voice ID as `voice` and `speed` as its voice speed. Deepgram's model names the voice, so `voice` is not used. `response_format` is mp3 (the default), opus, wav or pcm on ElevenLabs, and also aac and flac on Deepgram; pcm and wav are 24 kHz 16-bit mono, as OpenAI's are. Characters are counted as for `openai` targets.
- **Transcription.** The `file` is sent as the provider expects it, with `language` when given and language detection otherwise; AssemblyAI also takes `prompt`. `response_format` is json, text, verbose_json (with `words` when the provider returns word times), srt or vtt. Translations are not offered.
- **Audio minutes.** Deepgram and AssemblyAI report the audio's length, which is metered. Scribe does not, so the gateway meters the end time of the last word, which can be a little shorter than the file.
- **AssemblyAI is asynchronous.** The gateway uploads the file, starts a transcript, checks it every second, and answers when it is ready, within the provider's `timeout` (default 5 minutes; long recordings need more). Then it deletes the transcript and its audio from AssemblyAI, which would otherwise keep them. A transcript that is not ready in time answers `504`.
- Upstream errors keep their status, so fallback works as for other targets, and their message is passed in OpenAI's error shape.

### Image providers with their own APIs

| Type | Default `base_url` | Models | What it calls |
| --- | --- | --- | --- |
| `stability` | `https://api.stability.ai` | `stable-image-core`, `stable-image-ultra`, and `sd3*` models such as `sd3.5-large` | `POST /v2beta/stable-image/generate/{core,ultra,sd3}`, once per image |
| `fal` | `https://fal.run` | any fal text to image endpoint id, such as `fal-ai/flux/dev` | `POST /{model}`, the synchronous API |

- `prompt`, `n` (at most 4), `size`, `seed` and `negative_prompt` carry over. Stability takes the nearest of its aspect ratios to `size`; fal takes the exact width and height.
- Stability answers `data[].b64_json` and fal answers `data[].url` on fal's CDN, which is how each returns images. `output_format` is png, jpeg or webp on Stability, png or jpeg on fal.
- Images that a provider's safety filter blocks are left out and not counted, as for Imagen.
- Image edits stay on `openai` targets.

### Cohere

The `cohere` type (default `base_url` `https://api.cohere.com`) serves chat and embeddings through Cohere's OpenAI compatibility API, and `rerank` through Cohere's own `/v2/rerank`, which takes the same `query` and `documents` as the gateway's endpoint. `rerank*` models rerank, `embed*` models embed, and the others chat. Mistral needs no type of its own: its API is OpenAI-compatible, including transcription with Voxtral, so it is an `openai` provider (see the catalog preset).

### Veo on the Gemini API

A `gemini` target whose model starts with `veo-`, such as `veo-3.0-generate-001`, serves `videos` as a job, like OpenAI's:

- **Create** calls `predictLongRunning` with the `prompt`, `seconds` as `durationSeconds` (default 8) and `size` as Veo's `aspectRatio` (16:9 or 9:16) and `resolution` (720p, or 1080p when the short side is 1080 or more). The answer is OpenAI's video object, whose `id` encodes the Veo operation, and the requested seconds are metered. `input_reference` and remix are not offered for Veo.
- **Retrieve** reads the operation: `in_progress` until it is done, then `completed`, or `failed` with the reason when Veo filtered the video or reported an error.
- **Content** downloads the video from the Gemini API's file service with the provider's key. The gateway only sends the key to the provider's own host.
- **Delete** forgets the job in the gateway; Veo keeps generated videos for two days and has no delete.

### Nova Reel on Bedrock

A `bedrock` target whose model is Nova Reel, such as `amazon.nova-reel-v1:1`, serves `videos` when the provider sets `s3_output_uri`, an S3 folder in the provider's region that Bedrock can write to:

```yaml
providers:
  bedrock:
    type: bedrock
    region: us-east-1
    s3_output_uri: s3://acme-videos/reel
```

- **Create** calls StartAsyncInvoke. Six seconds (the default) is one shot; `seconds` may also be a multiple of six up to 120, which makes an automated multi-shot video. Nova Reel makes 1280x720 at 24 frames a second, so other sizes get `400`. The requested seconds are metered.
- **Retrieve** reads the invocation with GetAsyncInvoke: `in_progress`, `completed`, or `failed` with Bedrock's message.
- **Content** reads `output.mp4` from the invocation's folder in S3, signed with SigV4. S3 does not take Bedrock API keys, so this needs credentials from the AWS default chain with `s3:GetObject` on the folder; with only an API key, content answers `501`.
- **Delete** forgets the job in the gateway and leaves the video in the bucket, where your lifecycle rules apply.

Veo through Vertex AI and cloud rerank stay in issue #35: Vertex writes videos to Cloud Storage, which needs a design of its own.

### Gemini Live

The gateway relays the Gemini Live API at Google's own path, `GET /ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent`, so Google's SDKs work with the gateway as their base URL and a gateway key as their API key (in the `key` query parameter, `x-goog-api-key`, or a bearer token).

- The client's first message is its `setup`, whose `model` names a virtual model, such as `models/live`. The gateway checks it as it checks the `model` of any request, then dials the first target that accepts (a `gemini` provider with a Live model such as `gemini-live-2.5-flash-preview` or a `native-audio` model), and sends the setup with the target's model. A later `setup` is dropped, so a client cannot change models.
- Every server message with `usageMetadata` is charged, with audio tokens at the audio prices, and the request line totals the session, as for OpenAI realtime.
- Before the setup is read the session is already a WebSocket, so a refused model closes it with code 1008 and the reason, rather than an HTTP status.
- Keys with guardrails are refused, budgets close a session after the message that spent them, and `limits.max_realtime_session` applies.

### Nova Sonic

Bedrock serves Nova Sonic over `InvokeModelWithBidirectionalStream`, an HTTP/2 request whose body and response are both AWS event streams. Browsers and most WebSocket libraries cannot speak that, and clients would need AWS credentials, so the gateway offers it as a WebSocket at `GET /bedrock/model/{model}/invoke-with-bidirectional-stream`, where `{model}` is a virtual model:

- Each WebSocket message, either way, is one Nova Sonic event's JSON, such as `{"event":{"sessionStart":{...}}}` or `{"event":{"audioOutput":{...}}}`: the payload Bedrock carries base64 encoded in its event stream chunks. Clients build events exactly as in AWS's Nova Sonic examples and skip the event stream framing.
- The gateway key goes in a bearer token, `x-api-key`, or the `key` query parameter for browsers.
- The gateway opens the stream to the first target that accepts it (a `bedrock` provider with a model whose id contains `-sonic-`, such as `amazon.nova-sonic-v1:0`), sending the client's first event with it, so the client sends `sessionStart` before it expects output. Each later event is sent as it arrives. With SigV4, each message is signed in a chain from the request's signature, as the AWS SDKs do; with a Bedrock API key, the key is sent as a bearer token.
- Each `usageEvent` is charged by its `delta`: speech tokens at the audio prices, text tokens at the token prices. When the client closes, the gateway ends the input and waits up to five seconds for the last usage.
- An upstream exception closes the WebSocket with code 1011 and the exception's message; the end of the stream closes it normally.
- Keys with guardrails are refused, budgets close a session after the event that spent them, and `limits.max_realtime_session` applies.

```yaml
models:
  voice:
    targets:
      - provider: bedrock
        model: amazon.nova-sonic-v1:0
        price: {input_per_mtok: 0.06, output_per_mtok: 0.24, audio_input_per_mtok: 3.4, audio_output_per_mtok: 13.6}
```

WebRTC is not in this design: its media goes from the client straight to the provider, so the gateway could neither meter nor guard it.

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

- Batch (`/v1/batches`, `/v1/files`) has its own design in [batch.md](batch.md). The semantic cache is designed in [cache.md](cache.md). WebRTC stays in issue #35 for later.
