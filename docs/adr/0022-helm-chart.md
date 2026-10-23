# 0022. A basic Helm chart without bundled databases

- Status: accepted
- Date: 2026-10-23

## Context

v0.5 adds Kubernetes support ([helm.md](../helm.md)). Charts for comparable gateways often bundle Postgres and Redis subcharts, which are convenient for a demo and the wrong place for production data. An operator and CRDs are planned for v1.0.

## Decision

1. One Helm chart in this repository, versioned with the gateway and published as a signed OCI artifact on GHCR.
2. The chart runs the gateway only. Postgres, Valkey, secrets and certificates are referenced, not created.
3. The gateway's own `config.yaml` is a single chart value, and secrets reach it as environment variables from Kubernetes secrets.
4. The chart refuses several replicas with single-node storage or limits.
5. Migrations stay in the gateway's startup, made safe for parallel starts by a Postgres advisory lock.

## Consequences

- One config format across the binary, Compose and Kubernetes, and a small chart to keep correct.
- Installing needs a database and Valkey first, which the docs explain. There is no one-command demo on Kubernetes; Compose remains the quick start.
- No migration Job, so no ordering problems with GitOps tools.
