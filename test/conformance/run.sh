#!/bin/sh
# Runs the OpenAI SDK conformance check: a fake upstream, the gateway in
# front of it, and the official SDK calling every endpoint.
#
#   pip install openai==2.8.1 && test/conformance/run.sh
set -eu
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
pids=""
trap 'kill $pids 2>/dev/null; rm -rf "$work"' EXIT
(cd "$here/../.." && go build -o "$work/gateway" ./cmd/gateway)
python3 "$here/fake_upstream.py" 18191 > "$work/upstream.log" 2>&1 &
pids="$pids $!"
CONFORMANCE_DB="$work/gateway.db" "$work/gateway" -config "$here/config.yaml" > "$work/gateway.log" 2>&1 &
pids="$pids $!"
for _ in $(seq 1 50); do curl -sf http://127.0.0.1:18190/readyz >/dev/null && break; sleep 0.2; done
python3 "$here/sdk_test.py" || { cat "$work/gateway.log"; exit 1; }
