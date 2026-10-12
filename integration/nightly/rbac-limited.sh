#!/usr/bin/env bash
# rbac-limited.sh — fleet with only the RBAC it asks for: a throwaway
# minikube where fleet's connection is a ServiceAccount token whose Role, in
# one namespace, grants exactly the namespace-scoped permissions the
# preflight requires (internal/k8s/preflight/rbac.go requiredPermissions,
# plus PodDisruptionBudgets) and no cluster-scoped read at all, then the
# lifecycle tests 010-061 and 800 (a create interrupted) through that
# context.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin rbac-limited
nightly_require minikube kubectl docker devcontainer tmux
profile=fleet-rbac
ns=fleet-limited
ctx=fleet-limited
nightly_minikube "${profile}" --memory=6g --cpus=4
trap 'kubectl config delete-context "${ctx}" >/dev/null 2>&1 || true; kubectl config delete-user "${ctx}" >/dev/null 2>&1 || true; nightly_minikube_delete "${profile}"' EXIT
k() { kubectl --context "${profile}" "$@"; }

k create namespace "${ns}" >/dev/null
k -n "${ns}" create serviceaccount fleet >/dev/null
# Mirrors preflight.requiredPermissions (and pdbPermission).
k -n "${ns}" apply -f - >/dev/null <<'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: fleet}
rules:
- {apiGroups: [apps], resources: [statefulsets, deployments], verbs: [create, get, list, watch, patch, delete]}
- {apiGroups: [batch], resources: [jobs], verbs: [create, get, list, watch, patch, delete]}
- {apiGroups: [""], resources: [pods], verbs: [create, get, list, watch, delete]}
- {apiGroups: [""], resources: [pods/exec, pods/portforward], verbs: [create, get]}
- {apiGroups: [""], resources: [pods/log], verbs: [get]}
- {apiGroups: [""], resources: [events, limitranges, resourcequotas], verbs: [get, list]}
- {apiGroups: [apps], resources: [replicasets], verbs: [get, list]}
- {apiGroups: [""], resources: [persistentvolumeclaims, secrets, configmaps, services], verbs: [create, get, list, patch, delete]}
- {apiGroups: [coordination.k8s.io], resources: [leases], verbs: [create, get, update]}
- {apiGroups: [networking.k8s.io], resources: [networkpolicies], verbs: [create, get, list, patch, delete]}
- {apiGroups: [policy], resources: [poddisruptionbudgets], verbs: [create, get, list, patch, delete]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: fleet}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: fleet}
subjects: [{kind: ServiceAccount, name: fleet}]
EOF
token=$(k -n "${ns}" create token fleet --duration=8h)
kubectl config set-credentials "${ctx}" --token="${token}" >/dev/null
kubectl config set-context "${ctx}" --cluster="${profile}" --user="${ctx}" --namespace="${ns}" >/dev/null
# The token reaches what the Role grants, and nothing cluster-wide.
kubectl --context "${ctx}" auth can-i create statefulsets -n "${ns}" >/dev/null || { nightly_say "the limited token cannot create StatefulSets"; exit 2; }
if kubectl --context "${ctx}" auth can-i list nodes >/dev/null 2>&1; then
  nightly_say "the limited token can list nodes: it is not limited"
  exit 2
fi
nightly_note "- connection: ServiceAccount ${ns}/fleet (a token), its Role in ${ns} only"

nightly_itests "${ctx}" 010-062 FLEET_ITEST_NAMESPACE="${ns}" || nightly_dump "${profile}" lifecycle
NIGHTLY_ITEST_LABEL=cluster nightly_itests "${ctx}" 800-801 FLEET_ITEST_NAMESPACE="${ns}" || nightly_dump "${profile}" cluster
nightly_end
