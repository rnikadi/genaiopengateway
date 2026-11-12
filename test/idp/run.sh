#!/usr/bin/env bash
# Signs in to a gateway through a real identity provider in a browser
# (docs/identity-providers.md). Usage: test/idp/run.sh <idp> <oidc|saml>
#
# Every identity provider takes the same variables:
#   IDP_USER, IDP_PASSWORD   a test user without MFA, in IDP_GROUP
#   IDP_EMAIL                the user's email, when IDP_USER is not it
#   IDP_GROUP                the group mapped to the owner role, or
#   IDP_ROLE_BY_EMAIL=1      map the user's email instead (Google OIDC)
#   oidc: IDP_ISSUER, IDP_CLIENT_ID, IDP_CLIENT_SECRET
#   saml: IDP_METADATA_URL, SAML_SP_CERT, SAML_SP_KEY (the pair registered
#         with the identity provider), IDP_SINGLE_LOGOUT=1 to check it
# The gateway listens on http://localhost:18090 (oidc) or :18091 (saml),
# which is what the identity provider's app must name.
set -euo pipefail
idp=${1:?identity provider}
protocol=${2:?oidc or saml}
root=$(cd "$(dirname "$0")/../.." && pwd)
work=${WORK:-$(mktemp -d)}
mkdir -p "$work"

if [ -z "${GATEWAY_BIN:-}" ]; then
  GATEWAY_BIN=$work/gateway
  (cd "$root" && go build -o "$GATEWAY_BIN" ./cmd/gateway)
fi

case $protocol in
  oidc)
    port=18090
    sso=$(cat <<YAML
  oidc:
    issuer: "${IDP_ISSUER:?}"
    client_id: "${IDP_CLIENT_ID:?}"
    client_secret: "\${IDP_CLIENT_SECRET}"
    redirect_url: http://localhost:$port/ui/auth/callback
YAML
)
    ;;
  saml)
    port=18091
    : "${IDP_METADATA_URL:?}" "${SAML_SP_CERT:?}" "${SAML_SP_KEY:?}"
    sso=$(cat <<YAML
  saml:
    idp_metadata_url: "$IDP_METADATA_URL"
    acs_url: http://localhost:$port/ui/auth/saml/acs
    sp_certificate: "\${SAML_SP_CERT}"
    sp_private_key: "\${SAML_SP_KEY}"
    groups_attribute: "${IDP_GROUPS_ATTRIBUTE:-groups}"
YAML
)
    ;;
  *) echo "protocol must be oidc or saml" >&2; exit 2 ;;
esac

if [ "${IDP_ROLE_BY_EMAIL:-0}" = 1 ]; then
  roles="{email: \"${IDP_EMAIL:-$IDP_USER}\", role: owner}"
else
  roles="{group: \"${IDP_GROUP:-gateway-owners}\", role: owner}"
fi
cat > "$work/$idp-$protocol.yaml" <<YAML
listen: 127.0.0.1:$port
store: {driver: sqlite, dsn: "file:$work/$idp-$protocol.db"}
sso:
$sso
  roles: [$roles]
YAML

"$GATEWAY_BIN" -config "$work/$idp-$protocol.yaml" >"$work/$idp-$protocol.log" 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null || true' EXIT
timeout 30 sh -c "until curl -sf http://127.0.0.1:$port/readyz >/dev/null; do sleep 0.5; done" || {
  cat "$work/$idp-$protocol.log"; exit 1; }

cd "$root/test/e2e"
status=0
GATEWAY_URL=http://localhost:$port IDP=$idp PROTOCOL=$protocol \
  EXPECT_SINGLE_LOGOUT=$([ "$protocol" = saml ] && [ "${IDP_SINGLE_LOGOUT:-0}" = 1 ] && echo 1 || echo 0) \
  node idp-signin.mjs || status=$?
if [ $status -ne 0 ]; then
  echo "--- gateway log"; cat "$work/$idp-$protocol.log"
fi
exit $status
