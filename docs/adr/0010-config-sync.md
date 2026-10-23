# 0010. Data plane serves from a local snapshot

- Status: accepted
- Date: 2026-10-22

## Decision

In v0.1 every gateway replica is both control and data plane: it serves the admin API and loads snapshots from the shared store, polling for changes every 5 seconds (configurable) and refreshing immediately after its own admin writes.

In v1.0 a separate control plane can stream versioned snapshots over gRPC; data planes keep the last good snapshot on disk and keep serving if the control plane is unreachable.

## Consequences

v0.1 stays one binary. Key revocation takes effect within the poll interval on other replicas.
