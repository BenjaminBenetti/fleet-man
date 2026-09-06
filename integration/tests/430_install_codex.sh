#!/usr/bin/env bash
# Description: Codex CLI is installed by the startup script on an image WITHOUT node/npm.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

# Deliberately uses the default debian-base fixture (no node/npm): the
# regression behind issue #145 was the npm-based codex install silently
# failing on exactly this image class. The standalone installer needs
# only curl + tar. The node-image path stays covered by 440_install_both.
setup_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" false true /home/vscode

info "fleet up alpha (codex install enabled, no-node image)"
fleet_up alpha

info "asserting startup script log was created"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -f /home/vscode/.fleet/startup/codex.log \
  || fail "codex.log was not created — startup runner did not execute"

info "codex.log contents:"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /home/vscode/.fleet/startup/codex.log || true

info "asserting codex binary is on the user's PATH"
codex_path=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- bash -lc 'command -v codex' 2>&1) \
  || fail "codex is not on the user's PATH after install — output: ${codex_path}"
info "codex resolved at: ${codex_path}"
assert_contains "${codex_path}" "codex" "command -v codex returned no path"

info "asserting the complete package is installed outside the shared Codex mount"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- bash -lc '
  set -e
  package="$HOME/.local/share/fleet/codex/packages/standalone/current"
  test "$(command -v codex)" -ef "$package/bin/codex"
  test -f "$package/codex-package.json"
  test -x "$package/bin/codex-code-mode-host"
  test -x "$package/codex-path/rg"
  test -x "$package/codex-resources/bwrap"
  test ! -e "$HOME/.codex/packages/standalone"
  codex --version
' || fail "Codex package is incomplete or was installed in the shared mount"

pass "codex installed"
