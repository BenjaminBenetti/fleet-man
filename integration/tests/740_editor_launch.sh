#!/usr/bin/env bash
# itest: k8s
# Description: `fleet code --print` prints the VS Code command for an instance as one shell line, the TUI's 'c' and `fleet code` run exactly that command; a local devcontainer instance opens through the dev-container URI, a cluster instance through Remote-SSH to its *.fleet alias, whose ~/.ssh/config Include needs consent first (the TUI's dialog; `fleet code` without a terminal changes nothing).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../editor/editor.sh"
workdir=$(mktemp -d)
itest_cleanup() {
  tui_kill
  itest_editor_cleanup
  rm -rf "${workdir}"
}
itest_begin

setup_test
itest_editor_isolate_ssh
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

info "fleet code --print alpha"
printed=$("${FLEET_BIN}" code --print "${inst}" 2>"${workdir}/print.err")
info "printed: ${printed}"
assert_not_contains "${printed}" "Opening VS Code" "--print should print only the command"
assert_equals "1" "$(printf '%s\n' "${printed}" | wc -l | tr -d '[:space:]')" "--print should print one line"
remote_ssh=false
case "${printed}" in
  *"vscode-remote://ssh-remote+"*) remote_ssh=true ;;
esac
if itest_backend_is kubernetes && [ "${remote_ssh}" != true ]; then
  fail "a cluster instance should open over Remote-SSH: ${printed}"
fi
if itest_backend_is devcontainer && [ "${remote_ssh}" = true ]; then
  fail "a local devcontainer instance should open through its dev-container URI: ${printed}"
fi

# A stand-in for the `code` CLI: it records each run's arguments, one per
# line, then a "--" line, and lists the Remote-SSH extension without
# recording. The TUI must see it too: export covers a fresh tmux server,
# set-environment -g one that already runs.
runs="${workdir}/code-runs.log"
mkdir -p "${workdir}/bin"
cat > "${workdir}/bin/code" <<STUB
#!/bin/sh
if [ "\$1" = --list-extensions ]; then echo ms-vscode-remote.remote-ssh; exit 0; fi
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

if [ "${remote_ssh}" = true ]; then
  assert_contains "$(cat "${workdir}/print.err")" "Include fleet_config" "--print should say the Include is missing"
  info "without the Include, fleet code (no terminal) changes nothing and runs nothing"
  : > "${runs}"
  set +e
  out=$("${FLEET_BIN}" code "${inst}" </dev/null 2>&1)
  rc=$?
  set -e
  [ "${rc}" -ne 0 ] || fail "fleet code opened the editor without the Include: ${out}"
  assert_contains "${out}" "Include fleet_config" "the refusal should give the manual steps"
  [ ! -s "${runs}" ] || fail "fleet code ran the editor without the Include"
  [ ! -e "${HOME}/.ssh/config" ] || fail "fleet code wrote ~/.ssh/config without consent"
fi

info "the TUI's 'c' runs the same command"
: > "${runs}"
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60
tui_send j
sleep 0.3
tui_send c
if [ "${remote_ssh}" = true ]; then
  # First time: the consent dialog; "Allow and open" is selected.
  tui_wait_for "Open in VS Code" 20 || fail "no consent dialog: $(tui_capture)"
  tui_assert_contains "Include fleet_config" "the dialog names the line it adds"
  tui_send Enter
fi
deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
until grep -qx -- '--' "${runs}"; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "'c' never ran the editor: $(tui_capture)"
  sleep 0.25
done
assert_equals "${want}" "$(cat "${runs}")" "the arguments the TUI gave the editor"
if [ "${remote_ssh}" = true ]; then
  assert_contains "$(head -1 "${HOME}/.ssh/config")" "Include fleet_config" "consent put the Include first in ~/.ssh/config"
fi

info "fleet code alpha runs the printed command"
: > "${runs}"
out=$("${FLEET_BIN}" code "${inst}")
assert_contains "${out}" "Opening VS Code for ${inst}..." "fleet code should announce the open"
assert_equals "${want}" "$(cat "${runs}")" "the arguments fleet code gave the editor"

if [ "${remote_ssh}" = true ]; then
  # Remote-SSH opens <alias><workspaceFolder>; the alias routes through
  # ~/.ssh/fleet_config to the instance's ssh-server as its remoteUser.
  info "the Remote-SSH URI names the instance's alias and its folder"
  alias=$(itest_editor_uri_part "${printed}" alias)
  folder=$(itest_editor_uri_part "${printed}" folder)
  assert_equals "alpha.${FIXTURE_REPO_NAME}.fleet" "${alias}" "the alias on the local daemon"
  assert_equals "$("${FLEET_BIN}" exec "${inst}" -- id -un)" "$(itest_editor_ssh "${alias}" whoami)" "the alias reaches the remoteUser"
  assert_equals "yes" "$(itest_editor_ssh "${alias}" "test -f '${folder}/README.md' && echo yes")" "the folder holds the checkout"
else
  # VS Code's Dev Containers extension opens vscode-remote://dev-container+
  # <hex of the host workspace>/<folder>: it finds the container by the
  # workspace, through the label `devcontainer up` stamped on it.
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
