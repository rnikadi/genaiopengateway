# 0001. OpenAI-compatible API as the contract, plus native passthrough

- Status: accepted
- Date: 2026-10-22

## Context

Most SDKs, frameworks and tools speak the OpenAI HTTP API. Providers such as Anthropic expose features (prompt caching controls, extended thinking blocks) that do not map one-to-one onto it.

## Decision

The primary client contract is the OpenAI HTTP API (`/v1/chat/completions`, `/v1/embeddings`, `/v1/responses`, `/v1/models`, and later audio, images, realtime). Requests for models served by non-OpenAI providers are translated by an adapter.

Native provider APIs are also exposed as passthrough under their own paths (`/v1/messages` for Anthropic in v0.1) so clients that need provider-specific features keep them. Passthrough requests still go through authentication, limits, metering and logging.

We do not invent our own request API.

## Consequences

- Existing SDKs work by changing the base URL and key.
- Adapters must translate requests, responses, streaming events and errors, and must be covered by conformance tests.
- Features with no OpenAI equivalent are available only through passthrough.
