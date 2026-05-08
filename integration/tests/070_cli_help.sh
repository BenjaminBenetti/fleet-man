#!/usr/bin/env bash
# Description: `fleet --help` and every subcommand --help exit 0 and surface their usage.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

# This test is pure CLI surface — no devcontainer, no docker — so we
# skip setup_test (it would just wipe ~/.fleet for nothing).

# ===========================================
# Root help
# ===========================================
info "fleet --help"
root_help=$("${FLEET_BIN}" --help)
assert_contains "${root_help}" "fleet-man is a CLI/TUI tool" "root help missing long description"
assert_contains "${root_help}" "Available Commands:"        "root help missing commands header"

# Each visible subcommand we ship must appear in the Available Commands block.
# This protects against accidentally dropping one from NewRootCmd. The
# `_create-instance` command is intentionally Hidden, so it's not asserted.
for sub in up stop start down destroy list exec code logs status port-forward shell; do
  assert_contains "${root_help}" "${sub}" "root help missing subcommand: ${sub}"
done

# The hidden internal command must NOT show in the public help — surfacing
# it would invite users to call it directly, breaking the TUI contract.
assert_not_contains "${root_help}" "_create-instance" "internal command should remain hidden"

# ===========================================
# Per-subcommand help. Each must succeed and contain its own short text.
# Tuple format: <command>|<expected substring>
# ===========================================
declare -a checks=(
  "up|Spawn a new instance"
  "stop|Stop a devcontainer instance"
  "start|Start an existing stopped"
  "down|Stop and remove an instance"
  "destroy|Remove a fleet"
  "list|List devcontainer instances"
  "exec|Execute a command inside an instance"
  "logs|Show logs for an instance"
  "status|Show fleet-wide status summary"
)

for entry in "${checks[@]}"; do
  cmd="${entry%%|*}"
  expected="${entry#*|}"
  info "fleet ${cmd} --help"
  out=$("${FLEET_BIN}" "${cmd}" --help)
  assert_contains "${out}" "${expected}" "${cmd} --help missing expected text"
  assert_contains "${out}" "Usage:"        "${cmd} --help missing Usage block"
done

# `ls` is the alias for `list` — verify it resolves to the same command.
info "fleet ls --help (alias)"
ls_help=$("${FLEET_BIN}" ls --help)
assert_contains "${ls_help}" "List devcontainer instances" "ls alias should resolve to list"

# Up advertises its flags so users discover --repo / --branch.
info "fleet up --help advertises flags"
up_help=$("${FLEET_BIN}" up --help)
assert_contains "${up_help}" "--repo"   "up --help missing --repo flag"
assert_contains "${up_help}" "--branch" "up --help missing --branch flag"

pass "CLI --help works for root and all subcommands"
