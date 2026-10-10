#!/usr/bin/env bash
# Description: TUI o opens an external terminal on the selected instance — with a stub terminal emulator first on PATH, o launches it with the attach argv, and the window it opens is a plain (non-tmux) shell inside the instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

# The stub stands in for a terminal emulator: ptyxis, the first one the TUI
# looks for. Like a real one it gives the command a terminal of its own, here
# a tmux window (named ext) in the TUI's session, so the test can type into it
# and read it back. It records the argv it was given first.
stub_dir="${TEST_SCRATCH_DIR}/terminal-stub"
argv_log="${TEST_SCRATCH_DIR}/terminal-stub.argv"
mkdir -p "${stub_dir}"
cat > "${stub_dir}/ptyxis" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$@" > "${argv_log}"
[ "\${1:-}" = "--" ] && shift
exec tmux new-window -d -t "${TUI_SESSION}:" -n ext "\$@"
EOF
chmod +x "${stub_dir}/ptyxis"
export PATH="${stub_dir}:${PATH}"

info "spawning TUI with the stub terminal on PATH"
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

info "selecting alpha and pressing o"
tui_send j
sleep 0.5
tui_send o
tui_wait_for "Opened terminal for alpha" 10 || fail "o did not report opening a terminal"

[ -s "${argv_log}" ] || fail "o did not launch the terminal emulator"
info "terminal argv: $(tr '\n' ' ' < "${argv_log}")"
assert_equals "--" "$(head -n 1 "${argv_log}")" "ptyxis should get its -- separator before the command"
[ "$(wc -l < "${argv_log}")" -gt 1 ] || fail "the terminal got no command to run"

deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
until tmux list-windows -t "${TUI_SESSION}" -F '#{window_name}' 2>/dev/null | grep -qxF ext; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the stub terminal never opened its window"
  sleep 0.25
done

# Type into the new terminal. The arithmetic expands only when the shell runs
# the line, and $TMUX tells a plain shell from a tmux attach (o opens a fresh,
# non-persistent shell, unlike the session rows).
# Keys typed before the remote shell is up can be lost on the way in, so the
# (idempotent) line is typed again every few seconds until it has run.
info "running a command in the opened terminal"
marker=""
next_send=0
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if [ "$(date +%s)" -ge "${next_send}" ]; then
    tmux send-keys -t "${TUI_SESSION}:ext" -l \
      '{ echo O_TERMINAL_$((6*7)); [ -n "$TMUX" ] && echo in-tmux || echo plain-shell; echo "user=$(whoami)"; } > /tmp/o-terminal-marker'
    tmux send-keys -t "${TUI_SESSION}:ext" Enter
    next_send=$(( $(date +%s) + 5 ))
  fi
  sleep 0.5
  marker=$("${FLEET_BIN}" exec "${inst}" -- cat /tmp/o-terminal-marker 2>/dev/null || true)
  printf '%s' "${marker}" | grep -qF "O_TERMINAL_42" && break
done
if ! printf '%s' "${marker}" | grep -qF "O_TERMINAL_42"; then
  printf -- '--- ext window ---\n%s\n--- end ---\n' "$(tmux capture-pane -t "${TUI_SESSION}:ext" -p 2>/dev/null || true)" >&2
  fail "the command typed into the opened terminal never ran inside the instance"
fi
printf 'marker: %s\n' "$(printf '%s' "${marker}" | tr '\n' ' ')"
# The image's bash may also write terminal-title escapes into the file (a
# DEBUG trap), so match the lines' content rather than whole lines.
assert_contains "${marker}" "plain-shell" "o should open a plain shell, not a tmux session"
assert_not_contains "${marker}" "in-tmux" "o should open a plain shell, not a tmux session"

# Same user as fleet exec: the instance's remote user.
want_user=$("${FLEET_BIN}" exec "${inst}" -- whoami)
assert_contains "${marker}" "user=${want_user}" "the opened shell should run as the instance's user"

pass "o launches the terminal emulator with a plain shell inside the instance"
