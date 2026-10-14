#!/usr/bin/env bash
# itest: k8s
# Description: an instance with the published Go feature gets the debug grants the feature asks for (CAP_SYS_PTRACE, seccomp unconfined), runs under an init process, has the feature's Go toolchain and env, and keeps them across stop/start.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-go-feature" setup_test

inst="${FIXTURE_REPO_NAME}/alpha"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }

info "fleet up alpha (go feature)"
up_out=$(fleet_up alpha 2>&1) || fail "fleet up failed: ${up_out}"
printf '%s\n' "${up_out}" | tail -n 3

# CAP_SYS_PTRACE is capability 19. The bounding set is what the container was
# granted (the effective set of a non-root shell is empty). Docker's default
# bounding set (0xa80425fb) leaves it out, and its default seccomp profile is a
# filter (Seccomp: 2); the feature asks for both to be lifted.
check_debug_grants() {  # <when>
  local status cap_bnd seccomp
  status=$(in_instance cat /proc/self/status)
  cap_bnd=$(printf '%s\n' "${status}" | awk '$1 == "CapBnd:" { print $2 }')
  seccomp=$(printf '%s\n' "${status}" | awk '$1 == "Seccomp:" { print $2 }')
  info "${1}: CapBnd=${cap_bnd} Seccomp=${seccomp}"
  [ -n "${cap_bnd}" ] || fail "no CapBnd line in /proc/self/status (${1})"
  if [ $(( 0x${cap_bnd} >> 19 & 1 )) -ne 1 ]; then
    fail "CAP_SYS_PTRACE is missing from the bounding set ${cap_bnd} (${1})"
  fi
  assert_equals "0" "${seccomp}" "seccomp should be unconfined, as the feature asks (${1})"
}
check_debug_grants "after create"

# init: true — PID 1 is an init process reaping orphans, not the entrypoint.
if itest_backend_is kubernetes; then
  # A pod's PID 1 is always fleet's pod-init, which init: true puts in its
  # reaper mode (forwarding every signal, as tini does).
  pid1=$(in_instance sh -c "tr '\\0' ' ' </proc/1/cmdline")
  assert_contains "${pid1}" "pod-init" "PID 1 should be fleet's pod-init"
  pid1_spec=$(itest_k8s_spec_file "$(itest_container_id "${inst}")" pid1.json)
  printf '%s' "${pid1_spec}" | grep -qE '"init": *true' \
    || fail "pod-init should run in its reaper mode (the feature sets init: true): ${pid1_spec}"
else
  pid1=$(in_instance cat /proc/1/comm)
  case "${pid1}" in
    docker-init|tini|catatonit|dumb-init) info "PID 1 is ${pid1}" ;;
    *) fail "PID 1 should be an init process (the feature sets init: true), got ${pid1}" ;;
  esac
fi

# The feature's payload and containerEnv.
go_out=$(in_instance bash -lc 'go version')
assert_contains "${go_out}" "go version go1.24" "the feature's Go toolchain should be on PATH"
assert_equals "/go" "$(in_instance sh -c 'printf "%s\n" "$GOPATH"')" "the feature's GOPATH should be set"
in_instance sh -c 'cd "$(mktemp -d)" && printf "package main\nfunc main() { println(\"built\") }\n" > main.go && go mod init itest >/dev/null 2>&1 && go build -o hello . && ./hello' 2>&1 \
  | grep -qx built || fail "go build should work in the instance"

info "fleet stop + start alpha"
"${FLEET_BIN}" stop "${inst}" >/dev/null
"${FLEET_BIN}" start "${inst}" >/dev/null
check_debug_grants "after a stop and start"

pass "go feature: debug grants, init and toolchain"
