# Routing and failover

A virtual model has one or more targets: a provider and an upstream model. For each request, the gateway puts the targets in an order and tries them one after another until one answers. The model's **strategy** chooses that order. **Retries**, **per-target timeouts** and the **circuit breaker** decide when to move on.

```yaml
routing:
  circuit_breaker: {failures: 5, cooldown: 30s}

models:
  chat:
    strategy: weighted
    sticky: user
    retries: {attempts: 2, backoff: 250ms, max_backoff: 5s}
    targets:
      - {provider: openai, model: gpt-4.1-mini, weight: 3, timeout: 10s}
      - {provider: anthropic, model: claude-haiku-4-5, weight: 1}
```

Every field here can also be set in the admin UI. Model fields are on the model's form on the **Models** page, and the circuit breaker is under **Settings → Limits**. Changes apply on every replica within `store.refresh_interval`.

## Strategies

| Strategy | First target | Fallbacks |
|---|---|---|
| `ordered` (default) | The first one listed | The rest, in the order listed |
| `weighted` | Drawn at random in proportion to `weight` (default 1) | Drawn the same way from the targets left |
| `round_robin` | The next one in turn, per model and replica | The rest, in the order listed after it |
| `least_latency` | The one with the lowest recent time to first byte | The others, fastest first |
| `lowest_cost` | The one with the lowest price | The others, cheapest first |

- **`least_latency`** keeps an exponentially weighted average of each target's time to first byte, with each answer counting 20%. A target that has not answered yet sorts first, so every target gets measured. The average belongs to the replica, so replicas can disagree for a while.
- **`lowest_cost`** compares a typical request's price: three input tokens for each output token, plus per-request and media prices. Targets without prices count as free, so give every target a price.
- **Sticky routing** (`sticky: user`, for `weighted` and `round_robin` only) sends every request from one user to the same first target. Same-target traffic keeps a provider's prompt cache warm. The user is the OpenAI `user` field or the Anthropic `metadata.user_id` field. A request without either is routed as if sticky were off.

A request that continues an upstream job, such as polling a video, goes to the target that created it.

## When the gateway moves on

A try **fails** when the connection fails, the target's timeout passes, or the upstream answers 429 or a 5xx other than 501. Other answers, such as a 400, go back to the client as they are, because another target would refuse the request too.

- **Retries**: `retries.attempts` (0 to 5, default 0) is how many extra tries a target gets before the next target. The wait before retry *n* is drawn at random between 0 and `backoff × 2ⁿ⁻¹`, capped at `max_backoff`. When the upstream sends a `Retry-After` header no longer than `max_backoff`, the gateway waits that long instead. A longer `Retry-After` moves on to the next target at once.
- **Per-target timeout**: `timeout` on a target limits the wait for response headers. A streaming answer that has started is never cut off. The provider's own `timeout` still bounds the whole request.
- **Last target**: when every target has failed, the client gets the last target's error response. When the last target failed without a response (a timeout or a connection failure), the client gets `502 all_targets_failed`.

Every failed try is counted in `gog_fallbacks_total` and logged with the request.

## Circuit breaker

After `routing.circuit_breaker.failures` failed tries in a row (default 5), a target's breaker **opens**. Requests then skip that target for `cooldown` (default 30s). After the cooldown the breaker is **half open**: the next request tries the target as a probe, and other requests keep skipping it. A successful probe closes the breaker. A failed probe opens it for another cooldown. Set `failures: 0` to turn the breaker off.

If every target of a model is open, the gateway tries them all anyway, because a request that might work is better than one that is sure to fail.

Each replica keeps its own breakers, and config reloads don't reset them. You can see the state in two places:

- **Models page** of the admin UI, and `GET /admin/v1/models` (field `circuit` on each target). A target shows *Circuit open* or *Probing*.
- **Metric** `gog_target_circuit_state{target="provider/model"}`: 0 closed, 1 open, 2 half open. Alert on `max by (target) (gog_target_circuit_state) == 1` lasting longer than a few cooldowns.

See [ADR 0031](adr/0031-routing-strategies.md) for why routing state stays on each replica.

## Data residency

Label each provider with where it processes data, then pin organizations, teams or keys to labels:

```yaml
providers:
  openai-eu: {type: openai, base_url: https://eu.api.openai.com/v1, api_key: ${OPENAI_KEY}, residency: eu}
  bedrock-eu: {type: bedrock, region: eu-central-1, residency: eu}
  openai: {type: openai, base_url: https://api.openai.com/v1, api_key: ${OPENAI_KEY}, residency: us}
```

Set `residency: [eu]` on an organization, team or key: in the admin UI it's the **Data residency** field, and in the admin API it's the `residency` field. Requests from that key then go only to targets whose provider has one of the labels. The strategy, retries and the circuit breaker work as usual among those targets.

- Lists narrow level by level. A key in an organization pinned to `[eu, uk]` with its own `[eu]` uses only `eu` providers.
- A provider without a label never serves a pinned caller.
- When none of a model's targets qualify, the request is refused with `403 residency_not_allowed`, and nothing is sent upstream.
- The pins cover everything a caller's request sends upstream: model requests, batches, realtime sessions, and the guardrail classifier and semantic cache embedding calls made for the request. A guardrail or cache model with no allowed target fails like any other unavailable check, and its `on_error` setting decides what happens.
- Labels are free text. The gateway does not check that a provider really is where its label says, so match labels to the provider's own data processing terms.
