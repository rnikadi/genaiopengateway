# Gemini, Vertex AI and Bedrock adapters

Most enterprises buy models through their cloud: Gemini on Google Cloud, Claude and others on Vertex AI or Amazon Bedrock, with the cloud's identity, billing, data residency and quotas. Today the gateway reaches them only through OpenAI-compatible endpoints with static keys, which miss cloud credentials, Claude on the clouds and accurate usage for thinking models. This design adds three adapter types: `gemini`, `vertex` and `bedrock`. Decisions are recorded in [ADR 0021](adr/0021-cloud-providers.md). The adapter interface, error handling and conformance tests are those of [providers.md](providers.md).

## What each type serves

| Type | Auth | Chat completions | Messages (Anthropic) | Gemini `generateContent` | Embeddings |
| --- | --- | --- | --- | --- | --- |
| `gemini` | API key | translated to `generateContent` | no | passthrough | later |
| `vertex` | Google credentials | Gemini models translated; Claude models through the existing Anthropic translation | Claude models, passthrough | Gemini models, passthrough | later |
| `bedrock` | Bedrock API key, or AWS credentials with SigV4 | translated to Converse, for every Bedrock model | Claude models, passthrough through InvokeModel | no | Titan Text Embeddings V2 and Cohere Embed |

Chat completions work with streaming, tools, images as data URLs, and usage, for every type. The Responses API stays OpenAI-only for now.

## Config

```yaml
providers:
  gemini:
    type: gemini
    api_key: ${GEMINI_API_KEY}           # sent as x-goog-api-key
    # base_url: https://generativelanguage.googleapis.com   (default)
  vertex:
    type: vertex
    project: acme-ai-prod
    location: global                     # global, us, eu, or a region such as europe-west4
    # credentials_file: /var/run/secrets/gcp/key.json   (default: Application Default Credentials)
  bedrock:
    type: bedrock
    region: eu-central-1
    # api_key: ${AWS_BEARER_TOKEN_BEDROCK}   (default: the AWS credential chain, signed with SigV4)

models:
  gemini-pro:
    targets:
      - {provider: vertex, model: gemini-2.5-pro}
      - {provider: gemini, model: gemini-2.5-pro}
  claude-sonnet:
    targets:
      - {provider: bedrock, model: eu.anthropic.claude-sonnet-4-6}
      - {provider: vertex, model: anthropic/claude-sonnet-4-6}
  embed:
    targets:
      - {provider: bedrock, model: amazon.titan-embed-text-v2:0}
```

- On `vertex`, a target model is `publisher/model`. `google/` may be left out, so `gemini-2.5-pro` means `google/gemini-2.5-pro`. Only `google` and `anthropic` are supported.
- On `bedrock`, a target model is a model id, an inference profile id (such as `eu.anthropic.claude-sonnet-4-6`) or an ARN. Claude models are recognized by `anthropic.` in the id.
- `base_url` can be set on every type, for private endpoints (Private Service Connect, VPC endpoints) and for tests.

Startup fails if `vertex` has no `project` or `location`, or `bedrock` has no `region`.

## Credentials

**Google.** `vertex` uses Application Default Credentials through `golang.org/x/oauth2/google`: a service account key file, workload identity federation, or the metadata server on GKE and Cloud Run. Tokens have the `cloud-platform` scope and are cached and refreshed before they expire. `credentials_file` names a key file explicitly.

**AWS.** `bedrock` uses a Bedrock API key as a bearer token when `api_key` is set. Otherwise it loads the default AWS credential chain (environment, shared config and SSO, web identity for EKS, ECS task roles, EC2 instance profiles) and signs each request with SigV4. Only the `config`, `credentials`, `signer/v4` and `eventstream` parts of `aws-sdk-go-v2` are used, not the generated Bedrock client, so request bodies stay plain JSON like every other adapter.

Credentials never leave the adapter. A failure to get a token is a `502 upstream_auth_failed` that is eligible for fallback, and it is logged without the credential.

## Gemini translation (chat completions)

Used by `gemini`, and by `vertex` for Google models. The request and response bodies are the same on both; only the URL and auth differ.

| OpenAI | Gemini |
| --- | --- |
| `system` / `developer` messages | joined into `systemInstruction` |
| `user`, `assistant` messages | `contents` with roles `user` and `model` |
| text parts, image parts as data URLs | `text` and `inlineData` parts. Image URLs are rejected with `400`, as the gateway does not fetch URLs. |
| `assistant.tool_calls` | `functionCall` parts. Gemini's thought signature is returned in `tool_calls[].extra_content.google.thought_signature`, as Google's own OpenAI compatibility layer does, and sent back when the client includes it. |
| `tool` messages | `functionResponse` parts; the function name is found from the matching `tool_call_id` earlier in the request, and a `tool_call_id` that matches no earlier call is a `400`. A result that is a JSON object is sent as the response; any other text is sent as `{"result": text}`. |
| `tools` (function type) | `functionDeclarations` with the JSON Schema as `parametersJsonSchema` |
| `tool_choice` `auto` / `none` / `required` / named | `functionCallingConfig.mode` `AUTO` / `NONE` / `ANY` / `ANY` with `allowedFunctionNames` |
| `max_tokens`, `max_completion_tokens` | `generationConfig.maxOutputTokens` |
| `temperature`, `top_p`, `stop`, `seed` | `temperature`, `topP`, `stopSequences`, `seed` |
| `response_format` `json_object` / `json_schema` | `responseMimeType: application/json`, plus `responseJsonSchema` |
| `reasoning_effort` `minimal` / `low` / `medium` / `high` | `thinkingConfig.thinkingLevel` (Gemini 3 and later; older models reject it upstream) |
| `stream` | `streamGenerateContent?alt=sse` |
| `finishReason` `STOP` / `MAX_TOKENS` / `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII` | `finish_reason` `stop` / `length` / `content_filter`; `tool_calls` when the reply has function calls |
| `usageMetadata` | `prompt_tokens` = `promptTokenCount`; `completion_tokens` = `candidatesTokenCount` + `thoughtsTokenCount`; `reasoning_tokens` = `thoughtsTokenCount`; `cached_tokens` = `cachedContentTokenCount` |

Unsupported fields (`n` greater than 1, `logprobs`, audio parts) are rejected with `400 invalid_request` naming the field. A prompt blocked by Gemini's own safety filter (`promptFeedback.blockReason`) becomes `400 content_filter`.

**Why not use Gemini's OpenAI-compatible endpoint?** It works with the `openai` type today, and stays documented. But it does not report thinking tokens, which are billed as output, so costs for Gemini 2.5 and later would be wrong, and it does not reach Claude on Vertex. A native translation also lets Google's own SDKs use the gateway through passthrough.

## Gemini passthrough

`gemini` and `vertex` also serve Google's own API shape, so the Google GenAI SDK, the Gemini CLI and ADK can use the gateway by changing their base URL:

- `POST /v1beta/models/{model}:generateContent` and `:streamGenerateContent`, where `{model}` is a virtual model.
- The client's gateway key is accepted in `x-goog-api-key`, as these clients send it.
- The body is forwarded unchanged; usage is read from `usageMetadata`.
- Streams are always server-sent events (`alt=sse`), which is what the Google SDKs ask for. A key in the `key` query parameter is not accepted, since URLs end up in logs.
- When an `output` guardrail redacts a stream, the client gets the whole redacted response as one event.

On `vertex` the gateway sends the request to the publisher model URL. Model allow lists, limits, budgets, guardrails and body logging apply as on every endpoint.

## Claude on Vertex AI

Claude on Vertex takes the Anthropic Messages body with two changes, and both endpoints reuse the `anthropic` adapter's code:

- The model goes in the URL: `…/publishers/anthropic/models/{model}:rawPredict`, or `:streamRawPredict` for streams. The `model` field is removed from the body.
- `anthropic_version: vertex-2023-10-16` is set in the body, and the `anthropic-version` header is dropped. `anthropic-beta` headers are forwarded.

`/v1/messages` is passthrough with these changes. `/v1/chat/completions` uses the existing Anthropic translation, then the same changes.

Host names follow the location: `aiplatform.googleapis.com` for `global`, `aiplatform.us.rep.googleapis.com` and `aiplatform.eu.rep.googleapis.com` for `us` and `eu`, and `{region}-aiplatform.googleapis.com` for a region.

## Bedrock translation (chat completions)

Chat completions use the Converse API, which has one request shape for every Bedrock model.

| OpenAI | Converse |
| --- | --- |
| `system` / `developer` messages | `system` text blocks |
| `user`, `assistant` messages | `messages` with `text` and `image` blocks (data URLs as `bytes`; URLs rejected with `400`) |
| `assistant.tool_calls` | `toolUse` blocks |
| `tool` messages | `toolResult` blocks in a user message; consecutive results are merged |
| `tools` | `toolConfig.tools[].toolSpec` with `inputSchema.json` |
| `tool_choice` `auto` / `required` / named | `auto` / `any` / `tool`. `none` is rejected with `400`, as Converse has no equivalent. |
| `max_tokens`, `temperature`, `top_p`, `stop` | `inferenceConfig.maxTokens`, `temperature`, `topP`, `stopSequences` |
| `stream` | `converse-stream`, which answers in the AWS event stream encoding; the adapter decodes it and emits OpenAI SSE |
| `stopReason` `end_turn` / `max_tokens` / `tool_use` / `stop_sequence` / `guardrail_intervened`, `content_filtered` | `finish_reason` `stop` / `length` / `tool_calls` / `stop` / `content_filter` |
| `usage.inputTokens + cacheReadInputTokens + cacheWriteInputTokens`, `outputTokens` | `prompt_tokens`, `completion_tokens`, `cached_tokens` = `cacheReadInputTokens` |

Cache writes are counted at the input price, as for Anthropic.

**Claude through `/v1/messages`** uses InvokeModel (`/model/{id}/invoke` and `invoke-with-response-stream`) with the body unchanged except that `model` is removed and `anthropic_version: bedrock-2023-05-31` is set. Stream chunks arrive as event stream frames carrying Anthropic events, and are re-emitted as Anthropic SSE, so Anthropic SDKs and Claude Code work unchanged.

**Embeddings** use InvokeModel with the model's own body: Titan Text Embeddings V2 (one input per call, so an input array becomes parallel calls) and Cohere Embed (arrays in one call). `dimensions` maps to Titan's `dimensions`. Token counts come from the `x-amzn-bedrock-input-token-count` header.

**Errors.** `ThrottlingException` and `ServiceQuotaExceededException` become `429`, `ValidationException` `400`, `AccessDeniedException` `403`, `ModelNotReadyException`, `ServiceUnavailableException` and `InternalServerException` `5xx`. Fallback follows the existing rules.

## Testing

- Each adapter gets the conformance fixtures from [providers.md](providers.md): plain and streamed text, tools, images, errors and usage, including thinking and cached tokens.
- Gemini: thought signatures round-trip through `extra_content`, and `tool` messages find their function names.
- Vertex: URL per location and publisher, Claude body changes, and token refresh with a fake token source.
- Bedrock: SigV4 headers against a fixed vector, a bearer key, event stream decoding from recorded frames, and embeddings token counts.
- Live tests run only when cloud credentials are present, never in pull request CI.

## Not in scope

| Feature | Why |
| --- | --- |
| Embeddings on Gemini and Vertex | Different endpoints and no token counts on the Gemini API. The `openai` type with Gemini's compatible endpoint covers them meanwhile. |
| Bedrock Guardrails, Knowledge Bases and Agents | Cloud features outside a model call. Bedrock Guardrails can still be attached to an inference profile on the AWS side. |
| Fetching image URLs for the client | It would let clients make the gateway fetch any URL. Clients send data URLs. |
| Provisioned throughput settings | Picked through the model id or ARN, which already works. |
| Azure OpenAI with Entra ID tokens | Azure's v1 API works with the `openai` type and an API key today. Entra ID auth can be added if asked. |
