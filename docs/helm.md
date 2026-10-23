# Helm chart

Docker Compose is the local setup. Production for most enterprises is Kubernetes, and today every team writes its own manifests. This design adds a basic Helm chart that runs the gateway the way the project recommends: several replicas, Postgres and Valkey outside the chart, secrets from Kubernetes secrets, and the hardening the image already supports. Decisions are recorded in [ADR 0022](adr/0022-helm-chart.md). An operator and CRDs stay in v1.0.

## What the chart creates

| Object | Notes |
| --- | --- |
| `Deployment` | 2 replicas by default. Non-root, read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp, no service account token mounted. Liveness on `/healthz`, readiness on `/readyz`. Spread across nodes and zones with topology spread constraints. |
| `ConfigMap` | `config.yaml` from the `config` value, verbatim. A checksum annotation rolls the pods when it changes. |
| `Service` | `ClusterIP` on port 8080, plus the metrics port when `metricsPort` is set to match `metrics.listen`. |
| `PodDisruptionBudget` | `maxUnavailable: 1` when there are 2 or more replicas. |
| `Ingress` | Optional, off by default. |
| `HorizontalPodAutoscaler` | Optional, on CPU, off by default. |
| `ServiceMonitor` | Optional, for the Prometheus operator, off by default. |
| `NetworkPolicy` | Optional: ingress to port 8080 from chosen namespaces; egress left open, because providers and MCP servers are on the internet. |

It does **not** create Postgres, Valkey, secrets or certificates. Each company already runs these its own way (a managed database, an operator, External Secrets, cert-manager), and a chart that bundles them ends up running production data in a dev-grade setup.

## Values

```yaml
replicaCount: 2
image:
  repository: ghcr.io/rnikadi/genaiopengateway
  tag: ""                       # defaults to the chart's appVersion
  digest: ""                    # set to pin the signed image by digest

config: |                       # the gateway's config.yaml, see config.md
  store:
    driver: postgres
    dsn: ${DATABASE_URL}
  limiter:
    driver: valkey
    address: valkey.cache.svc:6379
    password: ${VALKEY_PASSWORD}
  admin:
    token: ${GATEWAY_ADMIN_TOKEN}
  ...

envFrom:                        # secrets referenced from config as ${VAR}
  - secretRef: {name: genaiopengateway}
env: []

resources:
  requests: {cpu: 250m, memory: 256Mi}
  limits: {memory: 1Gi}

serviceAccount:
  create: true
  annotations: {}               # for workload identity: GKE, EKS IRSA or Pod Identity, Azure

ingress: {enabled: false}
autoscaling: {enabled: false, minReplicas: 2, maxReplicas: 10, targetCPUUtilizationPercentage: 70}
serviceMonitor: {enabled: false}
metricsPort: null              # set when config has metrics.listen, such as 9090
networkPolicy: {enabled: false}
podAnnotations: {}
nodeSelector: {}
tolerations: []
affinity: {}
```

The config is the same file as everywhere else, so nothing about the gateway is configured twice. Secrets stay out of values: the config refers to `${VAR}` and the pod gets the variables from a secret. Service account annotations let the cloud adapters ([cloud-providers.md](cloud-providers.md)) use workload identity instead of keys.

## Checks in the chart

The chart refuses to render (`fail` in the template) when:

- `replicaCount` or `autoscaling.minReplicas` is above 1 and the config uses `store.driver: sqlite` or `limiter.driver: memory`. Several replicas need Postgres and Valkey to share keys, counters and budgets (ADRs 0004 and 0005).
- `config` is empty.

## Changes to the gateway

Two small changes make it safe to run several replicas from the first install:

1. **Migration lock.** Replicas starting together could run the same migration at once, and all but one would crash and restart. On Postgres, migrations take a `pg_advisory_lock` first, so one replica migrates and the others wait.
2. **Graceful shutdown.** On `SIGTERM` the gateway already stops accepting and drains requests. `/readyz` will fail from the moment shutdown starts, so the endpoint is removed from the Service before the listener closes. The chart sets `terminationGracePeriodSeconds` to 60 for long streams.

## Publishing

- The chart lives in `deploy/helm/genaiopengateway`, versioned with the gateway: chart version and `appVersion` both equal the release version.
- The release workflow packages it, pushes it to `oci://ghcr.io/rnikadi/charts/genaiopengateway`, and signs it with cosign like the images.
- Install: `helm install gateway oci://ghcr.io/rnikadi/charts/genaiopengateway --version 0.5.0 -f values.yaml`.

## Testing

- CI runs `helm lint` and renders the chart with the default values and with every optional object on, then checks the output with `kubeconform`.
- A CI job starts a kind cluster with Postgres and Valkey, installs the chart with 2 replicas, waits for both pods to be ready, and sends a chat request through a fake provider and an admin API call. This also exercises the migration lock.
- Template tests check that the replica guard fails as intended.

## Not in scope

| Feature | Why |
| --- | --- |
| Bundled Postgres and Valkey subcharts | See above; the deploy guide links to the usual operators instead. |
| An operator, CRDs for organizations, keys or MCP servers | v1.0, with the separate control plane. |
| Secrets management | External Secrets, sealed secrets or the cloud's secret store, referenced through `envFrom`. |
| Running migrations as a Job or hook | The lock makes startup migration safe; a Job adds ordering problems with Argo CD and Flux. |
