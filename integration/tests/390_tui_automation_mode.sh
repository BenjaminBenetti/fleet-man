#!/usr/bin/env bash
# Description: TUI automation mode (issue #188) — 'm' toggles a fleet between its
# instance view and its automation view (triggers + agents groups). Adding an
# agent through the dialog persists it and shows it in the agents group, and 'm'
# toggles back to the instance view.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15

# ===========================================
# Instance view shows the [automations] toggle button.
# ===========================================
tui_assert_contains "[automations]" "fleet header should offer the [automations] toggle"

# ===========================================
# 'm' switches the fleet to automation view: triggers + agents groups, the
# "+ add" action rows, and the [instances] toggle.
# ===========================================
info "pressing 'm' to enter automation mode"
tui_send m
tui_wait_for "triggers" 5
tui_assert_contains "agents" "agents group header should be visible"
tui_assert_contains "+ add trigger" "add-trigger action row missing"
tui_assert_contains "+ add agent" "add-agent action row missing"
tui_assert_contains "[instances]" "automation view should offer the [instances] toggle"

# ===========================================
# Add an agent. From the fleet header the rows are: header, triggers,
# + add trigger, agents, + add agent — four 'j' reach "+ add agent".
# ===========================================
info "opening the add-agent dialog"
for _ in 1 2 3 4; do tui_send j; sleep 0.15; done
tui_send Enter
tui_wait_for "New agent" 5
tui_assert_contains "Tmux:" "agent dialog should expose the tmux toggle"

info "typing an agent name and saving"
tui_send Enter         # activate the Name field
sleep 0.2
tui_send_text "builder"
sleep 0.2
tui_send Enter         # commit the name
sleep 0.2
# Name -> Command -> Tmux -> Sys prompt -> Backend -> [ Save ]: five 'j'.
for _ in 1 2 3 4 5; do tui_send j; sleep 0.15; done
tui_send Enter         # save
tui_wait_for "agents (1)" 10
tui_assert_contains "builder" "saved agent row should be visible"

# ===========================================
# 'm' toggles back to the instance view.
# ===========================================
info "pressing 'm' to return to the instance view"
tui_send m
tui_wait_for "[automations]" 5
tui_assert_contains "alpha" "instance view should show the instance again"

pass "TUI automation mode: toggle, add agent, toggle back"
