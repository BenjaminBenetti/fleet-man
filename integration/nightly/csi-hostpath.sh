#!/usr/bin/env bash
# csi-hostpath.sh — snapshot clones and the backend conformance suite on two
# storage classes: a throwaway minikube with the volumesnapshots and
# csi-hostpath-driver addons; clones (700, 812), DooD with its rebuild (808)
# and automation (391, 780) on csi-hostpath-sc, where clones are snapshots
# (T1); the conformance suite on standard (copy clones, T2) and on
# csi-hostpath-sc; and the live seed-cache test, which needs a snapshot
# class. The Go tests need Go and the tmux bundle (FLEET_POD_TMUX_<ARCH>).
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin csi-hostpath
nightly_require minikube kubectl docker devcontainer tmux go
profile=fleet-csi-hostpath
nightly_minikube "${profile}" --memory=8g --cpus=4 --addons=volumesnapshots,csi-hostpath-driver
trap 'nightly_minikube_delete "${profile}"' EXIT
kubectl --context "${profile}" get storageclass,volumesnapshotclass >&2

nightly_itests "${profile}" 700-701 FLEET_ITEST_CLUSTER_FIELDS=storage_class=csi-hostpath-sc || nightly_dump "${profile}" clone
NIGHTLY_ITEST_LABEL=dood nightly_itests "${profile}" 808-809 FLEET_ITEST_CLUSTER_FIELDS=storage_class=csi-hostpath-sc || nightly_dump "${profile}" dood
NIGHTLY_ITEST_LABEL=freeze nightly_itests "${profile}" 812-813 FLEET_ITEST_CLUSTER_FIELDS=storage_class=csi-hostpath-sc || nightly_dump "${profile}" freeze
NIGHTLY_ITEST_LABEL=automation nightly_itests "${profile}" 391-392 FLEET_ITEST_CLUSTER_FIELDS=storage_class=csi-hostpath-sc || nightly_dump "${profile}" automation
NIGHTLY_ITEST_LABEL=payloads nightly_itests "${profile}" 780-781 FLEET_ITEST_CLUSTER_FIELDS=storage_class=csi-hostpath-sc || nightly_dump "${profile}" payloads

# go_test <label> <env...> -- <go test args...> — one Go run, its log kept.
go_test() {
  local label="$1" log
  shift
  local envs=()
  while [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  log="${NIGHTLY_OUT}/go-${label}.log"
  nightly_say "go test ${label}"
  if (cd "${REPO_ROOT}" && env "${envs[@]}" go test -count=1 "$@") > "${log}" 2>&1; then
    nightly_note "- go ${label}: $(grep -E '^(ok|PASS)' "${log}" | tail -n 1)"
  else
    tail -n 40 "${log}" >&2
    nightly_fail "go ${label}"
    nightly_dump "${profile}" "go-${label}"
  fi
}
for class in standard csi-hostpath-sc; do
  go_test "conformance-${class}" FLEET_CONFORMANCE="kubernetes:${profile}" FLEET_CONFORMANCE_KUBE_STORAGE_CLASS="${class}" \
    -- -timeout 60m -v ./internal/backend/conformance
done
go_test seed-cache FLEET_KUBE_TEST_CONTEXT="${profile}" -- -timeout 30m -v -run 'TestLiveSeedCache' ./internal/backend/kubernetes
nightly_end
