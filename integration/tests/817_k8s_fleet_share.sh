#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a fleet's shared mounts (Claude Code's folder and files, a custom mount) live on the fleet's share: its first instance makes it (single-node mode) and records where it can be used; siblings run on that node and see one inode; the folders are the remote user's, ~/.claude.json the '{}' seed, the share's root out of the users' reach; the share outlives an instance for the next one.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a fleet's share on a cluster"

setup_agent_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" true false /home/node false false '["/opt/data"]'
# The share in the default class: single-node mode, also on a cluster whose
# ReadWriteMany class Auto would pick (multi-node mode is 823's).
default_class=$(itest_k8s_kubectl get storageclass -o go-template='{{range .items}}{{if eq (index .metadata.annotations "storageclass.kubernetes.io/is-default-class") "true"}}{{.metadata.name}}{{end}}{{end}}')
[ -z "${default_class}" ] || "${FLEET_BIN}" cluster set itest shared_storage_class="${default_class}" >/dev/null
alpha="${FIXTURE_REPO_NAME}/alpha"
beta="${FIXTURE_REPO_NAME}/beta"
gamma="${FIXTURE_REPO_NAME}/gamma"

# run_in <ref> <script> — the script's output in the instance, as its user.
run_in() { "${FLEET_BIN}" exec "$1" -- sh -c "$2"; }
# node_of <ref> — the node the instance's pod runs on.
node_of() { itest_k8s_kubectl get pod "$(itest_k8s_pod "$(itest_container_id "$1")")" -o 'jsonpath={.spec.nodeName}'; }

info "fleet up alpha + beta (Claude Code mount and /opt/data on)"
fleet_up alpha
fleet_up beta

info "the fleet's share: made once, single-node mode, its topology recorded"
share=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims share)
[ -n "${share}" ] || fail "the fleet has no share: $(itest_k8s_fleet_objects "${FIXTURE_REPO_NAME}" persistentvolumeclaims | tr '\n' ' ')"
assert_equals "single-node" "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.metadata.annotations.fleet\.dev/share-mode}')" "the share's mode"
[ -n "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.metadata.annotations.fleet\.dev/share-topology}')" ] \
  || fail "the share's topology was not recorded"
assert_equals "Bound" "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.status.phase}')" "the share is bound"
assert_equals "$(node_of "${alpha}")" "$(node_of "${beta}")" "the fleet's pods run on the share's node"

info "siblings share one inode"
run_in "${alpha}" 'echo from-alpha > ~/.claude/shared.txt'
assert_equals "$(run_in "${alpha}" 'stat -c %i ~/.claude/shared.txt')" "$(run_in "${beta}" 'stat -c %i ~/.claude/shared.txt')" "one inode for both"
assert_equals "from-alpha" "$(run_in "${beta}" 'cat ~/.claude/shared.txt')" "beta reads alpha's write at once"
run_in "${beta}" 'echo from-beta > /opt/data/custom.txt'
assert_equals "from-beta" "$(run_in "${alpha}" 'cat /opt/data/custom.txt')" "the custom mount is shared too"

info "the share's folders are the remote user's, its JSON files seeded, its root private"
uid=$(run_in "${alpha}" 'id -u')
assert_equals "${uid}" "$(run_in "${alpha}" 'stat -c %u ~/.claude /opt/data /fleet-mounts/files/.claude.json | sort -u')" "the share's folders belong to the remote user"
assert_equals "/fleet-mounts/files/.claude.json" "$(run_in "${alpha}" 'readlink ~/.claude.json')" "~/.claude.json links into the shared files"
assert_equals "{}" "$(run_in "${alpha}" 'cat ~/.claude.json')" "~/.claude.json is the '{}' seed, never empty"
if run_in "${alpha}" 'ls /opt/fleet/run/sync/share' >/dev/null 2>&1; then
  fail "the remote user reaches the share's root (its sync state) past their own folders"
fi
mounts=$(run_in "${alpha}" 'cat /proc/self/mountinfo')
assert_contains "${mounts}" " /home/node/.claude " "~/.claude is a mount of its own"

info "the share outlives an instance: the next one finds the fleet's folders"
"${FLEET_BIN}" down "${alpha}" >/dev/null
[ -n "$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims share)" ] || fail "the share went with an instance"
fleet_up gamma
assert_equals "from-alpha" "$(run_in "${gamma}" 'cat ~/.claude/shared.txt')" "gamma finds what alpha left"
assert_equals "$(node_of "${beta}")" "$(node_of "${gamma}")" "a later instance runs on the share's node too"

pass "fleet share: one per fleet, placed by its volume, one inode, laid out, kept"
