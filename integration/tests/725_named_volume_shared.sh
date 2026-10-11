#!/usr/bin/env bash
# itest: k8s
# Description: a devcontainer's named volume (not ${devcontainerId}'s) is copied up from the image when new, shared by every instance of the fleet naming it (a write in one is seen in the other), kept while one of them is left, and gone with the last: the next instance gets it copied up fresh. It never lands in the fleet's folders on the daemon host.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-volume" setup_test
fleet_dir=$(itest_share_dir "${FIXTURE_REPO_NAME}")
ref() { printf '%s/%s\n' "${FIXTURE_REPO_NAME}" "$1"; }

info "fleet up alpha and beta (both name the volume)"
fleet_up alpha
fleet_up beta

"${FLEET_BIN}" exec "$(ref alpha)" -- test -f /etc/skel/.bashrc \
  || fail "the new volume was not copied up from the image's /etc/skel"
"${FLEET_BIN}" exec "$(ref alpha)" -- sudo sh -c 'printf "from-alpha\n" > /etc/skel/from-alpha'
got=$("${FLEET_BIN}" exec "$(ref beta)" -- cat /etc/skel/from-alpha)
assert_contains "${got}" "from-alpha" "a write to the volume in alpha is not seen in beta"

info "fleet down alpha: beta still has the volume"
"${FLEET_BIN}" down "$(ref alpha)" >/dev/null
got=$("${FLEET_BIN}" exec "$(ref beta)" -- cat /etc/skel/from-alpha)
assert_contains "${got}" "from-alpha" "destroying one of two instances took the shared volume"

info "fleet down beta (its last instance) and up gamma: a fresh volume"
"${FLEET_BIN}" down "$(ref beta)" >/dev/null
fleet_up gamma
"${FLEET_BIN}" exec "$(ref gamma)" -- test -f /etc/skel/.bashrc \
  || fail "the volume made again was not copied up from the image"
if "${FLEET_BIN}" exec "$(ref gamma)" -- test -e /etc/skel/from-alpha; then
  fail "the volume outlived its last instance"
fi

if [ -d "${fleet_dir}" ] && find "${fleet_dir}" -name from-alpha | grep -q .; then
  fail "the named volume's content reached the fleet's folders on the daemon host"
fi

pass "named volume copied up, shared, removed with its last instance"
