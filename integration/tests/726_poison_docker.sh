#!/usr/bin/env bash
# itest: k8s
# Description: the poison-docker shim intercepts every docker call fleetd, the devcontainer CLI and the fleet CLI make: in record mode a devcontainer lifecycle passes through it and is logged, and in poison mode container verbs are refused (exit 125) while image, build and version calls still pass.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
# The shim reaches the daemon through its environment; stop the daemon so a
# later test gets one with a plain PATH.
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

setup_test

inst="${FIXTURE_REPO_NAME}/alpha"

# Docker-backed instances are docker containers, so the shim only records
# there; every other backend runs the whole lifecycle with docker poisoned.
if itest_backend_is devcontainer; then mode=record; else mode=poison; fi
info "installing the docker shim (${mode})"
itest_poison_docker "${mode}"
assert_equals "${INTEGRATION_DIR}/poison-docker/docker" "$(command -v docker)" "the shim should come first on PATH"

info "lifecycle: up, exec, logs, stop, start"
fleet_up alpha
cid=$(itest_container_id "${inst}")
[ -n "${cid}" ] || fail "could not read alpha's container_id from state.json"
assert_equals "through-the-shim" "$("${FLEET_BIN}" exec "${inst}" -- echo through-the-shim)" "exec"
"${FLEET_BIN}" logs "${inst}" >/dev/null 2>&1 || fail "fleet logs failed"
"${FLEET_BIN}" stop "${inst}" >/dev/null
"${FLEET_BIN}" start "${inst}" >/dev/null
assert_equals "after-restart" "$("${FLEET_BIN}" exec "${inst}" -- echo after-restart)" "exec after a restart"

info "the shim saw $(itest_poison_docker_log | wc -l | tr -d ' ') docker call(s)"
# show_calls <ERE> — the logged calls matching <ERE>, for a failure message.
show_calls() { itest_poison_docker_log | grep -E -- "$1" | cut -c1-200 | head -n 20 || true; }

if itest_backend_is devcontainer; then
  # The devcontainer CLI (run by fleetd) created the container through it...
  itest_poison_docker_saw '^pass run .*devcontainer\.local_folder=' \
    || fail "the shim never saw the devcontainer CLI's docker run: $(show_calls '^pass run ')"
  # ...and fleet drove that container through it afterwards.
  for verb in exec logs stop start; do
    itest_poison_docker_saw "^pass ${verb} .*${cid}" \
      || fail "the shim never saw docker ${verb} on alpha's container ${cid}: $(show_calls "^pass ${verb} ")"
  done
  if itest_poison_docker_saw '^refused '; then
    fail "record mode must not refuse anything: $(show_calls '^refused ')"
  fi

  # Poison mode bites: the same instance can no longer be driven.
  info "switching the shim to poison"
  itest_poison_docker poison
  if out=$("${FLEET_BIN}" exec "${inst}" -- echo should-not-run 2>&1); then
    fail "fleet exec should fail with container verbs poisoned, got: ${out}"
  fi
  itest_poison_docker_saw '^refused ' \
    || fail "the failed exec should show refused calls in the log: $(show_calls .)"
else
  # Nothing in a cluster backend's lifecycle may treat its instances as docker
  # containers.
  if itest_poison_docker_saw '^refused '; then
    fail "the ${FLEET_ITEST_BACKEND} lifecycle made poisoned docker calls: $(show_calls '^refused ')"
  fi
fi

# In poison mode, what builds and inspects images still goes through.
info "non-container verbs pass the poisoned shim"
docker version --format '{{.Server.Version}}' >/dev/null || fail "docker version should pass the poisoned shim"
docker image ls >/dev/null || fail "docker image ls should pass the poisoned shim"
docker inspect --type image mcr.microsoft.com/devcontainers/base:debian-12 >/dev/null \
  || fail "docker inspect --type image should pass the poisoned shim"
rc=0
docker ps >/dev/null 2>&1 || rc=$?
assert_equals "125" "${rc}" "docker ps should be refused with exit 125"
assert_contains "$(docker ps 2>&1 || true)" "poison docker: refused" "a refusal should say why"

pass "poison-docker shim records, passes through, and refuses container verbs"
