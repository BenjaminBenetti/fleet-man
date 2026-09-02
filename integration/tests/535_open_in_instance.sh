#!/usr/bin/env bash
# Description: in-instance `fleet open` (fo) delegates to the attached TUI: the human confirms an "Open request", the file lands in ~/Downloads and is handed to the opener.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
workdir=$(mktemp -d)
itest_cleanup() { tui_kill; rm -rf "${workdir}"; rm -f "${HOME}/Downloads/fo-itest.txt"; }
itest_begin

setup_test
fleet_up alpha

# A stub opener standing in for xdg-open on the TUI machine: it records the
# arguments it was invoked with. It must reach the TUI's environment — export
# covers a fresh tmux server, set-environment -g a server that already exists.
opener="${workdir}/opener.sh"
log="${workdir}/opened.log"
cat > "${opener}" <<STUB
#!/bin/sh
printf '%s\n' "\$*" >> "${log}"
STUB
chmod +x "${opener}"
export FLEET_OPENER="${opener}"
tmux set-environment -g FLEET_OPENER "${opener}" >/dev/null 2>&1 || true

# The 1-arg form delivers to ~/Downloads when it exists.
mkdir -p "${HOME}/Downloads"
rm -f "${HOME}/Downloads/fo-itest.txt"

info "create a file inside the instance"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "printf 'opened-bytes' > /tmp/fo-itest.txt"

info "the staged fleet.rc provides the fo alias"
alias_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- bash -ic 'type fo' 2>/dev/null || true)
assert_contains "${alias_out}" "fleet open" "fo should be an alias for fleet open"

info "an instance destination is rejected inside the instance before anything is sent"
set +e
out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- fleet open /tmp/fo-itest.txt /tmp/elsewhere 2>&1)
rc=$?
set -e
[ "${rc}" -ne 0 ] || fail "expected non-zero exit for a this-instance destination, got 0"
assert_contains "${out}" "host:" "the error should steer to host:path"

# Attach a TUI: it becomes the Watch subscriber the server opens the control
# socket for (as in 530), and the machine that performs the copy + open.
tui_spawn
tui_wait_for "alpha" 15

socket="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/.control/fleet.sock"
info "waiting for the server to open the control socket (TUI now attached)"
deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if [ -S "${socket}" ]; then break; fi
  sleep 0.25
done
assert_file_exists "${socket}"

info "fleet open /tmp/fo-itest.txt inside the instance"
out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- fleet open /tmp/fo-itest.txt 2>&1)
info "output: ${out}"
assert_contains "${out}" "and opening" "in-instance open should announce the delegated copy + open"

info "the TUI asks the human, naming the open as an effect"
tui_wait_for "Open request from ${FIXTURE_REPO_NAME}/alpha" 15
tui_assert_contains "opens it with THIS machine" "the prompt must spell out that the file will be opened"

info "allow once"
tui_send a
tui_wait_for "Opened :/tmp/fo-itest.txt" 30

assert_equals "opened-bytes" "$(cat "${HOME}/Downloads/fo-itest.txt")" "the file should land in ~/Downloads"
assert_equals "${HOME}/Downloads/fo-itest.txt" "$(cat "${log}")" "the opener should receive the delivered path"

pass "in-instance fleet open"
