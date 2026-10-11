#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, in a namespace that enforces the restricted pod security standard (no root), an image with no package manager gets the browser proxy: TUI 'b' starts fleet's own proxy, which serves the instance's localhost (plain HTTP and CONNECT) on the pod's loopback only.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
NS=""
workdir=$(mktemp -d)
itest_cleanup() {
  tui_kill
  if [ -n "${NS}" ]; then
    _be_kubectl delete namespace "${NS}" --wait=false >/dev/null 2>&1 || true
  fi
  rm -rf "${workdir}"
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the browser proxy on a restricted namespace"

tmux_var="FLEET_POD_TMUX_$(_be_arch | tr '[:lower:]' '[:upper:]')"
[ -f "${!tmux_var:-}" ] || fail "${tmux_var} names no tmux bundle (make pod-tmux): an image without tmux needs it"

setup_test

NS="${FLEET_ITEST_NAMESPACE}-restricted-b"
_be_kubectl create namespace "${NS}" >/dev/null 2>&1 || true
_be_kubectl label --overwrite namespace "${NS}" pod-security.kubernetes.io/enforce=restricted >/dev/null
out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name restricted -n "${NS}" 2>&1) || fail "cluster add: ${out}"

# busybox: a shell and an HTTP server, no package manager (nor tmux).
cat >"${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json" <<'JSON'
{
  "name": "fleet-man no-package-manager fixture",
  "image": "busybox:1.36.1"
}
JSON
(cd "${FIXTURE_REPO_DIR}" && git commit -qam "fixture: busybox")

inst="${FIXTURE_REPO_NAME}/alpha"
info "fleet up alpha on the restricted connection"
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" --cluster restricted

uid=$("${FLEET_BIN}" exec "${inst}" -- id -u)
[ "${uid}" != "0" ] || fail "the restricted pod runs as root"
for pm in apt-get apk dnf yum; do
  found=$("${FLEET_BIN}" exec "${inst}" -- sh -c "command -v ${pm} || true")
  assert_equals "" "${found}" "the image should have no ${pm}"
done

info "an HTTP server in the instance, on its loopback"
"${FLEET_BIN}" exec "${inst}" -- sh -c 'mkdir -p /tmp/www && echo "busybox-web" > /tmp/www/index.html && nohup httpd -f -p 127.0.0.1:17380 -h /tmp/www >/tmp/httpd.log 2>&1 </dev/null &'

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
tui_wait_for "alpha" 30 || fail "the TUI never listed alpha"
tui_wait_for "○ idle" 120 || fail "alpha never showed running"
tui_cursor_to "▶ ○ alpha"
info "press 'b'"
tui_send b
deadline=$(( $(date +%s) + $(_scale_timeout 180) ))
until [ -s "${launches}" ]; do
  if tui_capture | grep -q "Browser proxy error"; then fail "'b' failed: $(tui_capture)"; fi
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the browser was never launched: $(tui_capture)"
  sleep 1
done
proxy_port=$(grep -oE -- '--proxy-server=http://localhost:[0-9]+' "${launches}" | head -n 1 | grep -oE '[0-9]+$' || true)
[ -n "${proxy_port}" ] || fail "the browser was launched without a proxy: $(cat "${launches}")"
proxy="http://127.0.0.1:${proxy_port}"

info "through the proxy: plain HTTP and a CONNECT tunnel"
assert_equals "busybox-web" "$(curl -sS --max-time 30 -x "${proxy}" -f http://localhost:17380/)" "GET through the proxy"
assert_equals "busybox-web" "$(curl -sS --max-time 30 -x "${proxy}" -f -p http://localhost:17380/)" "GET through a CONNECT tunnel"

info "the proxy is fleet's own, on the pod's loopback"
proxy_cmd=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'tr "\0" " " < "/proc/$(cat /tmp/fleet-browser-proxy.pid)/cmdline"')
assert_contains "${proxy_cmd}" "browser-proxy --port 58888 --addr 127.0.0.1" "the proxy should be fleet's, bound to the loopback"
listening=$("${FLEET_BIN}" exec "${inst}" -- sh -c "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null | awk '\$4 == \"0A\" { split(\$2, a, \":\"); if (a[2] == \"E608\") print a[1] }' | sort -u | tr '\n' ' '")
assert_equals "0100007F " "${listening}" "nothing listens for the proxy on the pod's network"

pass "the browser proxy on a restricted namespace, on an image with no package manager"
