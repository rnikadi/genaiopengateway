#!/bin/sh
# Starts the gateway with no config file next to a fake upstream and runs
# the setup wizard in a browser (quickstart.mjs).
#
#   cd test/e2e && npm ci && npx playwright install chromium && ./quickstart.sh
set -eu
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
pids=""
trap 'kill $pids 2>/dev/null; rm -rf "$work"' EXIT
(cd "$here/../.." && go build -o "$work/gateway" ./cmd/gateway && go build -o "$work/bench" ./test/bench)
"$work/bench" -upstream 127.0.0.1:18291 > "$work/upstream.log" 2>&1 &
pids="$pids $!"
(cd "$work" && exec ./gateway -config missing.yaml > gateway.log 2> banner.log) &
pids="$pids $!"
for _ in $(seq 1 100); do curl -sf http://127.0.0.1:8080/readyz >/dev/null && break; sleep 0.2; done
cat "$work/banner.log"
BOOTSTRAP_FILE="$work/data/bootstrap.json" node "$here/quickstart.mjs" || { tail -40 "$work/gateway.log"; exit 1; }
BOOTSTRAP_FILE="$work/data/bootstrap.json" node "$here/a11y.mjs"
