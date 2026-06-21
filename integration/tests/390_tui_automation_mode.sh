#!/usr/bin/env bash
# Description: TUI automation mode (issue #188) — 'm' (or selecting the header's
# [automations]/[instances] toggle with →/l and pressing enter) flips a fleet
# between its instance view and its automation view (triggers + agents groups).
# Adding an agent through the dialog persists it and shows it in the agents
# group, and 'm' toggles back to the instance view.
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
# Keyboard selection: →/l focuses the header toggle and enter activates it,
# flipping to the automation view; a second enter flips back. (The cursor starts
# on the fleet header.)
# ===========================================
info "selecting the toggle with 'l' and pressing enter to enter automation mode"
tui_send l
sleep 0.2
tui_send Enter
tui_wait_for "[instances]" 5
tui_assert_contains "triggers" "selecting the toggle + enter should open the automation view"

info "pressing enter again to flip back to the instance view"
tui_send Enter
tui_wait_for "[automations]" 5
tui_assert_contains "alpha" "a second enter should return to the instance view"
tui_send h   # deselect the toggle before the rest of the flow

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
# 'a' opens the add dialog for the group the cursor is in (not the add-instance
# workflow). The cursor sits on the fleet header right after entering automation
# mode, where 'a' adds a trigger. esc cancels, leaving the cursor on the header.
# ===========================================
info "pressing 'a' on the header opens the add-trigger dialog"
tui_send a
tui_wait_for "New trigger" 5
tui_send Escape
sleep 0.3
tui_wait_for "+ add trigger" 5

# ===========================================
# Add an agent. From the fleet header the rows are: header, triggers,
# + add trigger, agents, + add agent — four 'j' reach "+ add agent".
# ===========================================
info "opening the add-agent dialog"
for _ in 1 2 3 4; do tui_send j; sleep 0.15; done
tui_send Enter
tui_wait_for "New agent" 5
tui_assert_contains "Backend:" "agent dialog should expose the backend selector"

info "typing an agent name and saving"
tui_send Enter         # activate the Name field
sleep 0.2
tui_send_text "builder"
sleep 0.2
tui_send Enter         # commit the name
sleep 0.2
# Name -> Command -> Sys prompt -> Backend -> [ Save ]: four 'j'.
for _ in 1 2 3 4; do tui_send j; sleep 0.15; done
tui_send Enter         # save
tui_wait_for "agents (1)" 10
tui_assert_contains "builder" "saved agent row should be visible"

# ===========================================
# 'd' on a trigger/agent confirms before deleting (rather than deleting
# immediately). The cursor lands on the "builder" agent row after the save.
# ===========================================
info "pressing 'd' on the agent opens a confirm dialog"
tui_send d
tui_wait_for "Delete agent" 5
tui_assert_contains "builder" "confirm prompt should name the agent"

info "cancelling with 'n' keeps the agent"
tui_send n
sleep 0.3
tui_wait_for "agents (1)" 5

info "confirming with 'y' deletes the agent"
tui_send d
tui_wait_for "Delete agent" 5
tui_send y
tui_wait_for "agents (0)" 10

# ===========================================
# 'm' toggles back to the instance view.
# ===========================================
info "pressing 'm' to return to the instance view"
tui_send m
tui_wait_for "[automations]" 5
tui_assert_contains "alpha" "instance view should show the instance again"

pass "TUI automation mode: toggle, add agent, toggle back"
