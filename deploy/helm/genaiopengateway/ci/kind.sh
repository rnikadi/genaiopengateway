#!/usr/bin/env bash
# Installs the chart with 2 replicas into the current cluster (kind in
# CI), with Postgres, Valkey and a fake provider, and sends traffic
# through it. Both pods migrate the same empty database at once, which
# exercises the migration lock. Expects the image genaiopengateway:ci to
# be loaded into the cluster.
set -euo pipefail
cd "$(dirname "$0")"
kubectl apply -f kind-deps.yaml
kubectl rollout status deploy/postgres deploy/valkey deploy/fake-provider --timeout=180s
helm install gog .. -f kind-values.yaml --wait --timeout 180s
test "$(kubectl get deploy gog-genaiopengateway -o jsonpath='{.status.readyReplicas}')" = 2

kubectl port-forward svc/gog-genaiopengateway 18080:8080 >/dev/null 2>&1 &
trap 'kill $!' EXIT
timeout 30 sh -c 'until curl -sf localhost:18080/readyz >/dev/null; do sleep 1; done'

# Several calls, so the Service spreads them over both pods.
for i in 1 2 3 4 5 6; do
  curl -sf localhost:18080/v1/chat/completions -H 'Authorization: Bearer ci-dev-key' \
    -H 'Content-Type: application/json' -d '{"model":"chat","messages":[{"role":"user","content":"hi"}]}' |
    grep -q 'hello from kind'
done
curl -sf localhost:18080/metrics >/dev/null && { echo "metrics should not be on the main port"; exit 1; } || true

# Usage is written to the shared store by whichever pod served the call.
usage() { curl -sf -H 'Authorization: Bearer ci-admin-token-0000000000000000000000' 'localhost:18080/admin/v1/usage?group_by=model'; }
for _ in $(seq 30); do
  usage | grep -q '"requests":6' && { echo "kind smoke test passed"; exit 0; }
  sleep 1
done
usage; exit 1
