#!/usr/bin/env bash
# Description: `fleet rebuild` after editing the instance's own devcontainer config (Dockerfile, devcontainer.json, a feature option) builds and runs the edited config, from a running and from a stopped instance, keeping the workspace and resetting the root filesystem.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-dockerfile" setup_test

inst="${FIXTURE_REPO_NAME}/alpha"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }

fleet_up alpha
first_container=$(itest_container_id "${inst}")
[ -n "${first_container}" ] || fail "could not read alpha's container_id from state.json"
assert_equals "from-build-args" "$(in_instance cat /etc/fleet-itest-build-arg)" "baseline image"
assert_equals "hello-from-feature-option" "$(in_instance cat /usr/local/share/itest-feature-greeting)" "baseline feature option"

# Root filesystem state the rebuild is expected to drop (a fresh container from
# the rebuilt image), and workspace state it must keep.
itest_install_local_deb "${inst}" fleet-itest-rebuild
in_instance sh -c 'printf "before-rebuild\n" > "$HOME/.itest-rebuild-home"'
itest_workspace_write "${inst}" UNCOMMITTED.txt "kept-through-rebuild"

# --- edit 1, rebuilt while running: the Dockerfile and devcontainer.json ---
# Edits go into the instance's own checkout, the way a user edits them in place.
info "editing the Dockerfile and devcontainer.json in alpha's workspace"
itest_workspace_write "${inst}" .devcontainer/Dockerfile "$(cat <<'EOF'
FROM mcr.microsoft.com/devcontainers/base:debian-12
ARG ITEST_BUILD_ARG=unset
RUN printf '%s\n' "${ITEST_BUILD_ARG}" > /etc/fleet-itest-build-arg
COPY README.md /etc/fleet-itest-context-readme
RUN printf 'added-by-edit\n' > /etc/fleet-itest-edited-dockerfile
EOF
)"
devcontainer_json() {  # <build arg> <feature greeting> <containerEnv value>
  cat <<EOF
{
  "name": "fleet-man Dockerfile + features fixture (edited)",
  "build": {
    "dockerfile": "Dockerfile",
    "context": "..",
    "args": { "ITEST_BUILD_ARG": "$1" }
  },
  "features": {
    "./itest-feature": { "greeting": "$2" }
  },
  "containerEnv": { "ITEST_CONTAINER_ENV": "$3" },
  "remoteEnv": { "ITEST_REMOTE_ENV": "from-remoteEnv" },
  "remoteUser": "vscode"
}
EOF
}
itest_workspace_write "${inst}" .devcontainer/devcontainer.json \
  "$(devcontainer_json build-arg-edit-1 hello-from-feature-option containerEnv-edit-1)"

info "fleet rebuild alpha (running)"
rebuild_out=$("${FLEET_BIN}" rebuild "${inst}" 2>&1) || fail "rebuild failed: ${rebuild_out}"
assert_contains "${rebuild_out}" "rebuilt" "rebuild output should mention rebuilt"
second_container=$(itest_container_id "${inst}")
[ -n "${second_container}" ] && [ "${second_container}" != "${first_container}" ] \
  || fail "rebuild should replace the container (was ${first_container}, now ${second_container:-none})"
assert_container_absent "${first_container}" "rebuild should remove the old container"
assert_container_running "${second_container}" "the rebuilt container should be running"

assert_equals "added-by-edit" "$(in_instance cat /etc/fleet-itest-edited-dockerfile)" \
  "the rebuilt image should include the edited Dockerfile's new step"
assert_equals "build-arg-edit-1" "$(in_instance cat /etc/fleet-itest-build-arg)" \
  "the rebuilt image should use the edited build arg"
assert_contains "$(in_instance env)" "ITEST_CONTAINER_ENV=containerEnv-edit-1" \
  "the rebuilt container should use the edited containerEnv"

# Kept: the workspace, including the edits and uncommitted work. Reset: the
# root filesystem. Re-staged: the in-instance fleet binary and its rc.
assert_equals "kept-through-rebuild" "$(itest_workspace_read "${inst}" UNCOMMITTED.txt)" \
  "rebuild must keep uncommitted workspace files"
assert_equals "not in the dpkg database" "$(itest_local_deb_status "${inst}" fleet-itest-rebuild)" \
  "rebuild starts from the rebuilt image, so a package installed before it is gone"
in_instance sh -c '! test -e "$HOME/.itest-rebuild-home"' \
  || fail "rebuild starts from the rebuilt image, so \$HOME should not keep a file written before it"
assert_equals "/usr/bin/fleet" "$(in_instance sh -c 'command -v fleet')" \
  "rebuild should re-stage the in-instance fleet binary"
assert_equals "alpha" "$(in_instance bash -c '. "$HOME/.fleet/fleet.rc" >/dev/null 2>&1; printf "%s\n" "${FLEET_INSTANCE_NAME:-}"')" \
  "rebuild should re-stage fleet.rc for the instance"

# --- edit 2, rebuilt while stopped: a feature option and containerEnv ---
# Edited before the stop: a cluster backend reaches the workspace only through
# the running instance. The rebuild reads it either way.
info "edit the feature option and containerEnv, then fleet stop alpha"
itest_workspace_write "${inst}" .devcontainer/devcontainer.json \
  "$(devcontainer_json build-arg-edit-1 greeting-edit-2 containerEnv-edit-2)"
"${FLEET_BIN}" stop "${inst}" >/dev/null
assert_container_stopped "${second_container}" "alpha should be stopped before the second rebuild"

info "fleet rebuild alpha (stopped)"
rebuild_out=$("${FLEET_BIN}" rebuild "${inst}" 2>&1) || fail "rebuild of a stopped instance failed: ${rebuild_out}"
third_container=$(itest_container_id "${inst}")
[ -n "${third_container}" ] && [ "${third_container}" != "${second_container}" ] \
  || fail "rebuild from stopped should replace the container (was ${second_container}, now ${third_container:-none})"
assert_container_absent "${second_container}" "rebuild from stopped should remove the old container"
assert_container_running "${third_container}" "rebuild from stopped should leave the instance running"
ls_row=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" | grep -E '^\S+\s+alpha\s' || true)
assert_contains "${ls_row}" "running" "ls should show alpha running after a rebuild from stopped"

assert_equals "greeting-edit-2" "$(in_instance cat /usr/local/share/itest-feature-greeting)" \
  "the rebuilt image should re-run the feature with its edited option"
assert_contains "$(in_instance env)" "ITEST_CONTAINER_ENV=containerEnv-edit-2" \
  "the rebuilt container should use the second containerEnv edit"
assert_equals "added-by-edit" "$(in_instance cat /etc/fleet-itest-edited-dockerfile)" \
  "the first edit's Dockerfile step should still be there"
assert_equals "kept-through-rebuild" "$(itest_workspace_read "${inst}" UNCOMMITTED.txt)" \
  "a rebuild from stopped must keep uncommitted workspace files"

pass "rebuild applies edited Dockerfile/devcontainer.json/feature options, running and stopped"
