# 0034: Data planes read versioned snapshots from a control plane over HTTP

- Status: accepted; refines [0010](0010-config-sync.md)
- Date: 2026-11-11

## Context

ADR 0010 planned a separate control plane for v1.0: data planes would receive versioned snapshots from it, keep the last good one on disk, and keep serving if it became unreachable.

Until now every replica was both control and data plane. Each one held the database credentials, served the admin API, and read the store every few seconds. Operators asked for two things: replicas at the edge or in other regions that never hold database credentials or expose the admin API, and replicas that can restart while the database is down.

ADR 0010 named gRPC streaming as the transport. Building it would add a protocol, generated code and a second port, for a snapshot that is small and changes rarely.

## Decision

- Any replica with a store can act as the control plane: with `data_planes.token` set, it serves `GET /cp/v1/snapshot` and `POST /cp/v1/records/{usage|mcp_calls|guardrail_events}` to callers presenting that token.
- A data plane is configured with `control_plane: {url, token, cache_file}` and no `store`. On each `store.refresh_interval` it asks for the snapshot with the version it holds as `If-None-Match`, and gets `304` or the new snapshot.
- The snapshot holds everything a replica reads from the store to serve requests: stored configuration objects, organizations, teams, unrevoked keys (by hash), team credentials and Cedar policies. Secrets stay sealed with `secrets.encryption_key`, which both planes share. Its version is a hash of its content.
- The data plane writes each new snapshot to `cache_file` (mode 0600, replaced atomically), and loads it when it starts while the control plane is unreachable. It reports ready only once it holds a snapshot.
- Usage, MCP call and guardrail records are batched and posted to the control plane, which stores them and publishes them to streams. Failed batches are retried, within the writer's bound.
- Gateway-wide settings stay in each data plane's file. Features that read or write the store while serving a request are not available on data planes: video jobs, files and batches, A2A task ownership, body logging, per-user MCP credentials, MCP OAuth callers and strict tool pinning. Those requests go to replicas with a store.

## Consequences

- Data planes hold no database credentials and serve no admin API or UI.
- Revocation reaches data planes within one refresh interval, as it reaches replicas today.
- A control plane outage does not stop data planes, even across restarts, but changes made meanwhile wait until it is back.
- Rate limits and budgets are shared only when every plane uses the same Valkey.
- A record batch that reached the control plane but whose answer was lost can be stored twice.
