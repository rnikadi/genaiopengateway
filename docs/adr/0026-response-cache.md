# 0026: An opt-in response cache per team, with semantic matching on one replica's vectors

- Status: accepted
- Date: 2026-10-24

## Context

Repeated prompts cost the same each time they reach a provider. A gateway sees every request, so it can answer repeats itself. Caching model answers is risky in two ways: answers must never cross teams, and a cached answer is wrong for workloads that want a fresh sample. A semantic cache needs embeddings and a nearest-neighbour search, which could pull in a vector database.

## Decision

- Cache chat completions, Anthropic Messages and embeddings, streamed or not, when a policy turns it on. It is off by default and set at organization, team or key level, the most specific level deciding.
- Key entries by team, endpoint, virtual model, the caller's guardrails and the canonical request body, so entries never cross teams or guardrail sets.
- Look up after input guardrails and rate limits, and store only 200 answers within a size limit, as the client received them.
- Store entries in Valkey when the limiter uses it, else in a bounded in-memory LRU.
- For semantic matching, embed only the last user message through a configured virtual model, require everything else to match exactly, and keep vectors in memory on each replica.
- Record hits with zero cost and the cost they saved, and show hits and savings in the admin UI.

## Consequences

- Teams that opt in pay nothing for repeats, and operators see what the cache saved.
- Semantic hits depend on the replica that stored the question; exact hits are shared through Valkey.
- The usage table gains two columns. Earlier rows read as zero.
- A stale answer persists until its TTL ends or an operator clears the team's cache.
