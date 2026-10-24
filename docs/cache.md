# Response cache

Teams send the same prompts again and again: evaluation runs, retries, FAQ bots, embeddings of documents that did not change. A cache in the gateway answers those from memory or Valkey instead of the provider, which saves their cost and latency. This design adds an exact cache and an opt-in semantic cache, turned on per organization, team or key, with hits and savings shown in the admin UI. It is tracked in issue #35, and the decision is ADR 0026.

## What is cached

- **Endpoints:** `POST /v1/chat/completions`, `POST /v1/messages` and `POST /v1/embeddings`, streamed or not. Other endpoints (Responses with its stored state, media, batches, realtime) are never cached.
- **Only successful answers:** status 200, up to `cache.max_entry_bytes` (default 1 MiB). The stored answer is what the client received, after output guardrails.
- **Never across teams:** a cache entry belongs to one team (to one key, for static keys from the config file). Nothing a team caches is ever served to another.

## Turning it on

A `cache` field joins the policy at each level. The most specific level that sets it decides, as with `log_bodies`:

```yaml
cache:
  mode: exact      # off, exact or semantic
  ttl: 1h          # how long an answer is reused; at most 30 days
```

Off is the default, because a cached answer for a prompt sent with a non-zero temperature is the same answer every time, which some workloads do not want. Operators set it in the policy form of an organization, team or key in the admin UI, or in the config file for static keys.

A client can skip the cache for one request with `Cache-Control: no-cache` (look up nothing, still store the answer) or `Cache-Control: no-store` (neither look up nor store).

## Exact matches

The cache key is a SHA-256 of the team, the endpoint, the virtual model, the guardrails that apply to the caller, and the request body with `model`, `user` and `metadata` removed, its JSON keys sorted. Two requests that differ in any other field, such as `temperature`, `tools` or `stream`, are different entries. A streamed request replays the stored event stream, so the client sees the same chunks.

## Semantic matches

With `mode: semantic`, a chat or Messages request that misses exactly is matched by meaning:

- The gateway embeds the text of the last user message with `cache.semantic.model`, a virtual model that serves embeddings. The embedding call is metered to the caller like any request, as endpoint `cache.embeddings`.
- Everything else must still match exactly: the system prompt, earlier turns, parameters and tools. Only the last user message is compared by meaning, so a paraphrased question in the same conversation hits, and the same question in a different conversation does not.
- The nearest stored question with cosine similarity of at least `cache.semantic.threshold` (default 0.95) is served.

The vectors are kept in memory on each replica, up to `cache.semantic.max_entries` (default 10,000, oldest dropped first). The answers they point to live in the shared store, so with Valkey every replica serves every exact hit, while a semantic hit needs the replica that stored the question. That trades some hit rate for not needing a vector database.

## Where entries live

With `limiter.driver: valkey`, entries are stored in that Valkey under `gog:cache:<org>:<team>:`, so every replica shares them and they expire with their TTL. Otherwise they are held in memory on each replica, up to `cache.max_memory_bytes` (default 256 MiB), least recently used first out.

## The request path

1. Authenticate, check the model, count the request against rate limits, and run input guardrails. Hits still count toward requests per minute, and a blocked prompt is never answered from the cache. A redacted prompt is looked up redacted.
2. Look up the cache. A hit answers with the stored body and content type, `X-Gateway-Target` of the target that produced it, and `X-Gog-Cache: hit` or `X-Gog-Cache: semantic`.
3. On a miss, route as usual and answer with `X-Gog-Cache: miss`. A 200 answer within the size limit is stored with its cost.

## Metering

A hit writes a usage record with no tokens and no cost, marked as a cache hit, with the cost of the answer it reused as `saved_usd`. Hits do not spend budgets or tokens per minute. Usage reports gain `cache_hits` and `saved_usd`, and the CSV export carries them.

## Admin API and UI

- The policy form of organizations, teams and keys gains a **Response cache** setting: inherit, off, exact or semantic, with a TTL.
- The **Usage** page shows cache hits and money saved, overall and per group, and the Requests page marks cached answers.
- The **Settings** page shows the cache configuration.
- `DELETE /admin/v1/cache?org_id=&team_id=` clears an organization's or a team's entries, for an answer that went stale. Owners and the organization's admins may; it is audited as `cache.clear`. The organization page has the button.

## Configuration

```yaml
cache:
  max_entry_bytes: 1048576
  max_memory_bytes: 268435456   # memory store only
  semantic:
    model: embed                # required for mode: semantic
    threshold: 0.95
    max_entries: 10000
```

A policy that asks for `semantic` when `cache.semantic.model` is not set is refused when it is saved.

## Testing

- Cache store tests on memory and Valkey: TTL, size bounds, LRU eviction, clearing by prefix.
- Server tests: an exact hit with no upstream call and zero cost; streams replayed; teams and static keys never sharing entries; guardrails in the key; `no-cache` and `no-store`; only 200s stored; the size limit; semantic hits above the threshold and misses below it or with a different context; usage records with `saved_usd`; clearing with audit and role checks.
- The OpenAI SDK conformance run sends a cached request twice and checks the second is a hit and costs nothing.
- The browser test sets a team's cache and sees the Usage page's cache figures.

## Not in this design

- A shared vector index across replicas (Valkey search or pgvector).
- Caching Responses API, media or MCP tool calls.
- Per-model cache settings.
