#!/usr/bin/env bash
# Description: `fleet clone` copies a running instance into a new one with its own container: the clone keeps the source's apt-installed package, $HOME and uncommitted workspace changes, is stamped with its own name, and the two diverge afterwards.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

src="${FIXTURE_REPO_NAME}/alpha"
dst="${FIXTURE_REPO_NAME}/beta"
ws="/workspaces/${FIXTURE_REPO_NAME}"

src_container=$(itest_container_id "${src}")
[ -n "${src_container}" ] || fail "could not read alpha's container_id from state.json"

# State the clone must carry, from the three places an instance keeps it:
#   - root filesystem: a package installed with apt (dpkg database + /usr/bin),
#   - the remote user's $HOME (also the root filesystem, but its own concern),
#   - the workspace: an untracked file and an uncommitted edit to a tracked one.
info "seeding alpha: apt package, \$HOME file, uncommitted workspace changes"
itest_install_local_deb "${src}" fleet-itest-clone
assert_equals "installed" "$(itest_local_deb_status "${src}" fleet-itest-clone)" \
  "the package should be installed in alpha before the clone"
"${FLEET_BIN}" exec "${src}" -- sh -c 'printf "home-from-alpha\n" > "$HOME/.itest-clone-home"'
itest_workspace_write "${src}" UNTRACKED.txt "untracked-from-alpha"
itest_workspace_write "${src}" README.md "readme-edited-in-alpha"
src_head=$(itest_workspace_git "${src}" rev-parse HEAD)
src_status=$(itest_workspace_git "${src}" status --porcelain)
info "alpha HEAD ${src_head}; uncommitted: $(printf '%s' "${src_status}" | tr '\n' ' ')"

# --- clone ---
info "fleet clone alpha beta"
clone_out=$("${FLEET_BIN}" clone "${src}" beta 2>&1) || fail "fleet clone failed: ${clone_out}"
printf '%s\n' "${clone_out}"
assert_contains "${clone_out}" "Cloning ${src} -> ${dst}" "clone should announce source and destination"
assert_contains "${clone_out}" "Instance ${dst} is running" "clone should report the new instance running"

dst_container=$(itest_container_id "${dst}")
[ -n "${dst_container}" ] || fail "could not read beta's container_id from state.json"
[ "${src_container}" != "${dst_container}" ] || fail "the clone reuses alpha's container ${src_container}"
assert_container_running "${dst_container}" "the clone's container should be running"
assert_container_running "${src_container}" "cloning must leave the source running"
itest_container_belongs_to "${dst_container}" "${dst}" \
  || fail "beta's container is not matched to beta's workspace"

ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
printf '%s\n' "${ls_out}"
beta_row=$(printf '%s\n' "${ls_out}" | grep -E '^\S+\s+beta\s' || true)
assert_contains "${beta_row}" "running" "ls should show beta running"

# --- what the clone carried over ---
assert_equals "installed" "$(itest_local_deb_status "${dst}" fleet-itest-clone)" \
  "the clone should keep alpha's apt-installed package"
home_out=$("${FLEET_BIN}" exec "${dst}" -- sh -c 'cat "$HOME/.itest-clone-home"')
assert_equals "home-from-alpha" "${home_out}" "the clone should keep alpha's \$HOME"
assert_equals "untracked-from-alpha" "$("${FLEET_BIN}" exec "${dst}" -- cat "${ws}/UNTRACKED.txt")" \
  "the clone should keep alpha's untracked workspace file"
assert_equals "readme-edited-in-alpha" "$("${FLEET_BIN}" exec "${dst}" -- cat "${ws}/README.md")" \
  "the clone should keep alpha's uncommitted edit"
assert_equals "${src_head}" "$(itest_workspace_git "${dst}" rev-parse HEAD)" \
  "the clone's checkout should sit on alpha's commit"
assert_equals "${src_status}" "$(itest_workspace_git "${dst}" status --porcelain)" \
  "the clone's checkout should have alpha's uncommitted changes, no more and no less"

# The clone is re-stamped as itself: its shells see their own instance name.
name_out=$("${FLEET_BIN}" exec "${dst}" -- bash -c '. "$HOME/.fleet/fleet.rc" >/dev/null 2>&1; printf "%s\n" "${FLEET_INSTANCE_NAME:-}"')
assert_equals "beta" "${name_out}" "the clone's fleet.rc should name the clone, not its source"

# --- the two are independent from here on ---
"${FLEET_BIN}" exec "${dst}" -- sh -c 'printf "home-from-beta\n" > "$HOME/.itest-clone-home"'
itest_workspace_write "${dst}" BETA_ONLY.txt "beta-only"
assert_equals "home-from-alpha" "$("${FLEET_BIN}" exec "${src}" -- sh -c 'cat "$HOME/.itest-clone-home"')" \
  "a \$HOME write in the clone must not reach the source"
if itest_workspace_test "${src}" -e BETA_ONLY.txt; then
  fail "a workspace write in the clone reached the source's workspace"
fi
itest_workspace_write "${src}" ALPHA_ONLY.txt "alpha-only"
if itest_workspace_test "${dst}" -e ALPHA_ONLY.txt; then
  fail "a workspace write in the source reached the clone's workspace"
fi

# --- removing the clone leaves the source alone ---
if itest_backend_is devcontainer; then
  # Docker clones by committing the source into a fleet-clone-* image that the
  # clone runs from, which down must remove with it. Clusters copy volumes.
  clone_image=$(docker inspect --format '{{.Config.Image}}' "${dst_container}")
  info "beta runs from the committed image ${clone_image}"
  case "${clone_image}" in
    fleet-clone-*) ;;
    *) fail "the clone should run from a committed fleet-clone-* image, not ${clone_image}" ;;
  esac
fi
info "fleet down beta"
"${FLEET_BIN}" down "${dst}" >/dev/null
assert_container_absent "${dst_container}" "down should remove the clone's container"
[ -z "$(itest_container_leftovers "${dst}")" ] || fail "containers left for beta after down"
assert_container_running "${src_container}" "removing the clone must leave the source running"
assert_equals "installed" "$(itest_local_deb_status "${src}" fleet-itest-clone)" \
  "removing the clone must leave the source's package alone"

if itest_backend_is devcontainer; then
  if docker image inspect "${clone_image}" >/dev/null 2>&1; then
    fail "down left the clone's committed image ${clone_image} behind"
  fi
fi

pass "clone keeps rootfs, \$HOME and uncommitted work; source unaffected"
