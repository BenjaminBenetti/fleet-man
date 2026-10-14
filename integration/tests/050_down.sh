#!/usr/bin/env bash
# itest: k8s
# Description: `fleet down` removes the container, wipes the workspace, and drops the state entry.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

inst="${FIXTURE_REPO_NAME}/alpha"
container_id=$(itest_container_id "${inst}")
[ -n "${container_id}" ] || fail "could not read container_id"
assert_workspace_exists "${inst}" "the instance workspace should exist after up"

info "fleet down alpha"
down_out=$("${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/alpha")
printf '%s\n' "${down_out}"
assert_contains "${down_out}" "removed" "down should mention removed"

# Container gone.
assert_container_absent "${container_id}" "container still exists after down"

# Workspace gone (devcontainer: the clone under
# ~/.fleet/workspaces/<fleet>/<instance>/<fleet>).
assert_workspace_absent "${inst}" "the instance workspace should be gone after down"

# State no longer has the instance.
state=$(cat "${HOME}/.fleet/state.json")
assert_not_contains "${state}" "\"name\": \"alpha\"" "instance should be removed from state"

# `ls` no longer lists it.
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_not_contains "${ls_out}" "alpha" "ls should not list removed instance"

pass "down"
