# 0002. Typed request envelope with pass-through of unknown fields

- Status: accepted
- Date: 2026-10-22

## Context

The gateway must read some request fields (model, stream, max tokens, messages for translation) but providers add fields faster than we can model them.

## Decision

The gateway decodes only the fields it needs into typed structs and keeps the full request as raw JSON. For OpenAI-compatible upstreams it rewrites only `model` (and `stream_options.include_usage` when streaming) and forwards every other field untouched. Translating adapters build the upstream request from the typed fields and document which fields they support.

## Consequences

- New OpenAI parameters work with OpenAI-compatible upstreams with no gateway release.
- Translating adapters reject or ignore unsupported fields explicitly, never silently corrupt them.
