#!/usr/bin/env bash
# Description: TUI 'b' starts the in-instance browser proxy and opens the browser on the Fleet Launch landing page; through that proxy curl gets plain HTTP, three Set-Cookie headers intact, a CONNECT tunnel and a WebSocket echo, and the landing page's health checks and app tab work.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../net/net.sh"
workdir=$(mktemp -d)
itest_cleanup() {
  tui_kill
  rm -rf "${workdir}"
}
itest_begin

setup_net_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

info "start the in-instance web server (the fixture's 'Itest Web' site)"
itest_net_serve "${inst}" http 17380

# A stand-in for Chrome on the TUI machine (CI runners ship a real one, which
# the stub must shadow): it records each launch's arguments, one line each.
# The TUI must see it: export covers a fresh tmux server, set-environment -g
# one that already runs.
launches="${workdir}/chrome-launches.log"
mkdir -p "${workdir}/bin"
cat > "${workdir}/bin/google-chrome" <<STUB
#!/bin/sh
echo "\$*" >> "${launches}"
STUB
chmod +x "${workdir}/bin/google-chrome"
export PATH="${workdir}/bin:${PATH}"
tmux set-environment -g PATH "${PATH}" >/dev/null 2>&1 || true

tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

info "select alpha and press 'b'"
tui_send j
sleep 0.3
tui_send b
# The first open installs the proxy inside the instance (a package install).
# The stand-in's record is the evidence; the TUI's status line is transient.
deadline=$(( $(date +%s) + $(_scale_timeout 300) ))
until [ -s "${launches}" ]; do
  if tui_capture | grep -q "Browser proxy error"; then fail "'b' failed: $(tui_capture)"; fi
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the browser was never launched: $(tui_capture)"
  sleep 1
done
launch=$(head -n 1 "${launches}")
proxy_port=$(printf '%s\n' "${launch}" | grep -oE -- '--proxy-server=http://localhost:[0-9]+' | grep -oE '[0-9]+$' || true)
[ -n "${proxy_port}" ] || fail "the browser was launched without a proxy: ${launch}"
proxy="http://127.0.0.1:${proxy_port}"
info "proxy forward on localhost:${proxy_port}"

info "the browser was launched through the proxy, opening the landing page"
assert_contains "${launch}" "--proxy-bypass-list=<-loopback>" "the browser must not bypass the proxy for localhost"
# The fixture configures a Fleet Launch block and no browser.initialUrl.
assert_equals "http://localhost:16767" "${launch##* }" "the browser should open on the landing page"

# curl_proxy <args...> — curl through the browser proxy, as the browser would.
curl_proxy() { curl -sS --max-time 30 -x "${proxy}" "$@"; }

info "plain HTTP to the instance's localhost"
assert_equals "fleet-itest-web 17380" "$(curl_proxy -f http://localhost:17380/)" "GET through the proxy"

info "a response with three Set-Cookie headers keeps all three"
headers=$(curl_proxy -f -D - -o /dev/null http://localhost:17380/cookies | tr -d '\r')
assert_equals "3" "$(printf '%s\n' "${headers}" | grep -ci '^set-cookie:')" "Set-Cookie headers through the proxy: ${headers}"
assert_contains "${headers}" "fleet_a=1; Path=/" "first cookie"
assert_contains "${headers}" "fleet_b=2; Path=/; HttpOnly" "second cookie"
assert_contains "${headers}" "fleet_c=3; Path=/; Max-Age=600" "third cookie"

info "a CONNECT tunnel (how a browser reaches HTTPS and WebSockets through a proxy)"
assert_equals "fleet-itest-web 17380" "$(curl_proxy -f -p http://localhost:17380/)" "GET through a CONNECT tunnel"

info "a WebSocket through a CONNECT tunnel"
out=$(itest_net_client ws "${proxy_port}" localhost:17380 fleet-itest-ping 2>&1) || fail "${out}"
info "${out}"

info "the landing page lists the fixture's sites and app"
page=$(curl_proxy -f http://localhost:16767/)
assert_contains "${page}" "Itest Web" "landing page site"
assert_contains "${page}" "Itest Down" "landing page site"
assert_contains "${page}" "Itest App" "landing page app"

info "the landing page's health checks reach the instance's services"
health=$(curl_proxy -f http://localhost:16767/health/0)
assert_contains "${health}" 'aria-label="healthy"' "a listening site should be healthy"
assert_contains "${health}" "200 OK" "a listening site's health label"
health=$(curl_proxy -f http://localhost:16767/health/1)
assert_contains "${health}" 'aria-label="unhealthy"' "a site with nothing listening should be unhealthy"
assert_contains "${health}" "unreachable" "a site with nothing listening"

info "opening the app tab starts the app and frames it"
app=$(curl_proxy -f --max-time 90 http://localhost:16767/app/0)
assert_contains "${app}" '<iframe src="http://localhost:17381"' "the app tab should frame the started app"
assert_equals "fleet-itest-web 17381" "$(curl_proxy -f http://localhost:17381/)" "the app the landing page started"

info "a second 'b' reuses the running proxy and its forward"
tui_send b
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
until [ "$(wc -l < "${launches}" | tr -d '[:space:]')" -ge 2 ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the second 'b' never launched the browser: $(tui_capture)"
  sleep 0.5
done
assert_contains "$(sed -n 2p "${launches}")" "--proxy-server=http://localhost:${proxy_port}" "the second launch should reuse the proxy forward"
assert_equals "fleet-itest-web 17380" "$(curl_proxy -f http://localhost:17380/)" "GET through the proxy after the second open"

pass "browser proxy and landing page"
