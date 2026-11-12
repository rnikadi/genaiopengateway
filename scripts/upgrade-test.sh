#!/usr/bin/env bash
# Upgrade and restore drill (docs/backup.md):
#
#   1. the previous release starts with zero config and gets an
#      organization, a team, a key and a stored provider and model;
#   2. this checkout starts on the same data directory, migrates it, and
#      must still serve them, with the old key working;
#   3. this checkout backs the data up, restores it into an empty data
#      directory, starts there, and must serve the same.
#
# FROM picks the release to upgrade from (default: the newest v* tag that
# is not on HEAD).
set -euo pipefail
cd "$(dirname "$0")/.."

FROM=${FROM:-$(git tag --list 'v*' --sort=-v:refname | grep -vxF -e "$(git tag --points-at HEAD)" -e '' | head -1)}
PORT=${PORT:-18080}
work=$(mktemp -d)
pid=
cleanup() {
  [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  git worktree remove --force "$work/old-src" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

echo "Upgrade from $FROM to $(git describe --tags --always --dirty)"
git worktree add --detach "$work/old-src" "$FROM" >/dev/null
(cd "$work/old-src" && go build -o "$work/old" ./cmd/gateway)
go build -o "$work/new" ./cmd/gateway

base=http://localhost:$PORT
TOKEN=$(openssl rand -hex 24)
ENCKEY=$(openssl rand -base64 32)
config() { # data-dir → config file
  mkdir -p -m 700 "$1"
  cat >"$1.yaml" <<YAML
listen: ":$PORT"
admin: {token: "$TOKEN"}
store: {driver: sqlite, dsn: "file:$1/gateway.db", refresh_interval: 1s}
secrets: {encryption_key: "$ENCKEY"}
YAML
  echo "$1.yaml"
}
start() { # binary config
  "$1" -config "$2" >"$work/log" 2>&1 &
  pid=$!
  for _ in $(seq 100); do
    curl -fsS "$base/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$pid" 2>/dev/null || { cat "$work/log"; return 1; }
    sleep 0.1
  done
  cat "$work/log"; return 1
}
stop() { kill "$pid"; wait "$pid" 2>/dev/null || true; pid=; }
admin() { # method path [body]
  curl -fsS -X "$1" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$base$2" ${3:+-d "$3"}
}
check() { # what
  admin GET /admin/v1/orgs | jq -e '.data | map(.name) | index("upgrade-org")' >/dev/null || { echo "FAIL ($1): organization missing"; exit 1; }
  admin GET "/admin/v1/orgs/$ORG/teams" | jq -e '.data | map(.name) | index("upgrade-team")' >/dev/null || { echo "FAIL ($1): team missing"; exit 1; }
  admin GET /admin/v1/config | jq -e '.data | map(select(.kind == "models" and .name == "upgrade-model")) | length == 1' >/dev/null || { echo "FAIL ($1): stored model missing"; exit 1; }
  # The key minted by the old release still authenticates and sees the model.
  curl -fsS -H "Authorization: Bearer $KEY" "$base/v1/models" | jq -e '.data | map(.id) | index("upgrade-model")' >/dev/null || { echo "FAIL ($1): key or model"; exit 1; }
  # The provider's stored secret can still be decrypted: it reads as set.
  admin GET /admin/v1/config | jq -e '.data[] | select(.kind == "providers" and .name == "upgrade-provider") | .spec.api_key == "set"' >/dev/null || { echo "FAIL ($1): provider secret"; exit 1; }
  echo "ok: $1"
}

# 1. The old release.
data=$(config "$work/data")
start "$work/old" "$data"
ORG=$(admin POST /admin/v1/orgs '{"name":"upgrade-org"}' | jq -r .id)
TEAM=$(admin POST "/admin/v1/orgs/$ORG/teams" '{"name":"upgrade-team"}' | jq -r .id)
KEY=$(admin POST "/admin/v1/teams/$TEAM/keys" '{"name":"upgrade-key"}' | jq -r .key)
admin PUT /admin/v1/config/providers/upgrade-provider '{"type":"openai","base_url":"http://127.0.0.1:9/v1","api_key":"sk-upgrade-secret"}' >/dev/null
admin PUT /admin/v1/config/models/upgrade-model '{"targets":[{"provider":"upgrade-provider","model":"m"}]}' >/dev/null
sleep 2 # one store.refresh_interval, so the old release serves the model
check "$FROM"
stop

# 2. This checkout on the same data.
start "$work/new" "$data"
check "upgraded"
stop

# 3. Back up, restore into an empty directory, serve from it.
"$work/new" backup -config "$data" -out "$work/backup.gogb"
restored=$(config "$work/restored") # the same encryption key
"$work/new" restore -config "$restored" "$work/backup.gogb"
start "$work/new" "$restored"
check "restored"
stop
echo "Upgrade and restore drill passed."
