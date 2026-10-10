#!/bin/sh
# Smoke test for the static tmux bundle: smoke.sh PREFIX [TMUX_VERSION]
#
# Meant for an image with neither tmux nor a terminfo database (the Dockerfile's
# verify stage, or any image via `make check-pod-tmux`), with the bundle
# unpacked at PREFIX. Exits non-zero on the first failure.
set -eu

prefix=$1
want=${2:-}
tmux=$prefix/bin/tmux
unset TMUX TERMINFO TERMINFO_DIRS

fail() {
  echo "smoke: $*" >&2
  exit 1
}

got=$("$tmux" -V)
if [ -n "$want" ] && [ "$got" != "tmux $want" ]; then
  fail "'tmux -V' printed '$got', want 'tmux $want'"
fi

dir=$(mktemp -d)
inner=$dir/inner
outer=$dir/outer
cleanup() {
  "$tmux" -S "$outer" kill-server 2>/dev/null || true
  "$tmux" -S "$inner" kill-server 2>/dev/null || true
  rm -rf "$dir"
}
trap cleanup EXIT

# wait_for WHAT COMMAND...: poll COMMAND for up to 30s (QEMU is slow).
wait_for() {
  what=$1
  shift
  i=0
  until "$@" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt 300 ] || fail "timed out waiting for $what"
    sleep 0.1
  done
}

pane_has() {
  "$tmux" -S "$inner" capture-pane -p -t smoke | grep -q "$1"
}

client_on() {
  "$tmux" -S "$inner" list-clients -F '#{client_termname}' | grep -qx "$1"
}

pane_dead() {
  [ "$("$tmux" -S "$outer" display -p -t "outer:$1" '#{pane_dead}')" = 1 ]
}

# A detached server: ptys, libevent, input and output all work. The pane
# reads a line only after it says so: keys sent before a shell is up are lost.
"$tmux" -S "$inner" -f /dev/null new-session -d -s smoke -x 80 -y 24 \
  'echo ready; read -r line; echo "got:$line $TERM"; exec sleep 600'
wait_for "the pane to start" pane_has '^ready$'
"$tmux" -S "$inner" send-keys -t smoke 'fleet-smoke' Enter
wait_for "pane output" pane_has '^got:fleet-smoke tmux-256color$'

# A client attach loads the outer terminal's terminfo entry. Each client runs
# in a pane of a second server, which gives it a pty, with TERMINFO_DIRS unset
# so the entry must come from the directory compiled into the bundle.
"$tmux" -S "$outer" -f /dev/null new-session -d -s outer -x 100 -y 30 sh
"$tmux" -S "$outer" set -g remain-on-exit on
for term in xterm-256color tmux-256color alacritty; do
  "$tmux" -S "$outer" new-window -t outer -n "$term" "TERM=$term exec '$tmux' -S '$inner' attach -t smoke"
  wait_for "the $term client" client_on "$term"
  "$tmux" -S "$inner" detach-client -s smoke
done

# An entry outside the set is refused, so the checks above are meaningful.
"$tmux" -S "$outer" new-window -t outer -n unknown "TERM=fleet-no-such-term exec '$tmux' -S '$inner' attach -t smoke"
wait_for "the unknown-terminal client to exit" pane_dead unknown
if client_on fleet-no-such-term; then
  fail "a client attached with an unknown TERM"
fi

echo "smoke: ok ($got)"
