#!/usr/bin/env bash
# itest: k8s
# Description: a change of the cluster's shared StorageClass moves a stopped fleet's share: while an instance runs the fleet is listed waiting (fleet cluster ls), once all are stopped a copy Job moves the share to the new class (same name, the old volume gone, nothing left of the move), and the instances start on it with every file, link, mode and owner kept, placed for the new class's mode (multi-node: no share-holder affinity, Codex's SQLite on each pod's disk). Needs a WaitForFirstConsumer or single-node default class and a second class (FLEET_ITEST_SHARE_TO_CLASS, else an nfs.csi.k8s.io one, fleet-shared-path or a hostpath.csi.k8s.io one); elsewhere it passes as not run.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a fleet share moving to another StorageClass"

default_class=$(itest_k8s_kubectl get storageclass -o go-template='{{range .items}}{{if eq (index .metadata.annotations "storageclass.kubernetes.io/is-default-class") "true"}}{{.metadata.name}}{{end}}{{end}}')
[ -n "${default_class}" ] || pass "not run: the cluster has no default StorageClass"
class_of() { itest_k8s_kubectl get storageclass -o go-template="{{range .items}}{{if eq .provisioner \"$1\"}}{{.metadata.name}}{{\"\\n\"}}{{end}}{{end}}" | head -1; }
to="${FLEET_ITEST_SHARE_TO_CLASS:-}"
[ -n "${to}" ] || to=$(class_of nfs.csi.k8s.io)
[ -n "${to}" ] || to=$(itest_k8s_kubectl get storageclass fleet-shared-path -o 'jsonpath={.metadata.name}' 2>/dev/null || true)
[ -n "${to}" ] || to=$(class_of hostpath.csi.k8s.io)
[ -n "${to}" ] && [ "${to}" != "${default_class}" ] || pass "not run: no second StorageClass to move a share to (FLEET_ITEST_SHARE_TO_CLASS)"

# Moves are looked at every few seconds (fleetd inherits the environment).
export FLEET_KUBE_SHARE_MOVE_INTERVAL=5s
setup_agent_test
"${FLEET_BIN}" cluster set itest shared_storage_class="${default_class}" storage_class="${default_class}" >/dev/null
seed_fleet_settings "${FIXTURE_REPO_NAME}" true true /home/node
alpha="${FIXTURE_REPO_NAME}/alpha"
beta="${FIXTURE_REPO_NAME}/beta"
run_in() { "${FLEET_BIN}" exec "$1" -- sh -c "$2"; }
share_class() { itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.spec.storageClassName}' 2>/dev/null || true; }
moves() { "${FLEET_BIN}" cluster ls itest 2>/dev/null | grep '^share move:' || true; }

info "fleet up alpha and beta, their share in ${default_class}"
fleet_up alpha
fleet_up beta
share=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims share)
[ -n "${share}" ] || fail "the fleet has no share"
assert_equals "${default_class}" "$(share_class)" "the share's class"
old_uid=$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.metadata.uid}')
run_in "${alpha}" 'mkdir -p ~/.claude/deep/er && echo moved > ~/.claude/deep/er/file.txt && chmod 640 ~/.claude/deep/er/file.txt && ln -s deep/er/file.txt ~/.claude/link && echo x > ~/.codex/notes'

info "the shared class becomes ${to}: the fleet waits while beta runs"
"${FLEET_BIN}" cluster set itest shared_storage_class="${to}" >/dev/null
"${FLEET_BIN}" cluster test itest >/dev/null 2>&1 || true
"${FLEET_BIN}" stop "${alpha}" >/dev/null
for _ in $(seq 1 60); do
  moves | grep -q "waiting" && break
  sleep 2
done
assert_contains "$(moves)" "share move: ${FIXTURE_REPO_NAME} ${default_class} → ${to}: waiting" "the fleet is listed waiting"
assert_equals "${default_class}" "$(share_class)" "the share stays while an instance runs"

info "beta stops: the share moves"
"${FLEET_BIN}" stop "${beta}" >/dev/null
for _ in $(seq 1 150); do
  [ "$(share_class)" = "${to}" ] && [ -z "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.metadata.annotations.fleet\.dev/share-move}' 2>/dev/null)" ] && [ -z "$(moves)" ] && break
  sleep 2
done
"${FLEET_BIN}" cluster ls itest || true
assert_equals "${to}" "$(share_class)" "the share is in the new class"
[ "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.metadata.uid}')" != "${old_uid}" ] || fail "the share is the old volume"
assert_equals "" "$(moves)" "nothing listed moving"
[ -z "$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims share-mv)" ] || fail "the move's copy is left"
mode=$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.metadata.annotations.fleet\.dev/share-mode}')
info "the moved share is in ${mode} mode"

info "the fleet starts on the moved share, everything kept"
"${FLEET_BIN}" start "${alpha}" >/dev/null
"${FLEET_BIN}" start "${beta}" >/dev/null
assert_equals "moved" "$(run_in "${beta}" 'cat ~/.claude/deep/er/file.txt')" "a file in a nested folder"
assert_equals "640" "$(run_in "${beta}" 'stat -c %a ~/.claude/deep/er/file.txt')" "its mode"
assert_equals "$(run_in "${alpha}" 'id -u')" "$(run_in "${beta}" 'stat -c %u ~/.claude/deep/er/file.txt')" "its owner"
assert_equals "deep/er/file.txt" "$(run_in "${alpha}" 'readlink ~/.claude/link')" "a symlink, as a link"
assert_equals "x" "$(run_in "${alpha}" 'cat ~/.codex/notes')" "the Codex folder"
run_in "${alpha}" 'echo after > ~/.claude/after.txt'
assert_equals "after" "$(run_in "${beta}" 'cat ~/.claude/after.txt')" "siblings share the moved share"
beta_sts=$(itest_k8s_sts "$(itest_container_id "${beta}")")
case "${mode}" in
  multi-node)
    assert_equals "ReadWriteMany" "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.spec.accessModes[0]}')" "a multi-node share's access"
    assert_equals "" "$(itest_k8s_kubectl get statefulset "${beta_sts}" -o 'jsonpath={.spec.template.spec.affinity.podAffinity}')" "no share-holder affinity left"
    assert_equals "/home/node/.local/state/fleet/codex-sqlite" "$(run_in "${beta}" 'echo "${CODEX_SQLITE_HOME}"')" "Codex's SQLite on the pod's own disk"
    ;;
  *)
    assert_equals "ReadWriteOnce" "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.spec.accessModes[0]}')" "a single-node share's access"
    assert_contains "$(itest_k8s_kubectl get statefulset "${beta_sts}" -o 'jsonpath={.spec.template.spec.affinity.podAffinity}')" "fleet.dev/share-group" "the share-holder affinity kept"
    ;;
esac

pass "a stopped fleet's share moved to the new shared class, everything kept"
