# Deploying to Kubernetes

The Helm chart runs the gateway the way the project recommends for production: several replicas, Postgres and Valkey run outside the chart, secrets kept in a Kubernetes secret, and a hardened pod. The design and its reasons are in [helm.md](helm.md). For a laptop, use [Docker Compose](../deploy/compose/README.md) instead.

It needs Kubernetes 1.30 or later and Helm 3.

## Before you install

1. **Postgres and Valkey.** Use a managed database and cache, or an operator such as CloudNativePG and the Valkey operator. The chart does not install them. Several replicas need both, so that keys, rate limits and budgets are shared, and the chart refuses to render a config with `store.driver: sqlite` or `limiter.driver: memory` when it would run more than one replica.
2. **A secret** with the values your config refers to as `${VAR}`:

   ```sh
   kubectl create secret generic genaiopengateway \
     --from-literal=GATEWAY_ADMIN_TOKEN="$(openssl rand -hex 32)" \
     --from-literal=DATABASE_URL='postgres://gateway:...@db.example.internal:5432/gateway?sslmode=verify-full' \
     --from-literal=VALKEY_ADDRESS=valkey.cache.svc:6379 \
     --from-literal=VALKEY_PASSWORD=... \
     --from-literal=OPENAI_API_KEY=sk-...
   ```

   In production, External Secrets or your cloud's secret store usually creates this secret. Point `envFrom` at whatever name it uses.

## Install

Write a `values.yaml` with at least your `config`, the gateway's normal [config.yaml](config.md):

```yaml
config: |
  listen: ":8080"
  metrics:
    listen: ":9090"
  admin:
    token: ${GATEWAY_ADMIN_TOKEN}
  store:
    driver: postgres
    dsn: ${DATABASE_URL}
  limiter:
    driver: valkey
    address: ${VALKEY_ADDRESS}
    password: ${VALKEY_PASSWORD}
  providers:
    openai: {type: openai, base_url: https://api.openai.com/v1, api_key: ${OPENAI_API_KEY}}
  models:
    chat-default:
      targets: [{provider: openai, model: gpt-4.1-mini}]
```

Then install the signed chart:

```sh
helm install gateway oci://ghcr.io/rnikadi/charts/genaiopengateway --version 0.5.0 -f values.yaml
kubectl port-forward svc/gateway-genaiopengateway 8080:8080
curl localhost:8080/readyz
```

The first replica to start runs the database migrations while the others wait for it, so a fresh install needs no separate migration step. A config change rolls the pods.

To check the chart and image signatures first:

```sh
cosign verify ghcr.io/rnikadi/charts/genaiopengateway:0.5.0 \
  --certificate-identity-regexp '^https://github.com/rnikadi/genaiopengateway/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Common settings

| Need | Values |
| --- | --- |
| Expose it | `ingress.enabled: true` with `className`, `hosts` and `tls`, or `service.type: LoadBalancer`. Long streams need the ingress's read timeout raised, such as `nginx.ingress.kubernetes.io/proxy-read-timeout: "600"`. |
| Scale on load | `autoscaling.enabled: true`; `minReplicas` and `maxReplicas`. |
| Prometheus operator | `serviceMonitor.enabled: true`. Metrics are on `metricsPort` (9090), which must match `metrics.listen` in config; set `metricsPort: null` when config has no `metrics.listen`. |
| Restrict who can call it | `networkPolicy.enabled: true` and `ingressNamespaceSelectors` for your ingress controller's namespace. |
| Cloud credentials without keys | `serviceAccount.annotations`, such as `iam.gke.io/gcp-service-account` or `eks.amazonaws.com/role-arn`, for the Vertex AI and Bedrock adapters ([cloud-providers.md](cloud-providers.md)). |
| Pin the exact image | `image.digest: sha256:...` |
| Extra variables | `env` and `envFrom`, as in a pod spec. |

Every value is listed with a comment in the chart's [values.yaml](../deploy/helm/genaiopengateway/values.yaml).

## What the pod does on shutdown

When a pod is told to stop, it waits 5 seconds so the Service and load balancers stop sending it new requests, fails `/readyz`, stops accepting connections, and lets running requests finish for up to 50 seconds. `terminationGracePeriodSeconds` is 60; raise both together if your streams run longer.

## Upgrades

Upgrade the chart and image together with `helm upgrade --version`. Migrations run on start, under the same lock. During the rollout, old and new pods serve side by side for a short time, so read the [changelog](../CHANGELOG.md) before upgrading across a minor version.
