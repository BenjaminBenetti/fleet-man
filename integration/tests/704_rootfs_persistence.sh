#!/usr/bin/env bash
# Description: stop/start keeps the instance's whole root filesystem: an apt-installed package, $HOME, files root wrote under /usr/local and /etc, a new top-level directory, and uncommitted workspace changes.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

inst="${FIXTURE_REPO_NAME}/alpha"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }
container_id=$(itest_container_id "${inst}")
[ -n "${container_id}" ] || fail "could not read alpha's container_id from state.json"

info "writing state across the root filesystem"
itest_install_local_deb "${inst}" fleet-itest-rootfs
in_instance sh -c 'printf "home-file\n" > "$HOME/.itest-rootfs-home" && mkdir -p "$HOME/.cache/itest" && printf "nested\n" > "$HOME/.cache/itest/nested"'
in_instance sudo sh -c 'printf "usr-local\n" > /usr/local/share/itest-rootfs && printf "etc-file\n" > /etc/itest-rootfs.conf'
# A directory the image does not have, at the top of the tree.
in_instance sudo sh -c 'mkdir -p /itest-top/sub && printf "top-dir\n" > /itest-top/sub/file && chown -R "$(id -u vscode):$(id -g vscode)" /itest-top'
# A symlink and a mode that a copy-based restore could lose.
in_instance sh -c 'ln -s .itest-rootfs-home "$HOME/.itest-rootfs-link" && printf "#!/bin/sh\necho mode-ok\n" > "$HOME/.itest-rootfs-exec" && chmod 0750 "$HOME/.itest-rootfs-exec"'
itest_workspace_write "${inst}" UNCOMMITTED.txt "uncommitted-before-stop"
itest_workspace_write "${inst}" README.md "readme-edited-before-stop"
status_before=$(itest_workspace_git "${inst}" status --porcelain)

check_rootfs() {  # <when>
  local when="$1"
  assert_equals "installed" "$(itest_local_deb_status "${inst}" fleet-itest-rootfs)" \
    "the apt-installed package should survive ${when}"
  assert_equals "home-file" "$(in_instance sh -c 'cat "$HOME/.itest-rootfs-home"')" "\$HOME should survive ${when}"
  assert_equals "nested" "$(in_instance sh -c 'cat "$HOME/.cache/itest/nested"')" \
    "a nested \$HOME directory should survive ${when}"
  assert_equals "usr-local" "$(in_instance cat /usr/local/share/itest-rootfs)" "/usr/local should survive ${when}"
  assert_equals "etc-file" "$(in_instance cat /etc/itest-rootfs.conf)" "/etc should survive ${when}"
  assert_equals "top-dir" "$(in_instance cat /itest-top/sub/file)" \
    "a new top-level directory should survive ${when}"
  assert_equals ".itest-rootfs-home" "$(in_instance sh -c 'readlink "$HOME/.itest-rootfs-link"')" \
    "a symlink should survive ${when}"
  assert_equals "750 mode-ok" "$(in_instance sh -c 'printf "%s " "$(stat -c %a "$HOME/.itest-rootfs-exec")"; "$HOME/.itest-rootfs-exec"')" \
    "an executable's mode should survive ${when}"
  assert_equals "uncommitted-before-stop" "$(itest_workspace_read "${inst}" UNCOMMITTED.txt)" \
    "an untracked workspace file should survive ${when}"
  assert_equals "${status_before}" "$(itest_workspace_git "${inst}" status --porcelain)" \
    "the workspace's uncommitted changes should survive ${when}"
}
check_rootfs "the writes (sanity)"

info "fleet stop + start alpha"
"${FLEET_BIN}" stop "${inst}" >/dev/null
assert_container_stopped "${container_id}" "alpha should be stopped"
"${FLEET_BIN}" start "${inst}" >/dev/null
assert_container_running "${container_id}" "start should resume the same container"
check_rootfs "a stop and start"

info "a second stop + start"
"${FLEET_BIN}" stop "${inst}" >/dev/null
"${FLEET_BIN}" start "${inst}" >/dev/null
check_rootfs "a second stop and start"

pass "stop/start keeps the root filesystem and the workspace"
