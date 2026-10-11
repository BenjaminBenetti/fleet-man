#!/usr/bin/env bash
# Description: with buildkit_server disabled, no buildkit server is created and the instance gets nothing of it: on docker no host .buildkit dir or socket mount, on a cluster no Deployment, TLS Secret or client certificate.
# itest: k8s
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
# claude=false codex=false homedir=/home/vscode gh=false buildkit=false
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false false

inst="${FIXTURE_REPO_NAME}/alpha"

info "fleet up alpha (buildkit disabled)"
fleet_up alpha

info "asserting no shared buildkit server was created"
st=$(itest_service_status "${FIXTURE_REPO_NAME}" buildkit)
assert_equals "absent" "${st}" "buildkit server should not exist when the setting is off"

if itest_backend_is kubernetes; then
  info "asserting no buildkit TLS Secret, volume or client certificate"
  for kind in secrets persistentvolumeclaims; do
    obj=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" "${kind}" buildkit)
    assert_equals "" "${obj}" "no buildkit ${kind} when the setting is off"
  done
  if "${FLEET_BIN}" exec "${inst}" -- sh -c 'test -e "$HOME/.fleet/buildkit"' 2>/dev/null; then
    fail "the instance got a buildkit credential with the setting off"
  fi
else
  info "asserting no host .buildkit directory was created"
  assert_file_absent "${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.buildkit"

  info "asserting nothing is mounted at /run/fleet-buildkit inside the instance"
  if "${FLEET_BIN}" exec "${inst}" -- test -e /run/fleet-buildkit/buildkitd.sock 2>/dev/null; then
    fail "buildkit socket should not be present inside the instance when disabled"
  fi
fi

pass "buildkit disabled: no server, and nothing of it in the instance"
