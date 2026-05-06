#!/usr/bin/env bash
# Description: local_dir backend skips auto-install of dotfiles to avoid trampling the host environment.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test

# Configure dotfiles auto-install with a URL that, if reached, would
# fail loudly. local_dir must NOT attempt the clone — if it did, the
# warn file would record a "dotfiles install failed" entry.
mkdir -p "${HOME}/.fleet"
cat > "${HOME}/.fleet/config.json" <<'JSON'
{
  "general_settings": {},
  "agent_settings": {"tool_selection": "claude"},
  "dotfiles_settings": {
    "repo_url": "https://example.invalid/should-never-clone.git",
    "install_script": "install.sh",
    "auto_install": true
  },
  "coder_settings": {"template": ""},
  "codespaces_settings": {}
}
JSON

# A pre-existing host ~/dotfiles dir would normally cause the dotfiles
# script to skip. Make sure there is nothing there so any attempted
# clone is observable. Save and restore if one happens to exist.
preserved_dotfiles=""
if [ -e "${HOME}/dotfiles" ]; then
  preserved_dotfiles="${HOME}/dotfiles.itest-bak.$$"
  mv "${HOME}/dotfiles" "${preserved_dotfiles}"
fi
itest_cleanup() {
  if [ -n "${preserved_dotfiles}" ] && [ -e "${preserved_dotfiles}" ]; then
    rm -rf "${HOME}/dotfiles"
    mv "${preserved_dotfiles}" "${HOME}/dotfiles"
  fi
}

info "fleet up alpha --backend local_dir (with auto_install dotfiles configured)"
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" --backend local_dir

# No dotfiles clone should have been attempted on the host.
assert_file_absent "${HOME}/dotfiles"

# No warning file from a failed dotfiles install should exist.
warn_path="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-alpha.warn"
if [ -e "${warn_path}" ]; then
  printf -- '--- warn file ---\n%s\n--- end ---\n' "$(cat "${warn_path}")" >&2
  fail "local_dir created a dotfiles warning file: ${warn_path}"
fi

# Sanity: instance is still running.
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_out}" "running" "instance should be running after dotfiles skip"

pass "skips dotfiles (local_dir)"
