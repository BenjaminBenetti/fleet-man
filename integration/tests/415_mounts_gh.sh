#!/usr/bin/env bash
# Description: gh CLI mount appears in the container at the expected path
# and persists a host-written file across the bind boundary.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_agent_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/node true

info "seeding host-side gh config dir with a stand-in hosts.yml"
host_gh_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.config/gh"
mkdir -p "${host_gh_dir}"
cat > "${host_gh_dir}/hosts.yml" <<'EOF'
github.com:
    user: fleet-integration
    oauth_token: ghp_dummy_integration_token
    git_protocol: https
EOF

info "fleet up alpha (gh mount enabled)"
fleet_up alpha

info "asserting /home/node/.config/gh is a directory inside the container"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -d /home/node/.config/gh \
  || fail "/home/node/.config/gh is missing or not a directory"

info "asserting /home/node/.config/gh is a real bind mount"
mountinfo=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /proc/self/mountinfo)
assert_contains "${mountinfo}" " /home/node/.config/gh " "gh dir is not a mount point"

info "asserting host-seeded hosts.yml is visible inside the container"
container_hosts=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /home/node/.config/gh/hosts.yml)
assert_contains "${container_hosts}" "ghp_dummy_integration_token" \
  "host-seeded hosts.yml did not appear inside the container"

info "asserting a container-side write propagates back to the host"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- \
  sh -c 'printf "editor: nano\n" > /home/node/.config/gh/config.yml'
assert_file_exists "${host_gh_dir}/config.yml"
host_config=$(cat "${host_gh_dir}/config.yml")
assert_contains "${host_config}" "editor: nano" \
  "container-side write did not reach the host file"

info "asserting host-side fleet mount root exists"
assert_file_exists "${host_gh_dir}"

pass "gh mount applied"
