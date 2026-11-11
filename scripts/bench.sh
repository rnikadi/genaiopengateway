#!/bin/sh
# Benchmarks the latency GenAIOpenGateway adds and the throughput it
# sustains, against a fake upstream that answers at once. See
# docs/benchmarks.md for the method and published results.
#
#   scripts/bench.sh                       # the gateway against the upstream alone
#   PEERS="litellm" scripts/bench.sh       # also LiteLLM proxy (needs `litellm` on PATH)
#   N=50000 C=64 STREAM=1 FORMAT=markdown scripts/bench.sh
#   GATE=10ms scripts/bench.sh             # fail if the gateway adds more at p50 (CI)
#
# Every process runs on this machine, so pin CPUs or use a quiet host for
# numbers worth publishing.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
pids=""
trap 'kill $pids 2>/dev/null; rm -rf "$work"' EXIT
N=${N:-20000} C=${C:-32} STREAM=${STREAM:-0} FORMAT=${FORMAT:-text} PEERS=${PEERS:-} GATE=${GATE:-0}

cd "$root"
go build -o "$work/gateway" ./cmd/gateway
go build -o "$work/bench" ./test/bench

wait_for() { for _ in $(seq 1 150); do curl -sf "$1" >/dev/null 2>&1 && return 0; sleep 0.2; done; echo "timed out waiting for $1" >&2; exit 1; }

"$work/bench" -upstream 127.0.0.1:18291 > "$work/upstream.log" 2>&1 &
pids="$pids $!"
wait_for http://127.0.0.1:18291/v1/models
BENCH_DB="$work/gateway.db" "$work/gateway" -config test/bench/gateway.yaml > "$work/gateway.log" 2>&1 &
pids="$pids $!"
wait_for http://127.0.0.1:18290/readyz

set -- -target upstream=http://127.0.0.1:18291/v1 \
  -target genaiopengateway=http://127.0.0.1:18290/v1,key=bench-key-00000000000000000000

for peer in $PEERS; do
  case "$peer" in
  litellm)
    cat > "$work/litellm.yaml" <<YAML
model_list:
  - model_name: bench
    litellm_params: {model: openai/bench-upstream, api_base: "http://127.0.0.1:18291/v1", api_key: upstream-key}
general_settings: {master_key: sk-bench-litellm}
litellm_settings: {telemetry: false}
YAML
    litellm --config "$work/litellm.yaml" --host 127.0.0.1 --port 18292 --num_workers "${LITELLM_WORKERS:-1}" > "$work/litellm.log" 2>&1 &
    pids="$pids $!"
    wait_for http://127.0.0.1:18292/health/liveliness
    set -- "$@" -target litellm=http://127.0.0.1:18292/v1,key=sk-bench-litellm
    ;;
  *) echo "unknown peer $peer (supported: litellm)" >&2; exit 2 ;;
  esac
done

stream=""
[ "$STREAM" = 1 ] && stream=-stream
"$work/bench" -n "$N" -c "$C" -format "$FORMAT" -max-added-p50 "$GATE" $stream "$@" || { tail -50 "$work/gateway.log"; exit 1; }
