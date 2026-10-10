#!/usr/bin/env bash
# itest: no-docker
# itest: k8s
# Description: TUI Settings → Kubernetes: the picker reads another kubeconfig on fleetd's host (ctrl+o) and connects a failing context anyway, the editor saves Allow privileged and Extra PATH, [ delete ] disconnects; on a reachable cluster the picker creates the namespace and connects it ready, and a baseline label shows it limited
set -euo pipefail

source "$(dirname "$0")/../common.sh"

kube_ctx=""
kube_ns=""
scratch=""
itest_cleanup() {
  tui_kill
  if [ -n "${kube_ns}" ]; then
    kubectl --context "${kube_ctx}" delete namespace "${kube_ns}" --wait=false >/dev/null 2>&1 || true
  fi
  [ -z "${scratch}" ] || rm -rf "${scratch}"
}
itest_begin

# A fresh ~/.fleet, as setup_test leaves it, but without setup_test's backend
# step: on a kubernetes run that registers the cluster under test, and this
# test connects it through the TUI. It needs no fleet and no instance.
rm -rf "${HOME}/.fleet"
mkdir -p "${HOME}/.fleet"

scratch=$(mktemp -d)
kubeconfig="${scratch}/kubeconfig"
itest_fake_kubeconfig "${kubeconfig}"

tui_spawn
tui_wait_for "No instances" 15
tui_send Enter
tui_wait_for "Tmux vim keys" 10
tui_wait_for "+ Kubernetes Cluster" 5
tui_assert_contains "pods/exec, pod create or statefulset patch in a namespace = inside your instances" \
  "the trust line must name the verbs that reach the instances"

info "ctrl+o in the picker reads another kubeconfig on fleetd's host"
tui_cursor_to '\+ Kubernetes Cluster'
tui_send Enter
tui_wait_for "Connect a Kubernetes cluster" 10
tui_send C-o
sleep 0.3
tui_send C-u
tui_send_text "${kubeconfig}"
tui_send Enter
tui_wait_for "itest-plugin" 15
tui_assert_contains "${kubeconfig}" "the picker must name the kubeconfig it read"
tui_wait_for "exec: itest-login-plugin" 5

info "test & connect: the unreachable context fails its test, then connects anyway"
tui_send Enter
tui_wait_for "unreachable" 60
tui_wait_for "connect anyway" 5
tui_send Enter
tui_wait_for_absent "Connect a Kubernetes cluster" 20
tui_wait_for "☸ itest-dead" 5
[ "$(itest_kube_clusters 'print scalar @c')" = 1 ] || fail "fleetd should hold one connection: $(cat "${HOME}/.fleet/kube_clusters.json")"
[ "$(itest_kube_clusters 'print "$c[0]{context} $c[0]{namespace} $c[0]{kubeconfig_paths}[0]"')" = "itest-dead team ${kubeconfig}" ] \
  || fail "the connection must pin the context, its namespace and the file read: $(cat "${HOME}/.fleet/kube_clusters.json")"

info "the editor saves Allow privileged and Extra PATH"
tui_cursor_to '☸ itest-dead'
tui_send Enter
tui_wait_for "Edit cluster ☸ itest-dead" 5
tui_cursor_to 'Allow privileged:'
tui_send Right
tui_wait_for "Privileged pods allowed" 10
[ "$(itest_kube_clusters 'print $c[0]{allow_privileged} ? "on" : "off"')" = on ] || fail "allow_privileged was not saved"
tui_cursor_to '▶ Advanced'
tui_send l
tui_cursor_to 'Extra PATH:'
tui_send Enter
sleep 0.3
tui_send_text "/opt/itest/bin"
tui_send Enter
tui_wait_for "Saved Extra PATH" 10
[ "$(itest_kube_clusters 'print join(":", @{$c[0]{extra_path}})')" = /opt/itest/bin ] || fail "extra_path was not saved"
[ "$(itest_kube_clusters 'print $c[0]{allow_privileged} ? "on" : "off"')" = on ] || fail "saving Extra PATH must leave Allow privileged alone"
tui_send Escape
tui_wait_for_absent "Edit cluster" 5

info "[ delete ] disconnects an unused cluster"
tui_cursor_to '☸ itest-dead'
tui_send l
tui_send l
tui_send Enter
tui_wait_for "Disconnect ☸ itest-dead" 5
tui_assert_contains "No instance or agent uses this cluster" "an unused cluster's dialog"
tui_send Enter
tui_wait_for "Disconnected ☸ itest-dead" 20
[ "$(itest_kube_clusters 'print scalar @c')" = 0 ] || fail "the connection must be gone: $(cat "${HOME}/.fleet/kube_clusters.json")"

# The rest needs a cluster that answers: the kubernetes run's, else the
# current context when there is one (a QA container started with
# FLEET_QA_START_MINIKUBE=1).
kube_ctx="${FLEET_ITEST_CLUSTER:-$(kubectl config current-context 2>/dev/null || true)}"
if [ -z "${kube_ctx}" ] || ! kubectl --context "${kube_ctx}" --request-timeout=10s get --raw=/readyz >/dev/null 2>&1; then
  pass "TUI Settings → Kubernetes: picker, connect anyway, editor saves, disconnect (no reachable cluster: the ready / limited half did not run)"
fi

kube_ns="fleet-it-p1-03-$$"
info "on ${kube_ctx}: create namespace ${kube_ns} from the picker and connect it"
tui_cursor_to '\+ Kubernetes Cluster'
tui_send Enter
tui_wait_for "Connect a Kubernetes cluster" 10
tui_send /
tui_send_text "${kube_ctx}"
tui_send Escape
sleep 0.3
tui_send n
sleep 0.3
tui_send C-u
tui_send_text "${kube_ns}"
tui_send Enter
tui_wait_for "namespace: ${kube_ns}" 5
tui_send Enter
tui_wait_for "namespace '${kube_ns}' does not exist" 180
tui_send c
tui_wait_for "app.kubernetes.io/managed-by: fleet" 20
tui_send Enter
tui_wait_for_absent "Connect a Kubernetes cluster" 240
tui_wait_for "● ready" 180
[ "$(kubectl --context "${kube_ctx}" get namespace "${kube_ns}" -o 'jsonpath={.metadata.labels.app\.kubernetes\.io/managed-by}')" = fleet ] \
  || fail "the namespace must carry exactly the labels the panel showed"

info "a baseline-labelled namespace shows limited"
kubectl --context "${kube_ctx}" label namespace "${kube_ns}" pod-security.kubernetes.io/enforce=baseline >/dev/null
tui_cursor_to "☸ ${kube_ctx:0:7}"
tui_send l
tui_send Enter
tui_wait_for ": limited" 180

info "disconnect it"
tui_send l
tui_send Enter
tui_wait_for "Disconnect ☸" 5
tui_send Enter
tui_wait_for "Disconnected ☸" 30
[ "$(itest_kube_clusters 'print scalar @c')" = 0 ] || fail "the connection must be gone"

pass "TUI Settings → Kubernetes: picker, connect anyway, editor saves, disconnect; ${kube_ctx} connected ready and shows limited under baseline"
