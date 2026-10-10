#!/usr/bin/env bash
# Description: `fleet code --print` prints the VS Code command for an instance as one shell line, `fleet code` and the TUI's 'c' run exactly that command, and its dev-container URI names the instance's workspace and an in-container folder holding the checkout.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
workdir=$(mktemp -d)
itest_cleanup() {
  tui_kill
  rm -rf "${workdir}"
}
itest_begin

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

info "fleet code --print alpha"
printed=$("${FLEET_BIN}" code --print "${inst}")
info "printed: ${printed}"
assert_not_contains "${printed}" "Opening VS Code" "--print should print only the command"
assert_equals "1" "$(printf '%s\n' "${printed}" | wc -l | tr -d '[:space:]')" "--print should print one line"

# A stand-in for the `code` CLI: it records each run's arguments, one per
# line, then a "--" line. The TUI must see it too: export covers a fresh tmux
# server, set-environment -g one that already runs.
runs="${workdir}/code-runs.log"
mkdir -p "${workdir}/bin"
cat > "${workdir}/bin/code" <<STUB
#!/bin/sh
for arg in "\$@"; do echo "\${arg}"; done >> "${runs}"
echo -- >> "${runs}"
STUB
chmod +x "${workdir}/bin/code"
export PATH="${workdir}/bin:${PATH}"
tmux set-environment -g PATH "${PATH}" >/dev/null 2>&1 || true

info "the printed line, run by a shell, runs the editor"
case "${printed}" in
  "code "*) ;;
  *) fail "the printed command should run the code CLI: ${printed}" ;;
esac
eval "${printed}"
want=$(cat "${runs}")
[ -n "${want}" ] || fail "running the printed line did not run the editor"

info "fleet code alpha runs the printed command"
: > "${runs}"
out=$("${FLEET_BIN}" code "${inst}")
assert_contains "${out}" "Opening VS Code for ${inst}..." "fleet code should announce the open"
assert_equals "${want}" "$(cat "${runs}")" "the arguments fleet code gave the editor"

info "the TUI's 'c' runs the same command"
: > "${runs}"
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60
tui_send j
sleep 0.3
tui_send c
tui_wait_for "Opened VS Code for alpha" 15
assert_equals "${want}" "$(cat "${runs}")" "the arguments the TUI gave the editor"

if itest_backend_is devcontainer; then
  # VS Code's Dev Containers extension opens vscode-remote://dev-container+
  # <hex of the host workspace>/<folder>: it finds the container by the
  # workspace, through the label `devcontainer up` stamped on it. (Cluster
  # instances open over Remote-SSH instead, P7.)
  info "the dev-container URI names the instance's workspace and its folder in the container"
  uri=$(printf '%s\n' "${want}" | sed -n 2p)
  assert_equals "--folder-uri" "$(printf '%s\n' "${want}" | sed -n 1p)" "the editor's first argument"
  case "${uri}" in
    vscode-remote://dev-container+*) ;;
    *) fail "expected a dev-container URI, got ${uri}" ;;
  esac
  authority="${uri#vscode-remote://dev-container+}"
  folder="/${authority#*/}"
  workspace=$(perl -e 'print pack("H*", $ARGV[0])' "${authority%%/*}")
  assert_equals "$(itest_host_workspace_dir "${inst}")" "${workspace}" "the URI's hex-encoded workspace"
  itest_container_belongs_to "$(itest_container_id "${inst}")" "${inst}" \
    || fail "the URI's workspace does not identify the instance's container"
  assert_equals "/workspaces/${FIXTURE_REPO_NAME}" "${folder}" "the folder VS Code opens"
  "${FLEET_BIN}" exec "${inst}" -- test -f "${folder}/README.md" \
    || fail "${folder} in the instance should hold the checkout"
fi

pass "editor launch command"
