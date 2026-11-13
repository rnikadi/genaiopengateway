# GenAIOpenGateway Helm chart

Runs the gateway on Kubernetes with several hardened replicas against your own Postgres and Valkey. See [Deploying to Kubernetes](../../../docs/deploy-kubernetes.md) for installing it and [helm.md](../../../docs/helm.md) for the design. Every value is described in [values.yaml](values.yaml).

`ci/` holds the checks CI runs: `test.sh` (lint, schemas, guards) and `kind.sh` (a 2-replica install with traffic).
