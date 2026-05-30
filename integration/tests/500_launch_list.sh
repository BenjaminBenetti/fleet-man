#!/usr/bin/env bash
# Description: `fleet launch list` inside an instance prints the configured fleetLaunch Links and Apps.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

# Uses the launch fixture, whose devcontainer.json declares a
# customizations.fleet.fleetLaunch block with two Links (Grafana,
# Project Docs) and one App (Webapp on :8080).
setup_launch_test

info "fleet up alpha (launch fixture)"
fleet_up alpha

# `fleet launch` is meant to be run INSIDE an instance: the host stages
# the fleet binary at /usr/bin/fleet, and `launch list` auto-detects the
# workspace devcontainer.json (devcontainer exec lands in the workspace
# folder). No host control socket is needed for the list form.
info "fleet exec alpha -- fleet launch list"
list_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- fleet launch list)
info "launch list output:"
printf '%s\n' "${list_out}"

# Section headers.
assert_contains "${list_out}" "Links:" "missing Links section header"
assert_contains "${list_out}" "Apps:"  "missing Apps section header"

# Link entries render as "<title> — <url>".
assert_contains "${list_out}" "Grafana — http://localhost:3000" \
  "Grafana link not listed"
assert_contains "${list_out}" "Project Docs — https://example.com/docs" \
  "Project Docs link not listed"

# App entries render as "<title> — http://localhost:<port>".
assert_contains "${list_out}" "Webapp — http://localhost:8080" \
  "Webapp app not listed"

# Neither section should report itself empty.
assert_not_contains "${list_out}" "(none)" \
  "a section was unexpectedly empty"

pass "launch list"
