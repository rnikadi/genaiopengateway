# 0005. Valkey for shared counters, memory for single node

- Status: accepted
- Date: 2026-10-22

## Context

Rate limits and budgets must hold across replicas. Redis changed to non-open-source licenses in 2024; Valkey is the BSD-licensed fork.

## Decision

A `limiter` interface with an in-memory implementation and a Valkey implementation (any Redis-protocol server works). Rate limits use fixed one-minute windows with atomic increments. Budget spend is tracked as a counter per scope and period, seeded from the store on startup.

If Valkey is unreachable the gateway fails open on rate limits and closed on budgets by default; both are configurable.

## Consequences

- Single-node installs need nothing extra.
- Fixed windows allow short bursts at window edges; acceptable for v0.1 and simpler than sliding logs.
