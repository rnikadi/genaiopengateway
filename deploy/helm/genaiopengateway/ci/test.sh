#!/usr/bin/env bash
# Lints the chart, checks the rendered objects against the Kubernetes
# schemas, and checks the guards that refuse configs that cannot work.
set -euo pipefail
cd "$(dirname "$0")/.."
kube=1.30.0
render() { helm template t . --kube-version "$kube" "$@"; }
schemas() { kubeconform -strict -summary -kubernetes-version "$kube" "$@"; }
fails() {
  local want=$1; shift
  if out=$(render "$@" 2>&1); then echo "rendered, want failure: $want"; exit 1; fi
  grep -q "$want" <<<"$out" || { echo "want $want, got: $out"; exit 1; }
}

helm lint --strict . --kube-version "$kube"
render | schemas
# The ServiceMonitor CRD has no schema in the default catalog.
render -f ci/all-values.yaml | schemas -ignore-missing-schemas

fails "config is empty" --set config=
fails "config is not valid YAML" --set-string 'config=a: ['
fails "store.driver: postgres" -f ci/sqlite-values.yaml
fails "limiter.driver: valkey" -f ci/memory-values.yaml
fails "limiter.driver: valkey" -f ci/memory-values.yaml --set replicaCount=1 --set autoscaling.enabled=true

single=$(render -f ci/single-values.yaml)
grep -q 'kind: PodDisruptionBudget' <<<"$single" && { echo "PDB with one replica"; exit 1; }
grep -q 'image: "\?ghcr.io/rnikadi/genaiopengateway@sha256:0' <(render -f ci/all-values.yaml) || { echo "digest not used"; exit 1; }
all=$(render -f ci/all-values.yaml)
grep -q 'alert: GatewayHighErrorRate' <<<"$all" || { echo "PrometheusRule has no alerts"; exit 1; }
grep -q '"uid": "genaiopengateway"' <<<"$all" || { echo "dashboard ConfigMap is empty"; exit 1; }
# The operator: cluster-wide by default, one namespace when asked.
grep -q 'kind: ClusterRole$' <<<"$all" || { echo "operator ClusterRole missing"; exit 1; }
grep -q 'command: \[/gogctl, operator' <<<"$all" || { echo "operator Deployment missing"; exit 1; }
scoped=$(render -f ci/operator-values.yaml)
grep -q 'kind: Role$' <<<"$scoped" && grep -q 'GOG_WATCH_NAMESPACE' <<<"$scoped" || { echo "namespaced operator not rendered"; exit 1; }
render -f ci/operator-values.yaml | schemas
# The CRDs parse; kind.sh installs them into a real API server.
for f in crds/*.yaml; do python3 -c 'import sys, yaml; yaml.safe_load(open(sys.argv[1]))' "$f"; done
echo "chart checks passed"
