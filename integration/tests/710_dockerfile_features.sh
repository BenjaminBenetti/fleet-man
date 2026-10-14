#!/usr/bin/env bash
# itest: k8s
# Description: an instance builds from a Dockerfile (build args, repo-root build context) plus a local feature with an option, and runs as remoteUser with containerEnv, remoteEnv and the feature's containerEnv set.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-dockerfile" setup_test

inst="${FIXTURE_REPO_NAME}/alpha"

info "fleet up alpha (Dockerfile + local feature)"
up_out=$(fleet_up alpha 2>&1) || fail "fleet up failed: ${up_out}"
printf '%s\n' "${up_out}" | tail -n 5
assert_container_running "$(itest_container_id "${inst}")" "the built instance should be running"

in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }

# Dockerfile: its RUN saw the build arg devcontainer.json passed...
assert_equals "from-build-args" "$(in_instance cat /etc/fleet-itest-build-arg)" \
  "the Dockerfile RUN should see build.args"
# ...and its COPY read from the repo root, the build.context.
assert_equals "$(itest_workspace_read "${inst}" README.md)" "$(in_instance cat /etc/fleet-itest-context-readme)" \
  "the Dockerfile COPY should read the build.context (the repo root)"

# Local feature: installed on top of the Dockerfile image, with the option
# devcontainer.json set, for the remote user.
assert_equals "itest-feature-ok" "$(in_instance itest-feature)" "the local feature's binary should be on PATH"
assert_equals "hello-from-feature-option" "$(in_instance cat /usr/local/share/itest-feature-greeting)" \
  "the feature should get the option devcontainer.json set"
assert_equals "vscode" "$(in_instance cat /usr/local/share/itest-feature-remote-user)" \
  "the feature should install for the remote user"

# Runtime config: remoteUser, containerEnv, remoteEnv and the feature's containerEnv.
assert_equals "vscode" "$(in_instance whoami)" "fleet exec should run as remoteUser"
env_out=$(in_instance env)
assert_contains "${env_out}" "ITEST_CONTAINER_ENV=from-containerEnv" "containerEnv should be set"
assert_contains "${env_out}" "ITEST_REMOTE_ENV=from-remoteEnv" "remoteEnv should be set for fleet exec"
assert_contains "${env_out}" "ITEST_FEATURE_ENV=from-feature" "the feature's containerEnv should be set"

pass "Dockerfile + local feature build and runtime config"
