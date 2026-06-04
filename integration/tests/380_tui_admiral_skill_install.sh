#!/usr/bin/env bash
# Description: Launching the TUI installs the Fleet Admiral skill into ~/.claude/skills.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

SKILL_DIR="${HOME}/.claude/skills/fleet-admiral"
SKILL_MD="${SKILL_DIR}/SKILL.md"
SKILL_HASH="${SKILL_DIR}/.hash"

# Start from a clean slate so we observe the install actually happen, not a
# leftover from a previous run / real user install. (Reinstalling the skill is
# harmless and correct — the TUI re-creates it below.)
rm -rf "${SKILL_DIR}"
assert_file_absent "${SKILL_MD}"

setup_test

# No fleet_up needed: the skill install fires at the top of tui.Run(), before
# any instance state matters, so a bare TUI launch is enough to exercise it.
info "Spawning TUI"
tui_spawn

# Poll for the installed skill. The install is synchronous at the start of
# Run(), so it lands as soon as the process is up — but give tmux/the binary a
# few seconds of headroom on slow runners.
info "Waiting for Fleet Admiral skill to be installed"
deadline=$(( $(date +%s) + $(_scale_timeout 15) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  [ -f "${SKILL_MD}" ] && break
  sleep 0.25
done

assert_file_exists "${SKILL_MD}"
assert_file_exists "${SKILL_HASH}"

# The installed manifest must be the real skill: assert on its frontmatter.
skill_body="$(cat "${SKILL_MD}")"
assert_contains "${skill_body}" "name: fleet-admiral" "SKILL.md missing skill name frontmatter"
assert_contains "${skill_body}" "Fleet Admiral"       "SKILL.md missing skill body"

# The hash file gates the fast-path skip on subsequent startups: it must hold a
# 64-char hex sha256 of the content that was written.
hash_body="$(cat "${SKILL_HASH}")"
if ! printf '%s' "${hash_body}" | grep -qE '^[0-9a-f]{64}$'; then
  fail ".hash is not a sha256 hex digest: [${hash_body}]"
fi

expected_hash="$(sha256sum "${SKILL_MD}" | cut -d' ' -f1)"
assert_equals "${expected_hash}" "${hash_body}" ".hash does not match installed SKILL.md content"

pass "TUI startup installs the Fleet Admiral skill"
