# shellcheck shell=bash
# Kubernetes side of the backend-neutral helpers in common.sh.
#
# common.sh sources this file when FLEET_ITEST_BACKEND=kubernetes. It defines
# the same _be_* functions as devcontainer.sh. FLEET_ITEST_CLUSTER is the
# kubeconfig context the run targets (run.sh defaults it to the current one).
#
# Only the preflight works so far. Every other function fails the calling test
# with what is missing and the unit that wires it: P2.13 (instances on a
# cluster), P3.0 (control sockets in the pod) and P4.6 (share sync).

# _be_not_yet <what> <unit> — fail the calling test: <what> has no kubernetes
# implementation yet. It exits rather than calling fail, so a call inside $(...)
# still ends the test with a single result row (via the caller's set -e).
_be_not_yet() {
  printf '%b[fail]%b  FLEET_ITEST_BACKEND=kubernetes: %s is not wired yet (%s)\n' \
    "${C_RED}" "${C_RESET}" "$1" "$2" >&2
  exit 1
}

# _be_preflight — run.sh's check that the target cluster answers. On failure
# it prints a [fatal] line and returns 1.
_be_preflight() {
  if ! command -v kubectl >/dev/null 2>&1; then
    _be_fatal "kubectl not on PATH"
    return 1
  fi
  if [ -z "${FLEET_ITEST_CLUSTER:-}" ]; then
    FLEET_ITEST_CLUSTER=$(kubectl config current-context 2>/dev/null || true)
  fi
  if [ -z "${FLEET_ITEST_CLUSTER}" ]; then
    _be_fatal "FLEET_ITEST_CLUSTER is unset and kubectl has no current context"
    return 1
  fi
  local contexts
  contexts=$(kubectl config get-contexts -o name 2>/dev/null || true)
  if ! printf '%s\n' "${contexts}" | grep -qxF -- "${FLEET_ITEST_CLUSTER}"; then
    _be_fatal "no kubeconfig context named ${FLEET_ITEST_CLUSTER} (FLEET_ITEST_CLUSTER)"
    return 1
  fi
  if ! kubectl --context "${FLEET_ITEST_CLUSTER}" --request-timeout=15s get --raw=/readyz >/dev/null 2>&1; then
    _be_fatal "cluster ${FLEET_ITEST_CLUSTER} is not ready (kubectl get --raw=/readyz failed)"
    return 1
  fi
  export FLEET_ITEST_CLUSTER
}

_be_fatal() {
  printf '%b[fatal]%b %s\n' "${C_RED}" "${C_RESET}" "$1" >&2
}

# P2.13: register FLEET_ITEST_CLUSTER with the fresh ~/.fleet and make it the
# default deploy target, so every `fleet up` in a test lands on the cluster.
_be_setup() { _be_not_yet "setup_test (provisioning on ${FLEET_ITEST_CLUSTER:-the cluster})" P2.13; }

# P2.13: delete what is left by label (managed-by=fleet plus this run's owner
# and cluster), the backstop for a test that crashed before `fleet destroy`.
# Teardown runs from run.sh, so this must never exit.
_be_teardown() {
  info "kubernetes teardown backstop not wired yet (P2.13); relying on fleet destroy"
}

# --- containers: the instance's StatefulSet and pod (P2.13) ---

_be_container_status()     { _be_not_yet itest_container_status P2.13; }
_be_container_exists()     { _be_not_yet itest_container_exists P2.13; }
_be_container_belongs_to() { _be_not_yet itest_container_belongs_to P2.13; }
_be_container_leftovers()  { _be_not_yet itest_container_leftovers P2.13; }
_be_container_logs()       { _be_not_yet itest_container_logs P2.13; }
_be_container_stop()       { _be_not_yet itest_container_stop P2.13; }
_be_container_exec_as()    { _be_not_yet itest_container_exec_as P2.13; }

# --- workspace: the checkout on the instance's volume (P2.13) ---

_be_workspace_exists() { _be_not_yet itest_workspace_exists P2.13; }
_be_workspace_test()   { _be_not_yet itest_workspace_test P2.13; }
_be_workspace_read()   { _be_not_yet itest_workspace_read P2.13; }
_be_workspace_write()  { _be_not_yet itest_workspace_write P2.13; }
_be_workspace_git()    { _be_not_yet itest_workspace_git P2.13; }

# --- control dir: /fleet-mounts/control inside the pod (P3.0) ---

_be_control_test() { _be_not_yet itest_control_test P3.0; }
_be_control_read() { _be_not_yet itest_control_read P3.0; }
_be_control_mode() { _be_not_yet itest_control_mode P3.0; }

# --- shared fleet mounts: wait for the share to sync both ways (P4.6) ---

_be_share_sync() { _be_not_yet itest_share_sync P4.6; }
