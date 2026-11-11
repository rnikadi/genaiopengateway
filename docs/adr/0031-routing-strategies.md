# 0031: Routing strategies, retries and circuit breakers live in the router, per replica

- Status: accepted
- Date: 2026-11-11

## Context

Until now a virtual model tried its targets in the listed order and moved on after any 429, 5xx or connection error. Operators asked for the routing controls other gateways have: splitting traffic by weight, taking turns, preferring the fastest or cheapest target, keeping a user on one target for prompt caching, retrying a busy target before giving up on it, giving a hung target a short timeout, and no longer sending traffic to a target that keeps failing.

The question was where this state should live. Latency averages and breaker state change on every request. Sharing them across replicas through Valkey would put a network round trip on the hot path, which the benchmark gate (an added p50 under 5 ms) does not leave room for. It would also make routing depend on Valkey being up.

## Decision

- Each model has a `strategy`: `ordered` (the default, unchanged behavior), `weighted`, `round_robin`, `least_latency` or `lowest_cost`. The strategy only orders the targets. Every target after the first is still a fallback.
- `sticky: user` seeds the order from a hash of the request's user, for `weighted` and `round_robin`.
- `retries` gives each target extra tries with full-jitter exponential backoff, honoring `Retry-After` up to `max_backoff`. A target's `timeout` limits only the wait for response headers, so streams are not cut off.
- A circuit breaker, set globally under `routing.circuit_breaker`, opens after N failures in a row and admits a single probe after the cooldown. When every target is open, all of them are tried.
- Latency averages, round-robin counters and breakers are kept in memory on each replica, in a `router.Health` that outlives config reloads. Nothing about them is stored or shared.

## Consequences

- Routing adds no network calls and works without Valkey.
- With N replicas, a failing target takes up to N × `failures` failed tries before every replica opens its breaker. Round robin takes turns per replica, not across the fleet. Both are acceptable for the problems they solve.
- A restart forgets what a replica learned. Breakers start closed again, which at worst costs `failures` tries per target.
- The state is visible per replica through `GET /admin/v1/models` and the `gog_target_circuit_state` metric, which Prometheus sums across replicas.
