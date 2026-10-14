#!/usr/bin/env bash
# itest: no-docker
# itest: k8s
# Description: TUI new-fleet with a file:// template URL prompts for the fleet name, then adds the fleet; with docker, an instance created from its Deploy row (the remembered target: a cluster on kubernetes) is a copy of the template
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test

info "spawning TUI on empty fleet list"
tui_spawn
tui_wait_for "No instances" 15

info "opening new-fleet dialog: it must advertise the file:// option"
tui_send n
tui_wait_for "New fleet" 5
tui_assert_contains "file:///abs/dir" "new-fleet dialog should mention file:// templates"

info "submitting a file:// template URL"
tui_send_text "${FIXTURE_TEMPLATE_URL}"
tui_send Enter

info "the fleet-name prompt appears, pre-filled with the dir's base name"
tui_wait_for "Name:" 5
tui_assert_contains "Template:" "name prompt should label the source as a template"
# Anchor on the Name row itself (label, its padding, the input's "> " prompt,
# then the value), not the bare name — the Template: line above already
# contains it as the URL's last segment, so a bare match would pass even
# with an empty prefill.
tui_assert_contains "Name:     > ${FIXTURE_REPO_NAME}" "name prompt should pre-fill the template dir's base name"

info "replacing the suggested name with an explicit one"
# ctrl+u clears the (cursor-at-end) input; then type the real name.
tui_send "C-u"
tui_send_text "scratch-fleet"
tui_send Enter

info "inspection runs against the template dir and the fleet is added"
tui_wait_for "Added fleet scratch-fleet" 15
tui_assert_not_contains "No devcontainer.json found" \
  "warn dialog appeared even though the template has a devcontainer.json"

info "verifying state persistence"
assert_file_exists "${HOME}/.fleet/state.json"
state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" "\"scratch-fleet\"" "fleet should be persisted under the typed name"
assert_contains "${state}" "\"remote\": \"${FIXTURE_TEMPLATE_URL}\"" "fleet should record the file:// remote"
assert_not_contains "${state}" "\"${FIXTURE_REPO_NAME}\": {" "fleet must not be auto-named after the template dir"

if [ -n "${FLEET_ITEST_NO_DOCKER:-}" ]; then
  pass "TUI new-fleet file:// template prompts for a name and adds the fleet"
fi

# ===========================================
# An instance of the template fleet, created from the New instance dialog:
# the Deploy row opens on the remembered target (on kubernetes the run's
# connection, which a template fleet now keeps), the branch row is locked,
# and the workspace is a copy of the template, not a clone.
# ===========================================
deploy_label="[ Devcontainer ]"
if itest_backend_is kubernetes; then
  deploy_label="[ $(itest_row_target) ]"
fi
# A file the template dir holds but its git history does not: only a copy of
# the dir (not a clone) brings it into the instance.
printf 'from-template-174\n' > "${FIXTURE_REPO_DIR}/sentinel-174.txt"
info "pressing 'a' on the template fleet's header"
# After 'n' the cursor may have stayed on the settings row (see 160).
tui_send k
sleep 0.3
tui_send a
tui_wait_for "instance-name" 5
tui_assert_contains "${deploy_label}" "the template fleet's Deploy row should open on the remembered target"
tui_assert_contains "n/a (template dir is copied as-is)" "the branch row should be locked for a template fleet"

info "naming the instance tpl and creating it"
tui_send_text "tpl"
sleep 0.3
tui_send Enter
tui_wait_for_absent "instance-name" 5
tui_wait_for "○ idle" 300

backend=$(jq -r '.fleets["scratch-fleet"].instances[] | select(.name == "tpl") | .backend' "${HOME}/.fleet/state.json")
assert_equals "${FLEET_ITEST_BACKEND}" "${backend}" "the template instance should run on the backend under test"
itest_workspace_test scratch-fleet/tpl -e .devcontainer/devcontainer.json \
  || fail "the template instance's workspace lacks .devcontainer/devcontainer.json"
assert_equals "from-template-174" "$(itest_workspace_read scratch-fleet/tpl sentinel-174.txt)" \
  "the template instance's workspace must be a copy of the template dir"

pass "TUI new-fleet file:// template prompts for a name, adds the fleet, and creates a template instance on the remembered target"
