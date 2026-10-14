#!/usr/bin/env bash
# itest: k8s
# Description: with dotfiles auto-install on, a new instance clones the dotfiles repo to ~/dotfiles and runs its install script once; stop/start leaves it alone, rebuild installs it again, and a hanging install is cut off and retried without blocking the instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
# The env below shortens the install timeout for the daemon; stop it so the
# setting does not reach a later test.
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

setup_test

# The dotfiles repo is the fixture repo itself, cloned inside the instance from
# its own workspace (so no network or host path is involved), with an install
# script and a file for it to link. A hanging script exercises the timeout.
mkdir -p "${FIXTURE_REPO_DIR}/itest-dotfiles"
cat > "${FIXTURE_REPO_DIR}/itest-dotfiles/install.sh" <<'EOF'
#!/bin/sh
# fleet runs this from the root of the dotfiles clone.
set -e
printf 'run\n' >> "$HOME/.itest-dotfiles.log"
pwd > "$HOME/.itest-dotfiles-cwd"
ln -sf "$PWD/itest-dotfiles/rc" "$HOME/.itest-dotfiles-rc"
printf 'export ITEST_DOTFILES=installed\n' >> "$HOME/.bashrc"
EOF
printf 'dotfiles-rc-content\n' > "${FIXTURE_REPO_DIR}/itest-dotfiles/rc"
cat > "${FIXTURE_REPO_DIR}/itest-dotfiles/hang.sh" <<'EOF'
#!/bin/sh
printf 'attempt\n' >> "$HOME/.itest-dotfiles-hang.log"
sleep 600
EOF
(cd "${FIXTURE_REPO_DIR}" && git add -A && git commit -q -m "fixture: dotfiles")
fixture_head=$(git -C "${FIXTURE_REPO_DIR}" rev-parse HEAD)

write_dotfiles_config() {  # <install script>
  cat > "${HOME}/.fleet/config.json" <<EOF
{
  "dotfiles_settings": {
    "repo_url": "/workspaces/${FIXTURE_REPO_NAME}",
    "install_script": "$1",
    "auto_install": true
  }
}
EOF
  itest_config_written
}
write_dotfiles_config itest-dotfiles/install.sh

# The daemon inherits these from the command that spawns it: a short attempt
# timeout, two attempts, for the hanging case below.
export FLEET_DOTFILES_INSTALL_TIMEOUT=10s
export FLEET_DOTFILES_INSTALL_ATTEMPTS=2
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
while [ "$(server_count)" != "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "a lingering daemon would not exit"
  sleep 0.2
done

inst="${FIXTURE_REPO_NAME}/alpha"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }
runs() { in_instance sh -c 'grep -c "^run$" "$HOME/.itest-dotfiles.log" 2>/dev/null || echo 0'; }

check_installed() {  # <when>
  assert_equals "${fixture_head}" "$(in_instance git -C /home/vscode/dotfiles rev-parse HEAD 2>&1)" \
    "\$HOME/dotfiles should be a clone of the dotfiles repo (${1})"
  assert_equals "/home/vscode/dotfiles" "$(in_instance sh -c 'cat "$HOME/.itest-dotfiles-cwd"')" \
    "the install script should run from the clone's root (${1})"
  assert_equals "dotfiles-rc-content" "$(in_instance sh -c 'cat "$HOME/.itest-dotfiles-rc"')" \
    "the install script's link should resolve into the clone (${1})"
  assert_equals "installed" "$(in_instance bash -ic 'printf "%s\n" "${ITEST_DOTFILES:-}"' 2>/dev/null | tail -n 1)" \
    "an interactive shell should pick up what the install script added to .bashrc (${1})"
}

info "fleet up alpha (dotfiles auto-install)"
up_out=$(fleet_up alpha 2>&1) || fail "fleet up failed: ${up_out}"
# Provisioning warnings land in the instance's creation log, not in `fleet up`
# output.
alpha_log="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-alpha.log"
assert_file_exists "${alpha_log}"
if grep -qF "dotfiles install failed" "${alpha_log}"; then
  cat "${alpha_log}"
  fail "a working dotfiles install should not warn in the creation log"
fi
check_installed "after create"
assert_equals "1" "$(runs)" "create should run the install script once"

info "fleet stop + start alpha"
"${FLEET_BIN}" stop "${inst}" >/dev/null
"${FLEET_BIN}" start "${inst}" >/dev/null
check_installed "after a stop and start"
assert_equals "1" "$(runs)" "start must not run the install script again"

if itest_backend_rebuilds; then
  info "fleet rebuild alpha"
  "${FLEET_BIN}" rebuild "${inst}" >/dev/null
  # The rebuilt container starts from the image again, so the clone and the
  # log are fresh: the install ran once in it.
  check_installed "after a rebuild"
  assert_equals "1" "$(runs)" "rebuild should install the dotfiles once into the new container"
else
  info "this backend cannot rebuild yet: the rebuild step is left out"
fi

info "fleet up beta with a dotfiles install script that hangs"
write_dotfiles_config itest-dotfiles/hang.sh
start_s=$(date +%s)
up_out=$("${FLEET_BIN}" up beta --repo "${FIXTURE_REPO_URL}" 2>&1) || fail "a hanging dotfiles install must not fail the create: ${up_out}"
info "beta came up in $(( $(date +%s) - start_s ))s"
beta="${FIXTURE_REPO_NAME}/beta"
# The give-up is a provisioning warning: it lands in the instance's creation log
# (what the TUI shows for the job) rather than failing the create.
creation_log="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-beta.log"
assert_file_exists "${creation_log}"
grep -qF "dotfiles install failed after 2 attempt(s)" "${creation_log}" \
  || { cat "${creation_log}"; fail "the creation log should warn that the dotfiles install gave up"; }
assert_container_running "$(itest_container_id "${beta}")" "beta should run despite the hanging dotfiles install"
assert_equals "2" "$("${FLEET_BIN}" exec "${beta}" -- sh -c 'grep -c "^attempt$" "$HOME/.itest-dotfiles-hang.log"')" \
  "each attempt should be cut off and the install retried once"

pass "dotfiles: installed once per container, bounded when they hang"
