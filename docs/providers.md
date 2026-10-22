# Provider adapters

An adapter connects one provider API to the gateway. v0.1 ships two: `openai` and `anthropic`.

## Interface

Each adapter declares the endpoints it serves and implements one method per endpoint family:

- **Passthrough endpoints** (the provider's own API shape, for example `openai` serving `/v1/chat/completions` or `anthropic` serving `/v1/messages`) forward the raw body with only `model` rewritten.
- **Translated endpoints** (for example `anthropic` serving `/v1/chat/completions`) convert the request, the response, every streaming event and errors.

Every adapter must report usage (`input_tokens`, `output_tokens`, `cached_input_tokens`, `reasoning_tokens`) from responses and streams so metering works.

## Anthropic translation rules (chat completions)

| OpenAI | Anthropic |
| --- | --- |
| `system` / `developer` messages | joined into `system` |
| `user`, `assistant` messages with text or image parts | `messages` with `text` and `image` blocks (base64 data URLs and https URLs) |
| `assistant.tool_calls` | `tool_use` blocks |
| `tool` messages | `tool_result` blocks in a user message; consecutive results are merged |
| `tools` (function type) | `tools` with `input_schema` |
| `tool_choice` `auto` / `none` / `required` / named | `auto` / `none` / `any` / `tool` |
| `max_tokens`, `max_completion_tokens` | `max_tokens` (default 4096 when absent, since Anthropic requires it) |
| `temperature`, `top_p`, `stop` | `temperature`, `top_p`, `stop_sequences` |
| `stream`, `stream_options.include_usage` | `stream`; usage is always reported in the final chunk |
| `stop_reason` `end_turn` / `max_tokens` / `tool_use` / `stop_sequence` | `finish_reason` `stop` / `length` / `tool_calls` / `stop` |
| `usage.input_tokens + cache_read + cache_creation`, `output_tokens` | `usage.prompt_tokens`, `completion_tokens`, `prompt_tokens_details.cached_tokens` |

Cache writes (`cache_creation_input_tokens`) are counted as input tokens at the input price; Anthropic bills them at a premium, so budgets for prompt-caching workloads run slightly low.

Unsupported fields (`n` greater than 1, `logprobs`, `response_format` of type `json_schema`) are rejected with `400 invalid_request` naming the field, never silently dropped. `response_format: json_object` is accepted and ignored with a warning header, as Claude follows JSON instructions in the prompt.

## Conformance tests

Each adapter has table-driven tests against recorded fixtures (request in, expected upstream request, upstream response, expected client response), covering:

1. Non-streaming text.
2. Streaming text, checking each event and the final usage chunk.
3. Tool definitions, a tool call, and a tool result round trip.
4. Images in input.
5. Upstream 400, 401, 429 and 500 errors mapped to the right client errors.
6. Usage extraction, including cached tokens.

Live tests against real providers run only when the provider's API key is present in the environment, and never in pull request CI.

## Adding a provider

1. Prefer the `openai` type if the provider has an OpenAI-compatible API; no code is needed.
2. Otherwise add a package under `internal/provider/<name>`, implement the interface, and add fixtures for every case above.
3. Document the provider type and its translation table in this file.
