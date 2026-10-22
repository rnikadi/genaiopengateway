# 0004. Postgres for clusters, SQLite for single node

- Status: accepted
- Date: 2026-10-22

## Context

Enterprises run Postgres. Developers want a single binary with no dependencies.

## Decision

One `store` interface with two implementations: Postgres (via pgx) and SQLite (via the pure-Go modernc.org/sqlite driver, so builds stay CGO-free). SQL is written once in the common subset; migrations are embedded and applied at startup with a version table.

The request path never queries the store. Keys, teams and limits are loaded into an in-memory snapshot and refreshed every few seconds and on admin writes. Usage records are written asynchronously in batches.

## Consequences

- A database outage does not stop traffic; it delays key changes and usage writes.
- SQL must avoid dialect-specific features; this is checked by running the store tests against both backends in CI.
