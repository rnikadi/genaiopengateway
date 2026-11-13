#!/usr/bin/env bash
# Starts Keycloak with the test realm for test/idp/run.sh, and writes the
# variables run.sh needs to $WORK/keycloak.env. With KEYCLOAK_HOME it runs
# that distribution; otherwise the container image.
set -euo pipefail
root=$(cd "$(dirname "$0")/../../.." && pwd)
work=${WORK:?set WORK to a directory}
mkdir -p "$work/import"
port=${KEYCLOAK_PORT:-8180}

if [ -z "${GATEWAY_BIN:-}" ]; then
  GATEWAY_BIN=$work/gateway
  (cd "$root" && go build -o "$GATEWAY_BIN" ./cmd/gateway)
fi
# The gateway's SAML key pair; Keycloak checks its signatures and
# encrypts assertions to it.
if [ ! -f "$work/sp.crt" ]; then
  "$GATEWAY_BIN" saml-keygen -cert "$work/sp.crt" -key "$work/sp.key" -years 1 >/dev/null
fi
export SP_CERT_B64=$(grep -v -- ----- "$work/sp.crt" | tr -d '\n')
export IDP_PASSWORD=${IDP_PASSWORD:-$(head -c 18 /dev/urandom | base64 | tr -d '/+=')}
export OIDC_CLIENT_SECRET=${OIDC_CLIENT_SECRET:-$(head -c 24 /dev/urandom | base64 | tr -d '/+=')}
export OIDC_GATEWAY=http://localhost:18090 SAML_GATEWAY=http://localhost:18091
python3 - "$root/test/idp/keycloak/realm.json" "$work/import/gateway-realm.json" <<'PY'
import os, re, sys
names = ["IDP_PASSWORD", "OIDC_CLIENT_SECRET", "OIDC_GATEWAY", "SAML_GATEWAY", "SP_CERT_B64"]
text = open(sys.argv[1]).read()
text = re.sub(r"\$\{(" + "|".join(names) + r")\}", lambda m: os.environ[m.group(1)], text)
open(sys.argv[2], "w").write(text)
PY

if [ -n "${KEYCLOAK_HOME:-}" ]; then
  cp "$work/import/gateway-realm.json" "$KEYCLOAK_HOME/data/import/" 2>/dev/null || {
    mkdir -p "$KEYCLOAK_HOME/data/import" && cp "$work/import/gateway-realm.json" "$KEYCLOAK_HOME/data/import/"; }
  KC_BOOTSTRAP_ADMIN_USERNAME=admin KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
    nohup "$KEYCLOAK_HOME/bin/kc.sh" start-dev --http-port "$port" --import-realm >"$work/keycloak.log" 2>&1 &
else
  docker run -d --name gog-keycloak -p "$port:8080" -v "$work/import:/opt/keycloak/data/import:ro" \
    -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
    "${KEYCLOAK_IMAGE:-quay.io/keycloak/keycloak:26.4}" start-dev --import-realm >/dev/null
fi
timeout 180 sh -c "until curl -sf http://localhost:$port/realms/gateway/.well-known/openid-configuration >/dev/null; do sleep 2; done"

cat >"$work/keycloak.env" <<ENV
IDP_USER=ada
IDP_EMAIL=ada@example.com
IDP_PASSWORD=$IDP_PASSWORD
IDP_GROUP=gateway-owners
IDP_ISSUER=http://localhost:$port/realms/gateway
IDP_CLIENT_ID=gateway
IDP_CLIENT_SECRET=$OIDC_CLIENT_SECRET
IDP_METADATA_URL=http://localhost:$port/realms/gateway/protocol/saml/descriptor
IDP_SINGLE_LOGOUT=1
ENV
printf 'SAML_SP_CERT=%q\nSAML_SP_KEY=%q\n' "$(cat "$work/sp.crt")" "$(cat "$work/sp.key")" >>"$work/keycloak.env"
echo "Keycloak is up on http://localhost:$port; variables in $work/keycloak.env"
