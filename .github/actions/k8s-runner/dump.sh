#!/usr/bin/env bash
# dump.sh <dir> — what a failed Kubernetes job leaves for its artifact: every
# namespace's objects and events, the fleet pods' descriptions and logs, the
# node's, and fleetd's log when one is left.
set -uo pipefail
dir="$1"
mkdir -p "${dir}"
kubectl get all,pvc,secrets,leases,events -A -o wide > "${dir}/objects.txt" 2>&1
kubectl describe pods -A -l app.kubernetes.io/managed-by=fleet > "${dir}/fleet-pods.txt" 2>&1
for pod in $(kubectl get pods -A -l app.kubernetes.io/managed-by=fleet -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null); do
  kubectl logs -n "${pod%%/*}" "${pod#*/}" --all-containers --prefix > "${dir}/log-${pod//\//_}.txt" 2>&1
done
minikube logs --file="${dir}/minikube.txt" >/dev/null 2>&1 || true
cp "${HOME}/.fleet/fleet.log" "${dir}/fleet.log" 2>/dev/null || true
