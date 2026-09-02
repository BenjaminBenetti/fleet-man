#!/usr/bin/env bash
# itest: no-docker
# Description: TUI new-fleet with a file:// template URL prompts for the fleet name, then adds the fleet
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

pass "TUI new-fleet file:// template prompts for a name and adds the fleet"
