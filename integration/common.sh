#!/usr/bin/env bash
# Shared helpers for fleet-man integration tests.
#
# Each test file sources this, calls `setup_test`, runs assertions, and
# exits 0 on pass. `run.sh` invokes teardown between tests so state does
# not bleed across runs.

set -euo pipefail

# Colors (only when stdout is a tty)
if [ -t 1 ]; then
  C_RED='\033[0;31m'
  C_GREEN='\033[0;32m'
  C_YELLOW='\033[0;33m'
  C_RESET='\033[0m'
else
  C_RED=''
  C_GREEN=''
  C_YELLOW=''
  C_RESET=''
fi

# Paths resolved relative to this file's directory.
INTEGRATION_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${INTEGRATION_DIR}/.." && pwd)"
FIXTURE_SRC="${INTEGRATION_DIR}/fixture"

# Alternate fixture used by tests that need an image with curl + npm
# pre-installed (Claude Code installer + Codex npm package). Tests opt
# into it via setup_agent_test instead of setup_test.
FIXTURE_AGENT_SRC="${INTEGRATION_DIR}/fixture-agent"

# Alternate fixture whose devcontainer.json carries a populated
# customizations.fleet.fleetLaunch block (Links + Apps). Tests that
# exercise `fleet launch` opt into it via setup_launch_test so the
# in-instance launcher has something to list / resolve.
FIXTURE_LAUNCH_SRC="${INTEGRATION_DIR}/fixture-launch"

# Alternate fixture whose postCreate installs a fake `claude` under
# ~/.local/bin (on PATH only via .bashrc, mimicking the real install) that
# records its args to /tmp/automation_agent_out. The automation scheduler test
# opts into it via setup_automation_test to verify a fired trigger actually
# launches the agent with the prompt.
FIXTURE_AUTOMATION_SRC="${INTEGRATION_DIR}/fixture-automation"

# Alternate fixture whose postCreate adds a second container user `app` (with a
# home dir that sorts before /home/vscode). Tests opt into it via
# setup_twouser_test to prove the containerUser resolution picks the devcontainer
# remoteUser regardless of which user holds a tmux socket / sorts first.
FIXTURE_TWOUSER_SRC="${INTEGRATION_DIR}/fixture-twouser"

# Test scratch dir created fresh per test.
TEST_SCRATCH_DIR="${TEST_SCRATCH_DIR:-/tmp/fleet-man-itest}"

# The fleet binary under test. run.sh builds it and exports FLEET_BIN.
FLEET_BIN="${FLEET_BIN:-fleet}"

# The backend the suite provisions instances on: devcontainer (the default) or
# kubernetes (FLEET_ITEST_CLUSTER names the kubeconfig context). Tests reach
# backend state only through the neutral helpers below, which dispatch to
# integration/backends/<backend>.sh.
FLEET_ITEST_BACKEND="${FLEET_ITEST_BACKEND:-devcontainer}"
case "${FLEET_ITEST_BACKEND}" in
  devcontainer|kubernetes) ;;
  *)
    printf '[fatal] unknown FLEET_ITEST_BACKEND=%s (want devcontainer or kubernetes)\n' \
      "${FLEET_ITEST_BACKEND}" >&2
    exit 2
    ;;
esac
export FLEET_ITEST_BACKEND
# shellcheck disable=SC1090
source "${INTEGRATION_DIR}/backends/${FLEET_ITEST_BACKEND}.sh"

# Fleet name is derived from the repo URL basename. We clone the fixture
# into a dir named "itest-fleet" so resolve.go sees fleet "itest-fleet".
#
# FIXTURE_REPO_URL is a PLAIN PATH, which git clones like any other remote
# (local clone). It must not be a file:// URL: fleet reserves file:// for
# TEMPLATE fleets (the dir is copied per instance, never cloned, and the
# fleet name is not derivable). FIXTURE_TEMPLATE_URL is that form of the
# same fixture, for the template tests.
FIXTURE_REPO_NAME="itest-fleet"
FIXTURE_REPO_DIR="${TEST_SCRATCH_DIR}/${FIXTURE_REPO_NAME}"
FIXTURE_REPO_URL="${FIXTURE_REPO_DIR}"
FIXTURE_TEMPLATE_URL="file://${FIXTURE_REPO_DIR}"

# Default tmux session name used by TUI tests. Overridable per-test.
TUI_SESSION="${TUI_SESSION:-fleetman-itest}"
TUI_WIDTH="${TUI_WIDTH:-140}"
# 60 rows accommodates the settings page when all sections are visible
# (General, Dotfiles, Codespaces, Browser, Tool Status, Help) — combined
# they exceed 40 rows on a 140-col layout, which would scroll the top
# off the visible viewport and break tests that grep for first-section
# rows like "Tmux vim keys".
TUI_HEIGHT="${TUI_HEIGHT:-60}"

# === Logging / result emission ===
#
# Each test is responsible for emitting its OWN result row — not the
# runner. `itest_begin` is called at the top of every test: it records
# the start time and installs an EXIT trap so a row is always emitted,
# even if the script crashes or `set -e` trips.
#
# `pass` / `fail` both print and emit the row explicitly. The EXIT trap
# only emits if neither was reached (catch-all for unexpected bash exits).
# The row format written to ${FLEET_ITEST_RESULTS} is:
#   <name>|<pass|fail>|<duration_s>|<description>
# run.sh aggregates this file into the final table. No central list
# needs editing when a new test is added.

info()  { printf "%b[info]%b  %s\n" "${C_YELLOW}" "${C_RESET}" "$*"; }

pass()  {
  printf "%b[pass]%b  %s\n" "${C_GREEN}"  "${C_RESET}" "$*"
  _itest_emit pass
  exit 0
}

fail()  {
  printf "%b[fail]%b  %s\n" "${C_RED}" "${C_RESET}" "$*" >&2
  _itest_emit fail
  exit 1
}

# itest_cleanup is a hook tests can override to run cleanup (e.g. kill a
# tmux session) before the EXIT trap emits the result row. Default: no-op.
itest_cleanup() { :; }

# itest_begin: call once per test, right after sourcing common.sh and
# before the test body. Records metadata and installs the result-emit
# EXIT trap.
itest_begin() {
  TEST_NAME="$(basename "$0" .sh)"
  TEST_DESCRIPTION="$(grep -m1 -E '^# Description:' "$0" 2>/dev/null \
    | sed -E 's/^# Description: *//')"
  if [ -z "${TEST_DESCRIPTION}" ]; then
    TEST_DESCRIPTION="(no description)"
  fi
  TEST_START="$(date +%s)"
  TEST_EMITTED=""
  trap _itest_exit_handler EXIT
}

# Internal: runs on EXIT. Calls itest_cleanup, then emits a row if the
# test did not already do so via pass/fail.
_itest_exit_handler() {
  local rc=$?
  itest_cleanup || true
  if [ -z "${TEST_EMITTED:-}" ]; then
    local status="fail"
    [ "${rc}" -eq 0 ] && status="pass"
    _itest_emit "${status}"
  fi
  # Preserve the original exit code.
  exit "${rc}"
}

# Internal: append one row to the shared results file. Safe to call
# outside a runner (just becomes a no-op when FLEET_ITEST_RESULTS is unset).
_itest_emit() {
  TEST_EMITTED=1
  local status="$1"
  local dur=0
  if [ -n "${TEST_START:-}" ]; then
    dur=$(( $(date +%s) - TEST_START ))
  fi
  if [ -n "${FLEET_ITEST_RESULTS:-}" ]; then
    printf '%s|%s|%s|%s\n' \
      "${TEST_NAME:-unknown}" \
      "${status}" \
      "${dur}" \
      "${TEST_DESCRIPTION:-(no description)}" \
      >> "${FLEET_ITEST_RESULTS}"
  fi
}

# === Assertions ===

# assert_equals <expected> <actual> <message>
assert_equals() {
  local expected="$1"; local actual="$2"; local msg="${3:-values not equal}"
  if [ "$expected" != "$actual" ]; then
    fail "${msg}: expected=[${expected}] actual=[${actual}]"
  fi
}

# assert_contains <haystack> <needle> <message>
assert_contains() {
  local haystack="$1"; local needle="$2"; local msg="${3:-substring not found}"
  if ! printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
    fail "${msg}: needle=[${needle}] haystack=[${haystack}]"
  fi
}

# assert_not_contains <haystack> <needle> <message>
assert_not_contains() {
  local haystack="$1"; local needle="$2"; local msg="${3:-substring unexpectedly found}"
  if printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
    fail "${msg}: needle=[${needle}] haystack=[${haystack}]"
  fi
}

# assert_file_exists <path>
assert_file_exists() {
  [ -e "$1" ] || fail "expected path to exist: $1"
}

# assert_file_absent <path>
assert_file_absent() {
  [ ! -e "$1" ] || fail "expected path to be absent: $1"
}

# === Setup / teardown ===

# setup_test prepares a clean environment:
#   1. removes ~/.fleet
#   2. recreates the fixture git repo at $FIXTURE_REPO_DIR
#   3. runs the backend's setup (nothing for devcontainer)
# Called at the top of each test.
setup_test() {
  # 1. Wipe fleet state. Leave an empty ~/.fleet behind — its presence
  #    suppresses the first-launch deps-check splash, which is not what
  #    these tests are trying to exercise and would otherwise block key
  #    input to the fleet list page.
  rm -rf "${HOME}/.fleet"
  mkdir -p "${HOME}/.fleet"

  # 2. Re-create the fixture repo from the committed fixture template.
  rm -rf "${TEST_SCRATCH_DIR}"
  mkdir -p "${FIXTURE_REPO_DIR}"
  cp -r "${FIXTURE_SRC}/." "${FIXTURE_REPO_DIR}/"

  (
    cd "${FIXTURE_REPO_DIR}"
    git init -q -b main
    git config user.email "itest@fleet-man.local"
    git config user.name  "Fleet Integration Test"
    git add -A
    git commit -q -m "fixture: initial commit"
  )

  # 3. Backend setup, after the wipe so it sees the fresh ~/.fleet.
  _be_setup
}

# setup_agent_test prepares a clean environment for tests that need an
# image with curl + npm available (the Claude / Codex install tests).
# Identical to setup_test but uses the agent fixture instead of the
# default debian-base fixture so the install scripts have the tools
# they need.
setup_agent_test() {
  FIXTURE_SRC="${FIXTURE_AGENT_SRC}" setup_test
}

# setup_automation_test prepares a clean environment whose fixture installs a
# fake `claude` (see FIXTURE_AUTOMATION_SRC) so the automation scheduler test can
# verify an agent actually launches with its prompt.
setup_automation_test() {
  FIXTURE_SRC="${FIXTURE_AUTOMATION_SRC}" setup_test
}

# setup_twouser_test prepares a clean environment whose fixture provisions a
# second container user (`app`) alongside the vscode remoteUser, so a test can
# verify fleet's session listing resolves the remoteUser even when another user
# holds a tmux socket. Identical to setup_test but uses the two-user fixture.
setup_twouser_test() {
  FIXTURE_SRC="${FIXTURE_TWOUSER_SRC}" setup_test
}

# setup_launch_test prepares a clean environment whose fixture
# devcontainer.json declares a customizations.fleet.fleetLaunch block,
# so the in-instance `fleet launch` subcommands have Links and Apps to
# list / resolve. Identical to setup_test but uses the launch fixture.
setup_launch_test() {
  FIXTURE_SRC="${FIXTURE_LAUNCH_SRC}" setup_test
}

# seed_fleet_settings writes ~/.fleet/state.json with a fleet record
# that already has its FleetSettings populated, so the very first
# `fleet up` call against the fleet picks them up. Settings are
# normally edited via the TUI; pre-seeding lets non-TUI tests exercise
# the settings-driven code paths without driving the TUI.
#
# Usage:
#   seed_fleet_settings <fleet_name> <claude:true|false> <codex:true|false> <homedir> [<gh:true|false>] [<buildkit:true|false>] [<custom_mounts>] [<auggie:true|false>]
#
# gh, buildkit, and auggie default to false so older callers that pre-date
# those settings keep working unchanged.
seed_fleet_settings() {
  local fleet_name="$1"
  local claude="${2:-false}"
  local codex="${3:-false}"
  local homedir="${4:-/home/node}"
  local gh="${5:-false}"
  local buildkit="${6:-false}"
  # Optional 7th arg: a JSON array literal of custom mount container paths,
  # e.g. '["/opt/data","/var/cache/shared"]'. Empty means no customMounts key.
  local custom_mounts="${7:-}"
  local auggie="${8:-false}"
  local custom_mounts_line=""
  if [[ -n "${custom_mounts}" ]]; then
    custom_mounts_line=$'\n        "customMounts": '"${custom_mounts}"','
  fi
  mkdir -p "${HOME}/.fleet"
  cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "${fleet_name}": {
      "name": "${fleet_name}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": {${custom_mounts_line}
        "claudeCodeMount": ${claude},
        "codexMount": ${codex},
        "ghMount": ${gh},
        "auggieMount": ${auggie},
        "buildkitServer": ${buildkit},
        "homeDir": "${homedir}"
      },
      "instances": []
    }
  }
}
EOF
}

# install_stub_agent <marker_path>
#
# Drop a fake `claude` binary onto PATH ahead of any real one and have
# it record its invocation to <marker_path>. Used by the new-fleet
# devcontainer-check tests so the Setup branch behaves identically
# regardless of whether the runner happens to have a real claude / codex
# install on PATH.
#
# The stub exits 0 immediately so bubbletea regains the terminal after
# tea.ExecProcess hands off — without a real agent the test would
# otherwise hang waiting for an interactive prompt.
#
# Side-effects:
#   - Writes the stub at $TEST_SCRATCH_DIR/stubs/claude
#   - Prepends that dir to PATH for the current shell. tmux inherits the
#     environment, so a subsequent tui_spawn will see the stub first.
install_stub_agent() {
  local marker="$1"
  local stub_dir="${TEST_SCRATCH_DIR}/stubs"
  mkdir -p "${stub_dir}"
  cat > "${stub_dir}/claude" <<EOF
#!/usr/bin/env bash
# Stub agent installed by install_stub_agent — used by integration tests
# to assert the Setup branch of the new-fleet devcontainer dialog
# actually launches an agent. Records its argv to the marker file and
# exits cleanly so the TUI repaints.
printf '%s\n' "\$*" > "${marker}"
exit 0
EOF
  chmod +x "${stub_dir}/claude"
  export PATH="${stub_dir}:${PATH}"
}

# teardown_test best-effort cleans up any containers spawned by a test.
# It tears down every fleet in state, then wipes state files. Called by
# run.sh after each test whether it passed or failed.
teardown_test() {
  # Kill any TUI tmux session left over from a TUI test. Safe if there
  # is none. Done first so nothing else is holding the state file open.
  if command -v tmux >/dev/null 2>&1; then
    tmux kill-session -t "${TUI_SESSION}" >/dev/null 2>&1 || true
  fi

  # Tear down instances. Best effort — a test that never reached
  # "up" will have no fleets; a test that left state in a weird place
  # is recovered by the backend's fallback sweep below.
  if [ -f "${HOME}/.fleet/state.json" ]; then
    # Iterate fleet names out of state.json without jq (not guaranteed on
    # the runner). Grep the top-level keys under "fleets".
    local fleets
    fleets=$(grep -oE '"[a-zA-Z0-9._-]+":\s*\{' "${HOME}/.fleet/state.json" 2>/dev/null \
      | grep -v '"fleets"' \
      | sed -E 's/"([^"]+)".*/\1/' \
      | sort -u || true)
    for f in ${fleets}; do
      "${FLEET_BIN}" destroy "${f}" >/dev/null 2>&1 || true
    done
  fi

  # Fallback: the backend removes whatever a crashed test left behind
  # (devcontainer: labelled containers and shared buildkit servers, and the
  # ownership of a root-written buildkit cache; kubernetes: the connection's
  # labelled objects and the test's daemons).
  _be_teardown || true

  rm -rf "${HOME}/.fleet"
  rm -rf "${TEST_SCRATCH_DIR}"
}

# fleet_up spawns an instance against the fixture repo. Uses --repo so
# the test does not depend on cwd being a git repo. Streams output to
# the caller's stdout/stderr.
fleet_up() {
  local name="$1"
  "${FLEET_BIN}" up "${name}" --repo "${FIXTURE_REPO_URL}"
}

# === Backend-neutral instance helpers ===
#
# Tests check an instance's container, workspace, control dir and shared
# mounts through these, never through docker (or kubectl) directly, so one
# test runs on every FLEET_ITEST_BACKEND. Each dispatches to its _be_* twin in
# integration/backends/<backend>.sh; on kubernetes the share sync still fails
# the test as "not wired yet", naming the unit that wires it.
#
# <ref> is <fleet>/<instance>, as `fleet exec` takes it; a bare <instance> is
# the fixture fleet's. Containers are named by the container_id fleet recorded
# for the instance (itest_container_id); on kubernetes that id has the
# k8s:<cluster>/<namespace>/<statefulset>@g<generation> form.

# itest_backend_is <backend> — true when the suite runs on <backend>. Only for
# a step that has no counterpart on other backends (a host bind mount, say);
# anything that does goes through a helper.
itest_backend_is() { [ "${FLEET_ITEST_BACKEND}" = "$1" ]; }

# itest_config_written — a test that writes ~/.fleet/config.json itself
# (after setup_test) calls this next, so the config takes effect as it would
# on a fresh ~/.fleet: creates still land on the backend under test, and the
# daemon the next command talks to starts with the config as written
# (kubernetes: the run's connection stays the remembered target, and the
# daemon setup started is stopped; devcontainer: nothing to do, setup starts
# no daemon).
itest_config_written() { _be_config_written; }

# itest_backend_rebuilds — true when the backend under test can rebuild an
# instance in place (devcontainer; kubernetes from P6.1, which turns its
# _be_rebuilds on). A test's rebuild steps go inside it.
itest_backend_rebuilds() { _be_rebuilds; }

# itest_row_target — what a TUI instance row shows beside its branch for an
# instance on the backend under test (devcontainer: ⬡; kubernetes: ☸ and the
# run's connection's name).
itest_row_target() { _be_row_target; }

# _itest_ref <ref> — <ref> as <fleet>/<instance>.
_itest_ref() {
  case "$1" in
    */*) printf '%s\n' "$1" ;;
    *) printf '%s/%s\n' "${FIXTURE_REPO_NAME}" "$1" ;;
  esac
}

# --- state.json: the same on every backend ---

# itest_container_id [<ref>] — the container_id fleet recorded for <ref>, or
# with no argument for the first instance that has one. Prints nothing when
# there is none.
itest_container_id() {
  local ref=""
  [ -z "${1:-}" ] || ref=$(_itest_ref "$1")
  _itest_state_container_ids "${ref}" first
}

# itest_container_ids — every recorded container_id, one per line.
itest_container_ids() { _itest_state_container_ids "" all; }

# _itest_state_container_ids <ref|""> <first|all>. perl's core JSON::PP rather
# than jq, which the runner is not guaranteed to have.
_itest_state_container_ids() {
  perl -MJSON::PP -e '
    my ($path, $ref, $mode) = @ARGV;
    open(my $fh, "<", $path) or exit 0;
    my $text = do { local $/; <$fh> };
    my $st = eval { decode_json($text) } or exit 0;
    my ($want_fleet, $want_inst) = $ref =~ m{^([^/]+)/(.+)$} ? ($1, $2) : ("", "");
    my $fleets = $st->{fleets} || {};
    for my $f (sort keys %$fleets) {
      next if $want_fleet ne "" && $f ne $want_fleet;
      for my $i (@{ $fleets->{$f}{instances} || [] }) {
        next if $want_inst ne "" && ($i->{name} // "") ne $want_inst;
        my $id = $i->{container_id} // "";
        next if $id eq "" && $want_inst eq "";
        print "$id\n" if $id ne "";
        exit 0 if $mode eq "first";
      }
    }
  ' "${HOME}/.fleet/state.json" "$1" "$2"
}

# --- daemon-host paths: the same on every backend ---

# itest_host_workspace_dir <ref> — where `fleet up` clones the instance's repo
# on the daemon host: ~/.fleet/workspaces/<fleet>/<instance>/<fleet>. Every
# backend clones there while creating, but only devcontainer keeps working in
# it (bind-mounted), so checks on an instance's files go through the
# itest_workspace_* helpers below.
itest_host_workspace_dir() {
  local ref
  ref=$(_itest_ref "$1")
  printf '%s\n' "${HOME}/.fleet/workspaces/${ref%%/*}/${ref#*/}/${ref%%/*}"
}

# itest_host_control_dir <ref> — the instance's control dir on the daemon host,
# which devcontainer bind-mounts at /fleet-mounts/control. Clusters keep it in
# the pod; itest_control_* reach it on every backend.
itest_host_control_dir() {
  local ref
  ref=$(_itest_ref "$1")
  printf '%s\n' "${HOME}/.fleet/workspaces/${ref%%/*}/${ref#*/}/.control"
}

# itest_share_dir <fleet> [<subpath>] — the fleet's shared-mount tree on the
# daemon host (~/.fleet/workspaces/<fleet>: .claude, .codex, .config/gh,
# .augment, files/, .mnt/<path>). It is the canonical copy on every backend;
# call itest_share_sync between a write on one side and a read on the other.
itest_share_dir() {
  printf '%s\n' "${HOME}/.fleet/workspaces/$1${2:+/$2}"
}

# --- containers ---

# itest_container_status <container_id> — "running" while it runs, "absent"
# once it is gone, otherwise the backend's word for a stopped one (docker:
# exited, created, ...).
itest_container_status() { _be_container_status "$1"; }

# itest_container_exists <container_id>
itest_container_exists() { _be_container_exists "$1"; }

# assert_container_running <container_id> [message]
assert_container_running() {
  local status
  status=$(itest_container_status "$1")
  [ "${status}" = "running" ] || fail "${2:-container should be running}: $1 is ${status}"
}

# assert_container_stopped <container_id> [message] — it exists and is not running.
assert_container_stopped() {
  local status
  status=$(itest_container_status "$1")
  case "${status}" in
    running|absent) fail "${2:-container should be stopped}: $1 is ${status}" ;;
  esac
}

# assert_container_absent <container_id> [message]
assert_container_absent() {
  if itest_container_exists "$1"; then
    fail "${2:-container should be gone}: $1 still exists"
  fi
}

# itest_container_belongs_to <container_id> <ref> — the container carries the
# identity fleet finds <ref>'s container by (devcontainer: the
# devcontainer.local_folder label naming its workspace). Explains a mismatch
# on stderr.
itest_container_belongs_to() { _be_container_belongs_to "$1" "$(_itest_ref "$2")"; }

# itest_container_leftovers <ref> — ids of containers still labelled for <ref>,
# one per line; empty once nothing is left.
itest_container_leftovers() { _be_container_leftovers "$(_itest_ref "$1")"; }

# itest_container_logs <container_id> — the container's own output log, what
# `fleet logs` should print.
itest_container_logs() { _be_container_logs "$1"; }

# itest_container_stop <container_id> — stop it behind fleet's back.
itest_container_stop() { _be_container_stop "$1"; }

# itest_container_exec_as <container_id> <user> <cmd> [args...] — run a command
# in the container as <user>, bypassing fleet.
itest_container_exec_as() { _be_container_exec_as "$@"; }

# itest_container_label <container_id> <label> — the value of a label the
# devcontainer CLI stamped on the container (e.g. devcontainer.metadata, the
# merged config an editor's attach applies); empty when it has none.
itest_container_label() { _be_container_label "$1" "$2"; }

# --- workspace: the instance's checkout ---

# itest_workspace_exists <ref>
itest_workspace_exists() { _be_workspace_exists "$(_itest_ref "$1")"; }

# assert_workspace_exists <ref> [message]
assert_workspace_exists() {
  itest_workspace_exists "$1" || fail "${2:-expected the workspace to exist}: $1"
}

# assert_workspace_absent <ref> [message]
assert_workspace_absent() {
  if itest_workspace_exists "$1"; then
    fail "${2:-expected the workspace to be gone}: $1"
  fi
}

# itest_workspace_test <ref> <test-op> <relpath> — test(1) on a path in the
# workspace, e.g. `itest_workspace_test "${inst}" -f README.md`.
itest_workspace_test() { _be_workspace_test "$(_itest_ref "$1")" "$2" "$3"; }

# itest_workspace_read <ref> <relpath> — print a workspace file.
itest_workspace_read() { _be_workspace_read "$(_itest_ref "$1")" "$2"; }

# itest_workspace_write <ref> <relpath> <content> — write <content> plus a
# newline to a workspace file.
itest_workspace_write() { _be_workspace_write "$(_itest_ref "$1")" "$2" "$3"; }

# itest_workspace_git <ref> <git-args...> — run git in the workspace checkout.
itest_workspace_git() {
  local ref
  ref=$(_itest_ref "$1")
  shift
  _be_workspace_git "${ref}" "$@"
}

# --- control dir: /fleet-mounts/control as the instance sees it ---

# itest_control_test <ref> <test-op> <name> — test(1) on <name> in the
# instance's control dir ("." is the dir itself), e.g. -S fleet.sock.
itest_control_test() { _be_control_test "$(_itest_ref "$1")" "$2" "$3"; }

# itest_control_read <ref> <name> — print a file from the control dir.
itest_control_read() { _be_control_read "$(_itest_ref "$1")" "$2"; }

# itest_control_mode <ref> <name> — octal permission bits of <name>.
itest_control_mode() { _be_control_mode "$(_itest_ref "$1")" "$2"; }

# --- shared fleet mounts ---

# itest_share_sync <fleet> — wait until the fleet's instances and its host
# share tree (itest_share_dir) agree. Instant on devcontainer, a bind mount.
itest_share_sync() { _be_share_sync "$1"; }

# --- instance root filesystem: reached through `fleet exec` on every backend ---

# itest_install_local_deb <ref> <package> — build a one-file .deb named
# <package> inside the instance and apt-install it from that file, so the
# instance's dpkg database and /usr gain a package exactly as `apt install`
# leaves them, with no network or mirror involved. The package's only file is
# /usr/bin/<package>, which prints "<package>-ok". Fails the test, with apt's
# output, when the install fails.
itest_install_local_deb() {
  local ref out
  ref=$(_itest_ref "$1")
  if ! out=$("${FLEET_BIN}" exec "${ref}" -- sh -c '
      set -e
      umask 022
      pkg="$1"
      d=$(mktemp -d)
      mkdir -p "$d/root/DEBIAN" "$d/root/usr/bin"
      printf "Package: %s\nVersion: 1.0\nArchitecture: all\nMaintainer: fleet itest <itest@fleet-man.local>\nDescription: fleet integration test package\n" \
        "$pkg" > "$d/root/DEBIAN/control"
      printf "#!/bin/sh\necho %s-ok\n" "$pkg" > "$d/root/usr/bin/$pkg"
      chmod 0755 "$d/root/usr/bin/$pkg"
      dpkg-deb --root-owner-group --build "$d/root" "$d/$pkg.deb"
      chmod 0755 "$d"  # apt reads the .deb as its _apt sandbox user
      sudo apt-get install -y -qq "$d/$pkg.deb"
      rm -rf "$d"
    ' sh "$2" 2>&1); then
    fail "installing the local package $2 in ${ref} failed: ${out}"
  fi
}

# itest_local_deb_status <ref> <package> — "installed" when dpkg lists the
# package as installed and its binary runs, otherwise what was found instead.
itest_local_deb_status() {
  local ref
  ref=$(_itest_ref "$1")
  "${FLEET_BIN}" exec "${ref}" -- sh -c '
      st=$(dpkg-query -W -f="\${Status}" "$1" 2>/dev/null) || st="not in the dpkg database"
      [ "$st" = "install ok installed" ] || { echo "$st"; exit 0; }
      [ "$("$1" 2>/dev/null)" = "$1-ok" ] || { echo "installed, but /usr/bin/$1 does not run"; exit 0; }
      echo installed
    ' sh "$2" 2>&1 || true
}

# --- poison docker: a docker stand-in on the PATH of the daemon under test ---
#
# itest_poison_docker <record|poison> puts integration/poison-docker/docker
# ahead of the real docker on PATH, so every docker call the daemon (and the
# devcontainer CLI it runs) makes goes through it, and logs each call (see
# itest_poison_docker_log). record passes every call through to the real
# docker. poison refuses the container verbs (run, exec, start, stop, inspect
# of a container, ps, logs, stats, ...; see the shim for the list) with exit
# code 125 and passes the rest (build, image, network, version, ...) through,
# so a backend that must never treat its instances as docker containers fails
# loudly the moment it does.
#
# The daemon inherits PATH from the client that spawns it, so this stops any
# daemon of the binary under test first; call it after setup_test and before
# the first fleet command. A test that calls it must stop the daemon in its
# itest_cleanup, so the shim does not outlive the test.
itest_poison_docker() {
  case "$1" in
    record|poison) ;;
    *) fail "itest_poison_docker: unknown mode '$1' (want record or poison)" ;;
  esac
  local shim_dir="${INTEGRATION_DIR}/poison-docker" real
  # The real docker: the first one on PATH that is not the shim itself.
  real=$(
    PATH=$(printf '%s' "${PATH}" | tr ':' '\n' | grep -vxF "${shim_dir}" | paste -sd: -)
    command -v docker
  ) || fail "itest_poison_docker: no real docker on PATH"
  export FLEET_ITEST_DOCKER_MODE="$1"
  export FLEET_ITEST_REAL_DOCKER="${real}"
  export FLEET_ITEST_DOCKER_LOG="${TEST_SCRATCH_DIR}/poison-docker.log"
  : > "${FLEET_ITEST_DOCKER_LOG}"
  case ":${PATH}:" in
    *":${shim_dir}:"*) ;;
    *) export PATH="${shim_dir}:${PATH}" ;;
  esac
  _itest_poison_docker_stop_daemons
}

# itest_poison_docker_log — every docker call the shim saw since
# itest_poison_docker, one per line: "<pass|refused> <args...>".
itest_poison_docker_log() { cat "${FLEET_ITEST_DOCKER_LOG}" 2>/dev/null || true; }

# itest_poison_docker_saw <ERE> — true when a logged call matches <ERE>, e.g.
# '^refused exec '. Reads the log file directly: the log gets long, and a
# grep -q at the end of a pipe would trip pipefail with SIGPIPE.
itest_poison_docker_saw() { grep -qE -- "$1" "${FLEET_ITEST_DOCKER_LOG}" 2>/dev/null; }

# _itest_poison_docker_stop_daemons — stop every daemon of the binary under
# test and wait until they are gone, so the next fleet command cold-spawns one
# with the shim on its PATH.
_itest_poison_docker_stop_daemons() {
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  local deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
  while [ "$(server_count)" != "0" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "itest_poison_docker: a daemon of ${FLEET_BIN} would not exit"
    sleep 0.2
  done
}

# === MCP ===

# itest_mcp_call <tool> <arguments-json> [<field>] — call one tool on the
# daemon's local MCP server (streamable HTTP on ~/.fleet/mcp.port, bearer token
# ~/.fleet/mcp.token): initialize, initialized, tools/call, then close the
# session. Prints the tool result as JSON ({content, structuredContent,
# isError}), or with <field> just that value (see itest_json_field). A tool
# error is a result with isError true, not a failure. Retries while a fresh
# daemon brings its listener up; returns 1 when it never answers or the call is
# a JSON-RPC error.
itest_mcp_call() {
  local tool="$1" args="$2" field="${3:-}"
  local hdr port token url sid body="" result rc
  hdr=$(mktemp)
  for _ in $(seq 1 20); do
    port=$(cat "${HOME}/.fleet/mcp.port" 2>/dev/null || true)
    token=$(cat "${HOME}/.fleet/mcp.token" 2>/dev/null || true)
    url="http://127.0.0.1:${port}/"
    if [ -n "${port}" ] && [ -n "${token}" ] && curl -sf -D "${hdr}" -o /dev/null -X POST "${url}" \
        -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' \
        -H 'Accept: application/json, text/event-stream' \
        -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"itest","version":"0"}}}'; then
      sid=$(grep -i '^mcp-session-id:' "${hdr}" | head -n 1 | cut -d: -f2- | tr -d ' \r')
      curl -sf -o /dev/null -X POST "${url}" \
        -H "Authorization: Bearer ${token}" -H "Mcp-Session-Id: ${sid}" \
        -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
        -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' || true
      body=$(curl -sf -X POST "${url}" \
        -H "Authorization: Bearer ${token}" -H "Mcp-Session-Id: ${sid}" \
        -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
        -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"${tool}\",\"arguments\":${args}}}" || true)
      curl -sf -o /dev/null -X DELETE "${url}" \
        -H "Authorization: Bearer ${token}" -H "Mcp-Session-Id: ${sid}" || true
      rc=0
      result=$(printf '%s' "${body}" | _itest_mcp_result) || rc=$?
      if [ "${rc}" -eq 0 ]; then
        rm -f "${hdr}"
        if [ -n "${field}" ]; then
          printf '%s' "${result}" | itest_json_field "${field}"
        else
          printf '%s' "${result}"
        fi
        return 0
      fi
      # A JSON-RPC error is the server's answer: retrying would not change it.
      if [ "${rc}" -eq 2 ]; then
        break
      fi
    fi
    sleep 0.5
  done
  rm -f "${hdr}"
  printf 'MCP %s failed (port=%s): %s\n' "${tool}" "${port:-none}" "${body}" >&2
  return 1
}

# _itest_mcp_result — the result object of the JSON-RPC reply with id 2 on
# stdin (a plain JSON body or an SSE stream of data: lines), as JSON. Exits 1
# when there is no such reply, 2 when it is a JSON-RPC error.
_itest_mcp_result() {
  perl -MJSON::PP -e '
    my $body = do { local $/; <STDIN> };
    my @msgs = map { /^data:\s?(.*)$/ ? ($1) : () } split /\r?\n/, $body;
    @msgs = ($body) unless @msgs;
    my $json = JSON::PP->new->utf8->canonical;
    for my $m (@msgs) {
      my $reply = eval { $json->decode($m) } or next;
      next unless ref $reply eq "HASH" && defined $reply->{id} && $reply->{id} eq "2";
      exit 2 unless ref $reply->{result} eq "HASH";
      print $json->encode($reply->{result});
      exit 0;
    }
    exit 1;
  '
}

# itest_json_field <path> — the value at a dotted <path> in the JSON document on
# stdin (structuredContent.sessions.0.session; numeric segments index arrays):
# strings raw, true/false for booleans, objects and arrays as compact JSON with
# sorted keys, nothing when the path does not resolve. Fails on invalid JSON.
itest_json_field() {
  perl -MJSON::PP -e '
    my ($path) = @ARGV;
    my $json = JSON::PP->new->utf8->canonical;
    my $v = $json->decode(do { local $/; <STDIN> });
    for my $k (split /\./, $path) {
      if (ref $v eq "HASH") { $v = $v->{$k} }
      elsif (ref $v eq "ARRAY" && $k =~ /^\d+$/) { $v = $v->[$k] }
      else { undef $v }
      exit 0 unless defined $v;
    }
    if (JSON::PP::is_bool($v)) { print $v ? "true" : "false" }
    elsif (ref $v) { print $json->encode($v) }
    else { binmode STDOUT, ":encoding(UTF-8)"; print $v }
  ' "$1"
}

# === TUI helpers ===
#
# The fleet TUI is a bubbletea app. Tests drive it the same way a human
# would: launch `fleet` inside a detached tmux session of fixed size,
# then send keys + capture the rendered screen.

# _scale_timeout <seconds>
# Multiply a wait-budget by FLEET_TUI_WAIT_SCALE (default 1, integer).
# Slow runners (e.g. WSL on a Windows host) export a higher value so the
# polling helpers below give the TUI/dockerd enough headroom without us
# editing every test.
_scale_timeout() {
  local raw="$1"
  local scale="${FLEET_TUI_WAIT_SCALE:-1}"
  printf '%s' "$(( raw * scale ))"
}

# tui_spawn [session] [args...]
# Start the TUI inside a detached tmux session. Any extra arguments are
# passed through to the fleet binary. The TMUX env var is implicitly set
# by running inside tmux, which the TUI uses to enable split pane mode.
tui_spawn() {
  local session="${1:-${TUI_SESSION}}"
  shift || true
  tmux kill-session -t "${session}" >/dev/null 2>&1 || true

  # Force TERM to something tmux+lipgloss render predictably on CI.
  tmux new-session -d -s "${session}" -x "${TUI_WIDTH}" -y "${TUI_HEIGHT}" \
    "env TERM=xterm-256color ${FLEET_BIN} $*"
}

# tui_send_text <text> [session]
# Type a string literally into the TUI (safe for URLs / names that
# contain tmux-reserved key words).
tui_send_text() {
  local text="$1"
  local session="${2:-${TUI_SESSION}}"
  tmux send-keys -t "${session}" -l "${text}"
}

# tui_send_pane <pane_index> <key> [session]
# Send a key to a specific pane by its visible index (0 = TUI, 1 = first
# split, 2 = second split, ...). Needed once a split pane is open and
# we want to talk to the shell rather than the TUI.
tui_send_pane() {
  local pane="$1"; local key="$2"; local session="${3:-${TUI_SESSION}}"
  tmux send-keys -t "${session}:.${pane}" "${key}"
}

# tui_send_pane_text <pane_index> <text> [session]
tui_send_pane_text() {
  local pane="$1"; local text="$2"; local session="${3:-${TUI_SESSION}}"
  tmux send-keys -t "${session}:.${pane}" -l "${text}"
}

# tui_capture_pane <pane_index> [session]
# Capture the visible contents of a specific pane, scrolled from the
# history so we see output that has already scrolled off-screen.
tui_capture_pane() {
  local pane="$1"; local session="${2:-${TUI_SESSION}}"
  tmux capture-pane -t "${session}:.${pane}" -p -S -200
}

# tui_pane_count [session] — number of panes in the active window.
tui_pane_count() {
  local session="${1:-${TUI_SESSION}}"
  tmux list-panes -t "${session}" 2>/dev/null | wc -l
}

# tui_wait_for_pane <expected_count> [timeout_s] [session]
# Wait until the tmux window has `expected_count` panes (e.g. 2 after a
# split opens, 1 after it's closed).
tui_wait_for_pane() {
  local expected="$1"; local timeout
  timeout=$(_scale_timeout "${2:-10}")
  local session="${3:-${TUI_SESSION}}"
  local deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    [ "$(tui_pane_count "${session}")" -eq "${expected}" ] && return 0
    sleep 0.25
  done
  printf '%b[fail]%b tui_wait_for_pane timed out — expected %s panes, have %s\n' \
    "${C_RED}" "${C_RESET}" "${expected}" "$(tui_pane_count "${session}")" >&2
  return 1
}

# tui_wait_for_in_pane <pane_index> <needle> [timeout_s] [session]
# Poll a specific pane's capture for a substring.
tui_wait_for_in_pane() {
  local pane="$1"; local needle="$2"; local timeout
  timeout=$(_scale_timeout "${3:-10}")
  local session="${4:-${TUI_SESSION}}"
  local deadline=$(( $(date +%s) + timeout ))
  local screen=""
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    screen=$(tui_capture_pane "${pane}" "${session}" || true)
    if printf '%s' "${screen}" | grep -qF -- "${needle}"; then
      return 0
    fi
    sleep 0.25
  done
  printf '%b[fail]%b timed out after %ss waiting in pane %s for: %s\n' \
    "${C_RED}" "${C_RESET}" "${timeout}" "${pane}" "${needle}" >&2
  printf -- '--- pane %s ---\n%s\n--- end ---\n' "${pane}" "${screen}" >&2
  return 1
}

# tui_capture [session]
# Print the current visible screen of the tmux session to stdout.
tui_capture() {
  local session="${1:-${TUI_SESSION}}"
  tmux capture-pane -t "${session}" -p
}

# tui_send <key> [session]
# Send a single key / chord to the session. Use tmux key names: j, k,
# Enter, Escape, Space, C-c, etc.
tui_send() {
  local key="$1"
  local session="${2:-${TUI_SESSION}}"
  tmux send-keys -t "${session}" "${key}"
}

# tui_wait_for <needle> [timeout_s] [session]
# Poll capture-pane for up to timeout seconds waiting for `needle` (plain
# substring) to appear. Default timeout 10s. Returns 0 on match, 1 on
# timeout. On timeout also dumps the final screen to stderr.
tui_wait_for() {
  local needle="$1"
  local timeout
  timeout=$(_scale_timeout "${2:-10}")
  local session="${3:-${TUI_SESSION}}"
  local deadline=$(( $(date +%s) + timeout ))
  local screen=""
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    screen=$(tui_capture "${session}" || true)
    if printf '%s' "${screen}" | grep -qF -- "${needle}"; then
      return 0
    fi
    sleep 0.25
  done
  printf '%b[fail]%b tui_wait_for timed out after %ss waiting for: %s\n' \
    "${C_RED}" "${C_RESET}" "${timeout}" "${needle}" >&2
  printf -- '--- final screen ---\n%s\n--- end screen ---\n' "${screen}" >&2
  return 1
}

# tui_wait_for_absent <needle> [timeout_s] [session]
# Inverse of tui_wait_for — poll until the needle disappears from screen.
tui_wait_for_absent() {
  local needle="$1"
  local timeout
  timeout=$(_scale_timeout "${2:-10}")
  local session="${3:-${TUI_SESSION}}"
  local deadline=$(( $(date +%s) + timeout ))
  local screen=""
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    screen=$(tui_capture "${session}" || true)
    if ! printf '%s' "${screen}" | grep -qF -- "${needle}"; then
      return 0
    fi
    sleep 0.25
  done
  printf '%b[fail]%b tui_wait_for_absent timed out after %ss waiting for: %s\n' \
    "${C_RED}" "${C_RESET}" "${timeout}" "${needle}" >&2
  printf -- '--- final screen ---\n%s\n--- end screen ---\n' "${screen}" >&2
  return 1
}

# tui_delete_fleet_cache <row label> <done message>
# From the fleet list with the cursor on a fleet's header, press the
# [ Delete cache ] button of one cache in the fleet options dialog (e →
# Caching → the row named <row label>, e.g. "Deb package cache"), confirm it,
# wait for <done message> (e.g. "Deb cache cleared") and close the dialog. Finds
# rows by the "> " cursor rather than by counting keypresses, so new rows above
# them do not break it.
tui_delete_fleet_cache() {
  local label="$1" done_msg="$2"
  tui_send e
  tui_wait_for "Edit fleet" 5 || fail "the fleet options dialog did not open"
  for _ in $(seq 1 30); do
    tui_capture | grep -qF "> ▶ Caching" && break
    tui_capture | grep -qF "> ▼ Caching" && break
    tui_send j
    sleep 0.1
  done
  if ! tui_capture | grep -qF "> ▼ Caching"; then
    tui_send l
    tui_wait_for "${label}" 5 || fail "the Caching section did not expand"
  fi
  for _ in $(seq 1 10); do
    tui_capture | grep -qF ">   [x] ${label}" && break
    tui_send j
    sleep 0.1
  done
  tui_capture | grep -qF ">   [x] ${label}" \
    || { tui_capture >&2; fail "could not put the cursor on the enabled '${label}' row"; }
  tui_send l
  tui_send Enter
  tui_wait_for "Delete cache?" 5 || fail "the Delete cache button did not ask to confirm"
  tui_send Enter
  tui_wait_for "${done_msg}" 120 || fail "the cache delete never reported '${done_msg}'"
  tui_send Escape
  tui_wait_for_absent "Edit fleet" 5 || fail "the fleet options dialog did not close"
}

# tui_kill [session]
# Kill the tmux session if it is still around. Safe to call multiple times.
tui_kill() {
  local session="${1:-${TUI_SESSION}}"
  tmux kill-session -t "${session}" >/dev/null 2>&1 || true
}

# tui_assert_contains <needle> [message]
# Fails the test if the current screen does NOT contain needle.
tui_assert_contains() {
  local needle="$1"; local msg="${2:-screen missing expected text}"
  local screen
  screen=$(tui_capture)
  if ! printf '%s' "${screen}" | grep -qF -- "${needle}"; then
    printf -- '--- screen ---\n%s\n--- end ---\n' "${screen}" >&2
    fail "${msg}: needle=[${needle}]"
  fi
}

# tui_assert_not_contains <needle> [message]
tui_assert_not_contains() {
  local needle="$1"; local msg="${2:-screen unexpectedly contains text}"
  local screen
  screen=$(tui_capture)
  if printf '%s' "${screen}" | grep -qF -- "${needle}"; then
    printf -- '--- screen ---\n%s\n--- end ---\n' "${screen}" >&2
    fail "${msg}: needle=[${needle}]"
  fi
}

# tui_cursor_to <ERE> [session]
# Press j until the cursor ("> ") is on a line matching the extended regex.
# By looking, not by counting: the rows above a target vary with the tools
# installed and the rows other features add.
tui_cursor_to() {
  local pattern="$1" session="${2:-${TUI_SESSION}}" _
  for _ in $(seq 1 90); do
    if tui_capture "${session}" | grep -Eq ">[[:space:]]+${pattern}"; then
      return 0
    fi
    tui_send j "${session}"
    sleep 0.15
  done
  tui_capture "${session}" >&2
  fail "could not put the cursor on '${pattern}'"
}

# itest_kube_clusters <perl>
# Evaluate perl with @c = the Kubernetes connections in
# ~/.fleet/kube_clusters.json (fleetd's registry), e.g.
# `itest_kube_clusters 'print scalar @c'`.
itest_kube_clusters() {
  perl -MJSON::PP -e 'local $/; my $j = decode_json(<STDIN>); my @c = @{$j->{clusters} || []}; '"$1" \
    < "${HOME}/.fleet/kube_clusters.json"
}

# itest_fake_kubeconfig <file>
# Write a kubeconfig no cluster answers: context itest-dead (a token, namespace
# team, the current context) and itest-plugin (an exec login plugin,
# itest-login-plugin, that is not on any PATH).
itest_fake_kubeconfig() {
  cat > "$1" <<'KUBECONFIG'
apiVersion: v1
kind: Config
clusters:
- name: dead
  cluster:
    server: https://127.0.0.1:9
- name: lab
  cluster:
    server: https://127.0.0.1:10
users:
- name: tok
  user:
    token: itest-not-a-real-token
- name: plug
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: itest-login-plugin
      interactiveMode: Never
contexts:
- name: itest-dead
  context:
    cluster: dead
    user: tok
    namespace: team
- name: itest-plugin
  context:
    cluster: lab
    user: plug
current-context: itest-dead
KUBECONFIG
}

# server_count — number of live `fleet server` daemons for this suite's
# binary. Shared here because macOS pgrep has no -c flag (the old per-test
# `pgrep -fc` printed nothing there and every count assertion failed); the
# pipe form behaves identically on Linux and macOS. `|| true` absorbs
# pgrep's exit 1 on zero matches under pipefail.
server_count() {
  { pgrep -f "${FLEET_BIN} server" 2>/dev/null || true; } | wc -l | tr -d ' '
}

# file_mode <path> — octal permission bits of a HOST file, portably. Not
# stat: GNU spells it -c '%a' and BSD -f '%Lp', and no probe order survives
# the BSD-shim job — the shim rejects -c like BSD, but a -f fallback lands
# on the real GNU stat where -f means file*system* status. perl's stat has
# one meaning everywhere. (In-container stat calls stay plain `stat -c` —
# containers are Linux regardless of the host.)
file_mode() {
  perl -e 'printf "%o", (stat shift)[2] & 07777' "$1"
}

# === Test markers ===
#
# A test declares how it relates to the backends with "# itest: <marker>"
# lines:
#   no-docker                    host-side only (no instance); the subset
#                                FLEET_ITEST_NO_DOCKER=1 runs
#   k8s                          runs on FLEET_ITEST_BACKEND=kubernetes too;
#                                the unit that ports its feature adds it
#   devcontainer-only: <reason>  checks a Docker mechanism clusters lack, so
#                                the kubernetes run skips it; <reason> says
#                                what replaces it. These must reach 0 before
#                                the kubernetes backend merges.
# An unmarked test that creates instances is not ported to kubernetes yet.
# run.sh fails a test with a malformed marker without running it.

# _itest_markers <test-file> — the file's marker values, one per line.
_itest_markers() {
  sed -n 's/^# itest:[[:space:]]*//p' "$1" | sed 's/[[:space:]]*$//'
}

# itest_has_marker <test-file> <marker> — e.g. `itest_has_marker "$f" k8s`.
itest_has_marker() {
  local markers
  markers=$(_itest_markers "$1")
  printf '%s\n' "${markers}" | grep -qxF -- "$2"
}

# itest_exemption <test-file> — the devcontainer-only reason; empty when the
# test is not exempt.
itest_exemption() {
  local markers
  markers=$(_itest_markers "$1")
  printf '%s\n' "${markers}" | sed -n 's/^devcontainer-only:[[:space:]]*//p' | head -n 1
}

# itest_marker_problem <test-file> — prints why the file's markers are
# malformed; prints nothing when they are fine.
itest_marker_problem() {
  local marker k8s="" exempt="" reason
  while IFS= read -r marker; do
    case "${marker}" in
      ""|no-docker) ;;
      k8s) k8s=1 ;;
      devcontainer-only|devcontainer-only:*)
        exempt=1
        reason=$(printf '%s' "${marker#devcontainer-only}" | sed 's/^://' | tr -d '[:space:]')
        if [ -z "${reason}" ]; then
          echo "devcontainer-only needs a reason: '# itest: devcontainer-only: <reason>'"
          return 0
        fi
        ;;
      *)
        echo "unknown marker '# itest: ${marker}' (want no-docker, k8s or devcontainer-only: <reason>)"
        return 0
        ;;
    esac
  done <<MARKERS
$(_itest_markers "$1")
MARKERS
  if [ -n "${k8s}" ] && [ -n "${exempt}" ]; then
    echo "'# itest: k8s' and '# itest: devcontainer-only' contradict each other"
  fi
}

# itest_k8s_class <test-file> — where the test stands on kubernetes: k8s,
# devcontainer-only, host-only (no-docker, not marked k8s) or pending.
itest_k8s_class() {
  if itest_has_marker "$1" k8s; then
    echo k8s
  elif [ -n "$(itest_exemption "$1")" ]; then
    echo devcontainer-only
  elif itest_has_marker "$1" no-docker; then
    echo host-only
  else
    echo pending
  fi
}
