#!/usr/bin/env bash
# csi-hostpath-limit.sh — give a throwaway cluster's csi-hostpath driver a low
# per-node volume attach limit, for the Kubernetes itest of the fleet-size cap
# (825): a node then takes only <limit> of the driver's volumes, as a small
# cloud VM takes few data disks.
#
#   csi-hostpath-limit.sh <context> [limit]   # default limit 3
#
# The cluster needs the csi-hostpath driver (minikube's csi-hostpath-driver
# addon: FLEET_QA_MINIKUBE_FLAGS="... --addons=csi-hostpath-driver" in the QA
# container). The driver's node plugin gets --maxvolumespernode=<limit>,
# which it reports to the kubelet (CSINode allocatable, which the scheduler's
# NodeVolumeLimits enforces); the script waits until every node's CSINode
# says so. It changes the cluster for everyone on it: never run it on a
# shared cluster. Then:
#
#   FLEET_ITEST_BACKEND=kubernetes FLEET_ITEST_CLUSTER=<context> \
#     FLEET_ITEST_RANGE=825-826 ./integration/run.sh
set -euo pipefail

ctx="${1:?usage: $0 <context> [limit]}"
limit="${2:-3}"
driver=hostpath.csi.k8s.io
k() { kubectl --context "${ctx}" "$@"; }

kind=daemonset
if ! k -n kube-system get daemonset csi-hostpathplugin >/dev/null 2>&1; then
  kind=statefulset
  k -n kube-system get statefulset csi-hostpathplugin >/dev/null 2>&1 ||
    { echo "csi-hostpath-limit: no csi-hostpathplugin in kube-system (enable minikube's csi-hostpath-driver addon)" >&2; exit 1; }
fi
args=$(k -n kube-system get "${kind}" csi-hostpathplugin -o 'jsonpath={.spec.template.spec.containers[?(@.name=="hostpath")].args}')
i=$(k -n kube-system get "${kind}" csi-hostpathplugin -o 'jsonpath={range .spec.template.spec.containers[*]}{.name}{"\n"}{end}' | grep -n '^hostpath$' | cut -d: -f1)
[ -n "${i}" ] || { echo "csi-hostpath-limit: the plugin has no hostpath container" >&2; exit 1; }
i=$((i - 1))
case "${args}" in
  *maxvolumespernode*)
    # Replace the limit set before.
    new=$(printf '%s' "${args}" | sed -E "s/--?maxvolumespernode=[0-9]+/--maxvolumespernode=${limit}/")
    k -n kube-system patch "${kind}" csi-hostpathplugin --type=json \
      -p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/${i}/args\",\"value\":${new}}]" >/dev/null ;;
  *)
    k -n kube-system patch "${kind}" csi-hostpathplugin --type=json \
      -p "[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/${i}/args/-\",\"value\":\"--maxvolumespernode=${limit}\"}]" >/dev/null ;;
esac
k -n kube-system rollout status "${kind}" csi-hostpathplugin --timeout=300s >/dev/null
for _ in $(seq 1 90); do
  # A node plugin re-registering leaves its CSINode without drivers a while.
  counts=$(k get csinode -o go-template="{{range .items}}{{range .spec.drivers}}{{if eq .name \"${driver}\"}}{{if .allocatable}}{{.allocatable.count}}{{end}}{{\"\\n\"}}{{end}}{{end}}{{end}}" 2>/dev/null | grep . | sort -u || true)
  [ "${counts}" = "${limit}" ] && { echo "${ctx}"; exit 0; }
  sleep 2
done
echo "csi-hostpath-limit: CSINode allocatable for ${driver} is '${counts}', not ${limit}" >&2
exit 1
