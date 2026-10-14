#!/usr/bin/env bash
# itest: k8s
# Description: on an SELinux-enforcing node (FLEET_ITEST_SELINUX=1, as the nightly k3s job sets it), a fleet's pods run at one MCS level of their own (the level their pod spec names, another fleet's differs), siblings read and write each other's files on the fleet's share (and the deep test's two-pod probe does at one level), and how long a sibling takes to start beside a share of 20000 files (the volume's relabel) is recorded; elsewhere it passes saying it did not run.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a fleet's share under SELinux"
[ "${FLEET_ITEST_SELINUX:-}" = 1 ] || pass "not run: FLEET_ITEST_SELINUX is not set (integration/nightly/selinux-k3s.sh sets it)"
report="${FLEET_ITEST_SELINUX_REPORT:-/dev/null}"
record() {
  printf '%s\t%s\t%s\t-\t-\n' "$1" "$2" "$3" >> "${report}"
  info "$1 = $2 $3"
}

setup_agent_test
other="itest-other"
cat > "${HOME}/.fleet/state.json" <<STATE
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}", "remote": "${FIXTURE_REPO_URL}",
      "settings": { "claudeCodeMount": true, "homeDir": "/home/node", "customMounts": ["/opt/data"] },
      "instances": []
    },
    "${other}": {
      "name": "${other}", "remote": "${FIXTURE_REPO_URL}",
      "settings": { "claudeCodeMount": true, "homeDir": "/home/node" },
      "instances": []
    }
  }
}
STATE
# Three pods on the one node (the default 2Gi request each would not fit).
"${FLEET_BIN}" cluster set itest resources.cpu_request=100m resources.memory_request=256Mi >/dev/null
alpha="${FIXTURE_REPO_NAME}/alpha"
beta="${FIXTURE_REPO_NAME}/beta"
gamma="${other}/gamma"
run_in() { "${FLEET_BIN}" exec "$1" -- sh -c "$2"; }
# level_of <ref> — the MCS level (s0:cX,cY) the instance's processes run at.
level_of() { run_in "$1" 'cat /proc/self/attr/current' | tr -d '\0' | cut -d: -f4-; }
spec_level_of() {
  itest_k8s_kubectl get pod "$(itest_k8s_pod "$(itest_container_id "$1")")" -o 'jsonpath={.spec.securityContext.seLinuxOptions.level}'
}

fleet_up alpha
label=$(run_in "${alpha}" 'cat /proc/self/attr/current' | tr -d '\0')
info "alpha runs as ${label}"
case "${label}" in
  *:container_t:s0:c*) ;;
  *) fail "alpha's processes have no container MCS label (is the node enforcing SELinux?): ${label}" ;;
esac
fleet_up beta
"${FLEET_BIN}" up "${gamma}" --repo "${FIXTURE_REPO_URL}" >/dev/null

info "one level per fleet: the pod spec's, shared by siblings, another fleet's another"
alpha_level=$(level_of "${alpha}")
assert_equals "$(spec_level_of "${alpha}")" "${alpha_level}" "alpha runs at its pod spec's level"
assert_equals "${alpha_level}" "$(level_of "${beta}")" "siblings run at one level"
[ "$(level_of "${gamma}")" != "${alpha_level}" ] || fail "another fleet runs at the same level ${alpha_level}"

info "the deep test's two-pod probe shares a file at one level"
deep=$("${FLEET_BIN}" cluster test --deep itest 2>&1 || true)
printf '%s\n' "${deep}" | grep -E '^share pods ' || true
printf '%s\n' "${deep}" | grep -Eq "^share pods +pass .*under SELinux, at one level \(s0:c[0-9]+,c[0-9]+\), the second read the first's file" \
  || fail "the deep test's two-pod probe did not share a file under SELinux: $(printf '%s\n' "${deep}" | grep -E '^share pods' || printf '%s' "${deep}")"

info "siblings share the fleet's files"
run_in "${alpha}" 'echo from-alpha > ~/.claude/selinux.txt && echo from-alpha > /opt/data/selinux.txt'
assert_equals "from-alpha" "$(run_in "${beta}" 'cat ~/.claude/selinux.txt')" "beta reads alpha's file"
run_in "${beta}" 'echo from-beta >> ~/.claude/selinux.txt && echo from-beta > /opt/data/beta.txt' \
  || fail "beta cannot write beside alpha's files"
assert_equals "from-beta" "$(run_in "${alpha}" 'cat /opt/data/beta.txt')" "alpha reads beta's custom-mount file"
file_label=$(run_in "${alpha}" 'ls -Zd ~/.claude/selinux.txt 2>/dev/null | cut -d" " -f1' || true)
info "a share file's label: ${file_label:-unknown}"
record share_file_label "${file_label:-unknown}" label

info "a sibling's start beside an empty share, then beside 20000 files"
start_ms() {
  local s
  "${FLEET_BIN}" stop "$1" >/dev/null
  s=$(date +%s%N)
  "${FLEET_BIN}" start "$1" >/dev/null
  echo $(( ($(date +%s%N) - s) / 1000000 ))
}
record sibling_start_ms "$(start_ms "${beta}")" ms
run_in "${alpha}" 'mkdir -p ~/.claude/many && cd ~/.claude/many && i=0; while [ $i -lt 20000 ]; do : > f$i; i=$((i+1)); done'
record sibling_start_20k_files_ms "$(start_ms "${beta}")" ms
assert_equals "20000" "$(run_in "${beta}" 'ls ~/.claude/many | wc -l' | tr -d ' ')" "beta sees the 20000 files after its start"
run_in "${beta}" 'echo after > ~/.claude/many/after' || fail "beta cannot write into the share after its start"
assert_equals "after" "$(run_in "${alpha}" 'cat ~/.claude/many/after')" "alpha reads what beta wrote after its start"

pass "one MCS level per fleet; siblings share files under SELinux"
