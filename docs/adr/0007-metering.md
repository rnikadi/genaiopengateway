# 0007. Per-modality usage units and an editable price catalog

- Status: accepted
- Date: 2026-10-22

## Context

Text is priced per token, speech per character or second, images per image, video per second. Prices differ per contract.

## Decision

A usage record holds: request id, time, org, team, key, virtual model, provider, upstream model, endpoint, status, latency, and a set of units: `input_tokens`, `output_tokens`, `cached_input_tokens`, `reasoning_tokens`, and later `characters`, `audio_seconds`, `images`, `video_seconds`. Cost in USD is computed at record time from the price catalog.

Prices live in YAML on each model target (`price: {input_per_mtok, output_per_mtok, cached_input_per_mtok}`). There is no built-in price list in v0.1; an unpriced model costs zero and is flagged in metrics.

For streaming, the gateway asks OpenAI-compatible upstreams for a final usage chunk (`stream_options.include_usage`) and reads usage from the stream as it passes through.

## Consequences

- Budgets are only as accurate as the configured prices.
- Upstreams that report no usage are metered as zero tokens and counted in a dedicated metric.
