#!/usr/bin/env bash
#
# Boot the image and drive it the way the router operator does: bring up the
# HTTPS API with a pinned certificate, apply configuration with commit-confirm,
# confirm it, read it back.
#
#   test/smoke.sh <image>
#
# A rootless engine cannot write host sysctls or load kernel modules, so the
# firewall and WireGuard checks only run with a rootful one (as in CI).
set -euo pipefail

IMAGE="${1:?usage: smoke.sh <image>}"
ENGINE="${CONTAINER_ENGINE:-$(command -v docker 2>/dev/null || command -v podman)}"
NAME="vyos-smoke-$$"
API_KEY="smoke-key"
WORK="$(mktemp -d)"

cleanup() {
  status=$?
  if [[ "${status}" -ne 0 ]]; then
    echo "--- journal of ${NAME} ---"
    "${ENGINE}" exec "${NAME}" journalctl -b --no-pager -n 60 2>/dev/null || true
  fi
  "${ENGINE}" rm -f "${NAME}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
  exit "${status}"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

rootless=false
if "${ENGINE}" info --format '{{.Host.Security.Rootless}}' 2>/dev/null | grep -q true; then
  rootless=true
fi

step "starting ${IMAGE} with ${ENGINE} (rootless=${rootless})"
"${ENGINE}" run -d --name "${NAME}" --hostname vyos --privileged \
  -v /lib/modules:/lib/modules:ro -p 127.0.0.1::443 "${IMAGE}" >/dev/null
PORT="$("${ENGINE}" port "${NAME}" 443/tcp | head -1 | sed 's/.*://')"
URL="https://127.0.0.1:${PORT}"

step "waiting for the boot configuration to load"
for _ in $(seq 1 60); do
  if "${ENGINE}" exec "${NAME}" test -e /tmp/vyos-config-status 2>/dev/null; then
    break
  fi
  sleep 2
done
"${ENGINE}" exec "${NAME}" test -e /tmp/vyos-config-status || fail "VyOS did not finish booting"
if [[ "${rootless}" == false ]]; then
  [[ "$("${ENGINE}" exec "${NAME}" cat /tmp/vyos-config-status)" == 0 ]] || fail "boot configuration failed to load"
fi

step "failover helper is installed"
"${ENGINE}" exec "${NAME}" /usr/local/bin/hcloud-vrrp-failover -version

step "enabling the REST API with a pinned certificate"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "${WORK}/api.key" -out "${WORK}/api.crt" -days 1 -subj "/CN=vyos-smoke" \
  -addext "subjectAltName=IP:127.0.0.1" 2>/dev/null
body() { grep -v -- '-----' "$1" | tr -d '\n'; }
cat > "${WORK}/seed.sh" <<SEED
#!/bin/vbash
source /opt/vyatta/etc/functions/script-template
configure
set pki certificate api certificate '$(body "${WORK}/api.crt")'
set pki certificate api private key '$(body "${WORK}/api.key")'
set service https certificates certificate api
set service https api keys id smoke key '${API_KEY}'
set service https api rest
set system config-management commit-confirm action reload
commit
save
exit
SEED
"${ENGINE}" cp "${WORK}/seed.sh" "${NAME}:/tmp/seed.sh"
"${ENGINE}" exec "${NAME}" chmod +x /tmp/seed.sh
"${ENGINE}" exec "${NAME}" sg vyattacfg -c /tmp/seed.sh

# call <endpoint> <json body>: prints the response body, fails on a non-200.
call() {
  local out code
  out="$(curl --silent --cacert "${WORK}/api.crt" --max-time 120 --write-out '\n%{http_code}' \
    --request POST "${URL}/$1" --header 'Content-Type: application/json' --data "$2")"
  code="${out##*$'\n'}"
  out="${out%$'\n'*}"
  if [[ "${code}" != 200 ]]; then
    echo "${out}" >&2
    fail "POST /$1 returned HTTP ${code}"
  fi
  echo "${out}"
}
keyed() { printf '{"key":"%s",%s}' "${API_KEY}" "$1"; }
exists() { call retrieve "$(keyed "\"op\":\"exists\",\"path\":$1")" | jq -e '.data == true' >/dev/null; }

step "GET /info answers without a key"
for _ in $(seq 1 30); do
  if curl --silent --fail --cacert "${WORK}/api.crt" --max-time 5 "${URL}/info" >"${WORK}/info"; then
    break
  fi
  sleep 1
done
jq -e '.success == true and (.data.version | length > 0)' "${WORK}/info" >/dev/null || fail "/info did not answer"
jq -r '.data.version' "${WORK}/info"

step "a wrong key is rejected"
code="$(curl --silent --cacert "${WORK}/api.crt" --output /dev/null --write-out '%{http_code}' \
  --request POST "${URL}/retrieve" --header 'Content-Type: application/json' \
  --data '{"key":"wrong","op":"exists","path":["interfaces"]}')"
[[ "${code}" == 401 ]] || fail "wrong key got HTTP ${code}, want 401"

step "commit-confirm, confirm, read back, save"
call configure "$(keyed '"confirm_time":1,"commands":[{"op":"set","path":["interfaces","dummy","dum0","address","192.0.2.1/32"]}]')" >/dev/null
call config-file "$(keyed '"op":"confirm"')" | jq -r '.data'
exists '["interfaces","dummy","dum0"]' || fail "dum0 missing after confirm"
call show "$(keyed '"op":"show","path":["configuration","commands"]')" \
  | jq -r '.data' | grep -q "^set interfaces dummy dum0 address '192.0.2.1/32'$" \
  || fail "dum0 not in 'show configuration commands'"
call config-file "$(keyed '"op":"save"')" >/dev/null

step "routing and metrics features commit"
call configure "$(keyed '"commands":[
  {"op":"set","path":["protocols","bgp","system-as","65001"]},
  {"op":"set","path":["protocols","bgp","neighbor","192.0.2.2","remote-as","65001"]},
  {"op":"set","path":["protocols","bgp","neighbor","192.0.2.2","address-family","ipv4-unicast"]},
  {"op":"set","path":["service","monitoring","prometheus","node-exporter","listen-address","127.0.0.1"]}]')" >/dev/null

if [[ "${rootless}" == true ]]; then
  step "SKIPPED: firewall, NAT, WireGuard and VRRP need a rootful engine"
else
  step "firewall, NAT, WireGuard and VRRP commit"
  call configure "$(keyed '"commands":[
    {"op":"set","path":["firewall","ipv4","input","filter","default-action","accept"]},
    {"op":"set","path":["nat","source","rule","100","outbound-interface","name","dum0"]},
    {"op":"set","path":["nat","source","rule","100","translation","address","masquerade"]},
    {"op":"set","path":["interfaces","wireguard","wg0","address","10.99.0.1/24"]},
    {"op":"set","path":["interfaces","wireguard","wg0","port","51820"]},
    {"op":"set","path":["interfaces","wireguard","wg0","private-key","kKMnp6QBm1Wj0zxnc1uBbSs3hhdpVIFeQy9hxRTBxWQ="]},
    {"op":"set","path":["interfaces","wireguard","wg0","peer","lab","public-key","xo2pQF7WbTFQkAOgNi6s9RNdu1uEhn2wGYOvUkb7HBg="]},
    {"op":"set","path":["interfaces","wireguard","wg0","peer","lab","allowed-ips","10.99.0.2/32"]},
    {"op":"set","path":["high-availability","vrrp","group","wan","vrid","10"]},
    {"op":"set","path":["high-availability","vrrp","group","wan","interface","dum0"]},
    {"op":"set","path":["high-availability","vrrp","group","wan","address","192.0.2.100/32"]},
    {"op":"set","path":["high-availability","vrrp","group","wan","hello-source-address","192.0.2.1"]},
    {"op":"set","path":["high-availability","vrrp","group","wan","peer-address","192.0.2.9"]},
    {"op":"set","path":["high-availability","vrrp","group","wan","transition-script","master","/usr/local/bin/hcloud-vrrp-failover wan"]}]')" >/dev/null
  "${ENGINE}" exec "${NAME}" ip link show wg0 >/dev/null || fail "wg0 was not created"
fi

step "smoke test passed"
