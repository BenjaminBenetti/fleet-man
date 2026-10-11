# shellcheck shell=bash
# Kubernetes side of the backend-neutral helpers in common.sh.
#
# common.sh sources this file when FLEET_ITEST_BACKEND=kubernetes. It defines
# the same _be_* functions as devcontainer.sh. FLEET_ITEST_CLUSTER is the
# kubeconfig context the run targets (run.sh defaults it to the current one),
# FLEET_ITEST_NAMESPACE the namespace every test's connection uses (default
# fleet-itest; created when missing, never deleted).
#
# Each test's setup connects FLEET_ITEST_CLUSTER to the fresh ~/.fleet and
# makes it the remembered deploy target, so `fleet up` without --backend, MCP
# fleet_up and the TUI's New instance dialog all create on the cluster. The
# daemon serves the pods the pod build of fleet named by
# FLEET_POD_BINARY_<ARCH> (run.sh builds one when it builds FLEET_BIN), and,
# for images without tmux, the bundle FLEET_POD_TMUX_<ARCH> names.
#
# Containers are an instance's StatefulSet and its pod, named by the
# container id fleet records: k8s:<connection>/<namespace>/<statefulset>@g<gen>.
# The workspace is the checkout on the instance's disk, and the control dir
# the pod's /fleet-mounts/control, both reached through `fleet exec`. The
# fleet's shared mounts live on its share in the cluster, which fleetd keeps
# converged with the host tree (itest_share_dir); `fleet sync wait` is the
# barrier between a write on one side and a read on the other.

FLEET_ITEST_NAMESPACE="${FLEET_ITEST_NAMESPACE:-fleet-itest}"
export FLEET_ITEST_NAMESPACE

# _be_arch — the daemon host's architecture as Go names it.
_be_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) uname -m ;;
  esac
}

# _be_preflight — run.sh's check that the target cluster answers and the
# daemon has a pod binary to serve. On failure it prints a [fatal] line and
# returns 1.
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
  local pod_var
  pod_var="FLEET_POD_BINARY_$(_be_arch | tr '[:lower:]' '[:upper:]')"
  if [ -z "${!pod_var:-}" ] || [ ! -f "${!pod_var}" ]; then
    _be_fatal "${pod_var} names no file: point it at the pod build of the fleet under test (make fleet-pod)"
    return 1
  fi
  export "${pod_var?}"
}

_be_fatal() {
  printf '%b[fatal]%b %s\n' "${C_RED}" "${C_RESET}" "$1" >&2
}

# _be_kubectl <args...> — kubectl on the run's context.
_be_kubectl() { kubectl --context "${FLEET_ITEST_CLUSTER}" --request-timeout=30s "$@"; }

# _be_setup — connect FLEET_ITEST_CLUSTER (in FLEET_ITEST_NAMESPACE, as
# connection "itest") to the fresh ~/.fleet, make it the remembered deploy
# target, and wait until fleetd judges it usable (ready or limited), so a
# TUI's Deploy row offers it.
_be_setup() {
  if ! _be_kubectl get namespace "${FLEET_ITEST_NAMESPACE}" >/dev/null 2>&1; then
    _be_kubectl create namespace "${FLEET_ITEST_NAMESPACE}" >/dev/null 2>&1 \
      || _be_kubectl get namespace "${FLEET_ITEST_NAMESPACE}" >/dev/null \
      || fail "could not create namespace ${FLEET_ITEST_NAMESPACE} on ${FLEET_ITEST_CLUSTER}"
  fi
  local out fields=()
  # FLEET_ITEST_CLUSTER_FIELDS: connection settings for the run, as `fleet
  # cluster add` takes them (storage_class=csi-hostpath-sc clones by
  # snapshot).
  read -r -a fields <<<"${FLEET_ITEST_CLUSTER_FIELDS:-}"
  out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name itest -n "${FLEET_ITEST_NAMESPACE}" ${fields[@]+"${fields[@]}"} 2>&1) \
    || fail "fleet cluster add ${FLEET_ITEST_CLUSTER}: ${out}"
  local id
  id=$(_be_cluster_id)
  [ -n "${id}" ] || fail "fleet cluster add left no connection in kube_clusters.json: ${out}"
  itest_remember_cluster_target "${id}"
  local deadline=$(( $(date +%s) + $(_scale_timeout 120) )) line=""
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    line=$("${FLEET_BIN}" cluster ls 2>/dev/null | awk '$1 == "itest"' || true)
    case " ${line} " in
      *" ready "*|*" limited "*) return 0 ;;
    esac
    sleep 1
  done
  "${FLEET_BIN}" cluster test itest >&2 || true
  fail "connection itest on ${FLEET_ITEST_CLUSTER} never became usable: ${line}"
}

# _be_config_written — after a test wrote config.json itself: the run's
# connection is the remembered target again, and the daemon setup started
# (to connect it) is stopped, so the next command's daemon reads the config
# at its start as a first daemon would. The connection itself is kept
# (kube_clusters.json).
_be_config_written() {
  local id
  id=$(_be_cluster_id)
  [ -n "${id}" ] || fail "no Kubernetes connection to keep as the deploy target"
  itest_remember_cluster_target "${id}"
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  local deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  while [ "$(server_count)" != "0" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "the daemon setup started would not exit"
    sleep 0.2
  done
}

_be_row_target() { printf '%s\n' "☸ itest"; }

# A cluster instance rebuilds (a new root generation on its disk).
_be_rebuilds() { return 0; }

# itest_remember_cluster_target <connection-id> — make the connection the
# remembered deploy target in ~/.fleet/config.json, keeping every other
# setting.
itest_remember_cluster_target() {
  perl -MJSON::PP -e '
    my ($path, $id) = @ARGV;
    my $cfg = {};
    if (open(my $fh, "<", $path)) {
      local $/;
      my $text = <$fh>;
      $cfg = eval { decode_json($text) } || {};
    }
    $cfg->{default_backend} = "kubernetes";
    $cfg->{default_kube_cluster} = $id;
    open(my $out, ">", "$path.tmp") or die "write $path.tmp: $!";
    print $out JSON::PP->new->canonical->pretty->encode($cfg);
    close $out;
    rename "$path.tmp", $path or die "rename: $!";
  ' "${HOME}/.fleet/config.json" "$1"
}

# _be_cluster_id — the id of the run's connection ("itest") in
# ~/.fleet/kube_clusters.json; empty when there is none.
_be_cluster_id() {
  [ -f "${HOME}/.fleet/kube_clusters.json" ] || return 0
  perl -MJSON::PP -e '
    local $/; my $j = eval { decode_json(<STDIN>) } || {};
    for my $c (@{$j->{clusters} || []}) {
      if (($c->{display_name} // "") eq "itest" || ($c->{name} // "") eq "itest") { print $c->{id}; exit 0 }
    }
    my @c = @{$j->{clusters} || []};
    print $c[0]{id} if @c == 1;
  ' < "${HOME}/.fleet/kube_clusters.json"
}

# _be_teardown — the backstop after `fleet destroy`: delete what is left of
# this test's connection by label (managed-by=fleet plus its fleet.dev/cluster
# id, which is unique to the test), then stop this test's daemons so the next
# test starts with its own. Teardown runs from run.sh, so this never exits.
_be_teardown() {
  local id
  id=$(_be_cluster_id 2>/dev/null || true)
  if [ -n "${id}" ]; then
    local sel="app.kubernetes.io/managed-by=fleet,fleet.dev/cluster=${id}"
    _be_kubectl -n "${FLEET_ITEST_NAMESPACE}" delete statefulsets,jobs,deployments,services,networkpolicies,poddisruptionbudgets,secrets,configmaps,persistentvolumeclaims \
      -l "${sel}" --wait=false >/dev/null 2>&1 || true
    _be_kubectl -n "${FLEET_ITEST_NAMESPACE}" delete pods -l "${sel}" --grace-period=0 --force --wait=false >/dev/null 2>&1 || true
  fi
  # The daemons of this test: a SIGTERM runs their bounded shutdown.
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  local deadline=$(( $(date +%s) + 25 ))
  while [ "$(server_count)" != "0" ] && [ "$(date +%s)" -lt "${deadline}" ]; do
    sleep 0.2
  done
  pkill -9 -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
}

# --- containers: the instance's StatefulSet and pod ---

# _be_cid_parts <container_id> — "<connection> <namespace> <statefulset>" of a
# k8s:<connection>/<namespace>/<statefulset>@g<gen> id; fails on another form.
_be_cid_parts() {
  local id="${1#k8s:}"
  [ "${id}" != "$1" ] || { printf 'not a Kubernetes container id: %s\n' "$1" >&2; return 1; }
  id="${id%@g*}"
  local cluster="${id%%/*}" rest="${id#*/}"
  printf '%s %s %s\n' "${cluster}" "${rest%%/*}" "${rest#*/}"
}

# _be_container_status <container_id> — "running" while its pod runs and is
# ready, "absent" once the StatefulSet is gone or being deleted (a down's
# foreground delete keeps it until its pod has terminated) or runs a later
# root generation than the id names (a rebuild replaced it), "stopped" at 0
# replicas, else the pod's phase (lowercased) or "starting" while it has none
# yet.
_be_container_status() {
  local parts ns sts replicas ready phase gen live_gen deleting
  parts=$(_be_cid_parts "$1") || { echo absent; return 0; }
  read -r _ ns sts <<<"${parts}"
  gen="${1##*@g}"
  if ! replicas=$(_be_kubectl -n "${ns}" get statefulset "${sts}" -o 'jsonpath={.spec.replicas}|{.metadata.annotations.fleet\.dev/rebuild-gen}|{.metadata.deletionTimestamp}' 2>/dev/null); then
    echo absent
    return 0
  fi
  IFS='|' read -r replicas live_gen deleting <<<"${replicas}"
  if [ -n "${deleting:-}" ]; then
    echo absent
    return 0
  fi
  case "${gen}" in
    ''|*[!0-9]*) ;;
    *) if [ -n "${live_gen}" ] && [ "${live_gen}" -gt "${gen}" ]; then echo absent; return 0; fi ;;
  esac
  if [ "${replicas}" = "0" ]; then
    echo stopped
    return 0
  fi
  phase=$(_be_kubectl -n "${ns}" get pod "${sts}-0" -o 'jsonpath={.status.phase}' 2>/dev/null || true)
  ready=$(_be_kubectl -n "${ns}" get pod "${sts}-0" -o 'jsonpath={.status.containerStatuses[?(@.name=="dev")].ready}' 2>/dev/null || true)
  if [ "${phase}" = "Running" ] && [ "${ready}" = "true" ]; then
    echo running
  elif [ -n "${phase}" ]; then
    printf '%s\n' "${phase}" | tr '[:upper:]' '[:lower:]'
  else
    echo starting
  fi
}

# _be_container_exists <container_id> — its StatefulSet is there and not
# being deleted.
_be_container_exists() {
  [ "$(_be_container_status "$1")" != "absent" ]
}

# _be_slug <s> <limit> — names.Slug: lowercase [a-z0-9-], other runs one "-",
# at most limit characters, never empty.
_be_slug() {
  perl -e '
    my ($s, $limit) = @ARGV;
    $limit = 1 if $limit < 1;
    $s = lc $s;
    $s =~ s/[^a-z0-9]+/-/g;
    $s =~ s/^-+//;
    $s =~ s/-+$//;
    if (length($s) > $limit) { $s = substr($s, 0, $limit); $s =~ s/-+$//; }
    $s = "x" if $s eq "";
    print $s;
  ' "$1" "$2"
}

# _be_sts_prefix <ref> — "fi-<fleet>-<instance>-", what the StatefulSet and
# disk of <ref> are named up to their key (names.Instance).
_be_sts_prefix() {
  local f i room
  f=$(_be_slug "${1%%/*}" 16)
  room=$(( 52 - 3 - ${#f} - 8 - 2 ))
  [ "${room}" -lt 24 ] || room=24
  i=$(_be_slug "${1#*/}" "${room}")
  printf 'fi-%s-%s-\n' "${f}" "${i}"
}

# _be_kube_key <ref> — the instance's kube_key in state.json; empty without one.
_be_kube_key() {
  perl -MJSON::PP -e '
    my ($path, $ref) = @ARGV;
    open(my $fh, "<", $path) or exit 0;
    my $st = eval { local $/; decode_json(<$fh>) } or exit 0;
    my ($f, $i) = $ref =~ m{^([^/]+)/(.+)$} or exit 0;
    for my $x (@{ $st->{fleets}{$f}{instances} || [] }) {
      print $x->{kube_key} // "" if ($x->{name} // "") eq $i;
    }
  ' "${HOME}/.fleet/state.json" "$1"
}

# The StatefulSet is <ref>'s when it is named for it and labelled as the
# test's connection's object with the key fleet recorded for <ref>.
_be_container_belongs_to() {  # <container_id> <ref>
  local parts cluster ns sts key labels prefix
  parts=$(_be_cid_parts "$1") || return 1
  read -r cluster ns sts <<<"${parts}"
  prefix=$(_be_sts_prefix "$2")
  key=$(_be_kube_key "$2")
  labels=$(_be_kubectl -n "${ns}" get statefulset "${sts}" \
    -o 'jsonpath={.metadata.labels.app\.kubernetes\.io/managed-by} {.metadata.labels.fleet\.dev/cluster} {.metadata.labels.fleet\.dev/key}' 2>/dev/null || true)
  if [ "${sts#"${prefix}"}" != "${sts}" ] && [ "${labels}" = "fleet ${cluster} ${key}" ]; then
    return 0
  fi
  printf 'statefulset %s: labels [%s], want [fleet %s %s] and a name starting %s\n' "${sts}" "${labels}" "${cluster}" "${key}" "${prefix}" >&2
  return 1
}

# _be_live_names <kinds> — kind/name of the test's connection's objects of
# kinds (kubectl's comma list) that are not being deleted, one per line.
_be_live_names() {
  local id
  id=$(_be_cluster_id)
  [ -n "${id}" ] || return 0
  _be_kubectl -n "${FLEET_ITEST_NAMESPACE}" get "$1" \
    -l "app.kubernetes.io/managed-by=fleet,fleet.dev/cluster=${id}" \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.kind}}/{{.metadata.name}}{{"\n"}}{{end}}{{end}}' 2>/dev/null || true
}

# _be_container_leftovers <ref> — the test's connection's StatefulSets and
# pods named for <ref> and not being deleted, one per line.
_be_container_leftovers() {
  local prefix
  prefix=$(_be_sts_prefix "$1")
  _be_live_names statefulsets,pods | grep -E "/${prefix}[a-z2-7]{8}(-0)?\$" || true
}

# _be_container_logs <container_id> — the dev container's own output, this
# run and (when the pod restarted) the one before.
_be_container_logs() {
  local parts ns sts
  parts=$(_be_cid_parts "$1") || return 1
  read -r _ ns sts <<<"${parts}"
  _be_kubectl -n "${ns}" logs "${sts}-0" -c dev --previous 2>/dev/null || true
  _be_kubectl -n "${ns}" logs "${sts}-0" -c dev 2>&1
}

# _be_container_stop <container_id> — scale it to 0 behind fleet's back and
# wait for the pod to go.
_be_container_stop() {
  local parts ns sts
  parts=$(_be_cid_parts "$1") || return 1
  read -r _ ns sts <<<"${parts}"
  _be_kubectl -n "${ns}" scale statefulset "${sts}" --replicas=0 >/dev/null
  _be_kubectl -n "${ns}" wait --for=delete "pod/${sts}-0" --timeout=120s >/dev/null 2>&1 || true
}

# _be_container_exec_as <container_id> <user> <cmd> [args...] — kubectl exec
# into the dev container (as its own user, root unless the namespace runs
# pods without it), switching to <user> with runuser.
_be_container_exec_as() {
  local parts ns sts user
  parts=$(_be_cid_parts "$1") || return 1
  read -r _ ns sts <<<"${parts}"
  user="$2"
  shift 2
  if [ "${user}" = "root" ] || [ "${user}" = "0" ]; then
    _be_kubectl -n "${ns}" exec -i "${sts}-0" -c dev -- "$@"
  else
    _be_kubectl -n "${ns}" exec -i "${sts}-0" -c dev -- runuser -u "${user}" -- "$@"
  fi
}

# _be_container_label <container_id> <label> — a label `devcontainer up` put
# on the container, as the kubeshim captured it (the spec Secret's
# inspect.json, which set-up's inspect answers with).
_be_container_label() {
  local parts ns sts gen
  parts=$(_be_cid_parts "$1") || return 0
  read -r _ ns sts <<<"${parts}"
  gen="${1##*@g}"
  _be_kubectl -n "${ns}" get secret "${sts}-spec-g${gen}" -o 'jsonpath={.data.inspect\.json}' 2>/dev/null \
    | base64 -d 2>/dev/null \
    | perl -MJSON::PP -e '
        my $c = eval { local $/; decode_json(<STDIN>) } or exit 0;
        my $v = ($c->{labels} || {})->{$ARGV[0]};
        print $v if defined $v;
      ' "$2"
}

# --- workspace: the checkout on the instance's disk, through `fleet exec` ---
#
# `fleet exec` runs in the instance's workspace folder, so relative paths are
# the workspace's. While the instance is created (no container id yet) the
# checkout is the host clone fleet seeds the disk from.

# _be_workspace_exists <ref> — the instance's disk (a PVC named for it on the
# test's connection, not being deleted) or, before it has one, the host
# checkout.
_be_workspace_exists() {
  [ -e "$(itest_host_workspace_dir "$1")" ] && return 0
  local prefix
  prefix=$(_be_sts_prefix "$1")
  _be_live_names persistentvolumeclaims | grep -qE "/${prefix}[a-z2-7]{8}\$"
}

_be_workspace_test() {  # <ref> <test-op> <relpath>
  "${FLEET_BIN}" exec "$1" -- test "$2" "$3"
}

_be_workspace_read() {  # <ref> <relpath>
  "${FLEET_BIN}" exec "$1" -- cat "$2"
}

_be_workspace_write() {  # <ref> <relpath> <content>
  "${FLEET_BIN}" exec "$1" -- sh -c 'printf "%s\n" "$2" > "$1"' sh "$2" "$3"
}

_be_workspace_git() {  # <ref> <git-args...>
  local ref="$1"
  shift
  "${FLEET_BIN}" exec "${ref}" -- git "$@"
}

# --- control dir: /fleet-mounts/control, the pod's own, through `fleet exec` ---
#
# The pod agent keeps it: the in-pod sockets fleetd wants bound and the files
# it keeps there (the status mod's). There is no host side.

_be_control_test() {  # <ref> <test-op> <name>
  "${FLEET_BIN}" exec "$1" -- test "$2" "/fleet-mounts/control/$3"
}

_be_control_read() {  # <ref> <name>
  "${FLEET_BIN}" exec "$1" -- cat "/fleet-mounts/control/$2"
}

_be_control_mode() {  # <ref> <name>
  "${FLEET_BIN}" exec "$1" -- stat -c %a "/fleet-mounts/control/$2"
}

# --- shared fleet mounts: wait for the share to sync both ways ---
#
# FLEET_ITEST_SHARE_SYNC_TIMEOUT bounds the wait (a Go duration; 120s). A
# sync that does not converge fails the test, naming what was behind.

_be_share_sync() {  # <fleet>
  local out
  if ! out=$("${FLEET_BIN}" sync wait "$1" --timeout "${FLEET_ITEST_SHARE_SYNC_TIMEOUT:-120s}" 2>&1); then
    fail "fleet sync wait $1: ${out}"
  fi
}

# --- a fleet's own objects (svcruntime/kubert) ---

# _be_fleet_prefix <fleet> — "fs-<fleet>-", what the fleet's objects on the
# connection are named up to their h8 (names.FleetObject).
_be_fleet_prefix() { printf 'fs-%s-\n' "$(_be_slug "$1" 24)"; }

# itest_k8s_fleet_objects <fleet> [kinds] — the fleet's objects on the test's
# connection (its anchor, Services, NetworkPolicies, Deployments and cache
# volumes by default), kind/name per line, those being deleted included.
itest_k8s_fleet_objects() {
  local id prefix
  id=$(_be_cluster_id)
  prefix=$(_be_fleet_prefix "$1")
  [ -n "${id}" ] || return 0
  itest_k8s_kubectl get "${2:-configmaps,services,networkpolicies,deployments,persistentvolumeclaims}" \
    -l "app.kubernetes.io/managed-by=fleet,fleet.dev/cluster=${id},fleet.dev/fleet" -o name 2>/dev/null \
    | awk -F/ -v p="${prefix}" '
        # fs-<fleet>-<h8>-<suffix>: exactly the h8 and a suffix after the
        # prefix, so fleet "itest" never takes fleet "itest-fleet"'\''s.
        substr($2, 1, length(p)) == p {
          rest = substr($2, length(p) + 1)
          if (index(rest, "-") == 9 && index(substr(rest, 10), "-") == 0) print
        }' || true
}

# itest_k8s_fleet_object <fleet> <kind> <suffix> — the name of one of the
# fleet's objects (suffix: anchor, net, instances, caches, egress, aptcache,
# imgcache, buildkit); empty when there is none.
itest_k8s_fleet_object() {
  itest_k8s_fleet_objects "$1" "$2" | awk -F/ -v s="-$3" 'substr($2, length($2) - length(s) + 1) == s { print $2; exit }'
}

_be_service_suffix() {
  case "$1" in
    buildkit) echo buildkit ;;
    debcache) echo aptcache ;;
    imagecache) echo imgcache ;;
    *) fail "no shared service $1" ;;
  esac
}

_be_service_status() {  # <fleet> <service>
  local d avail
  d=$(itest_k8s_fleet_object "$1" deployments "$(_be_service_suffix "$2")")
  [ -n "${d}" ] || { echo absent; return 0; }
  avail=$(itest_k8s_kubectl get deployment "${d}" -o 'jsonpath={.status.availableReplicas}' 2>/dev/null || true)
  if [ "${avail:-0}" -ge 1 ] 2>/dev/null; then echo running; else echo starting; fi
}

_be_service_remove() {  # <fleet> <service>
  local d
  d=$(itest_k8s_fleet_object "$1" deployments "$(_be_service_suffix "$2")")
  [ -z "${d}" ] || itest_k8s_kubectl delete deployment "${d}" --wait=true >/dev/null 2>&1 || true
}

# --- for the Kubernetes-only tests (8xx) ---
#
# Tests that check what only a cluster has (a pod replaced behind fleet's
# back, the API server cut off, the job journal) reach it through these. On
# devcontainer those tests pass at once, saying they are kubernetes only.

# itest_k8s_kubectl <args...> — kubectl on the run's context and namespace.
itest_k8s_kubectl() { _be_kubectl -n "${FLEET_ITEST_NAMESPACE}" "$@"; }

# itest_k8s_connection — the test's connection id.
itest_k8s_connection() { _be_cluster_id; }

# itest_k8s_key <ref> — the kube_key fleet recorded for <ref>.
itest_k8s_key() { _be_kube_key "$(_itest_ref "$1")"; }

# itest_k8s_key_objects <key> — every object of the test's connection
# labelled with instance key <key>, kind/name per line, those being deleted
# included.
itest_k8s_key_objects() {
  local id
  id=$(_be_cluster_id)
  [ -n "${id}" ] && [ -n "$1" ] || return 0
  itest_k8s_kubectl get statefulsets,jobs,pods,persistentvolumeclaims,secrets,configmaps,services,poddisruptionbudgets,networkpolicies \
    -l "app.kubernetes.io/managed-by=fleet,fleet.dev/cluster=${id},fleet.dev/key=$1" -o name 2>/dev/null || true
  # A clone's snapshots, on a cluster with the API (a CRD).
  itest_k8s_kubectl get volumesnapshots.snapshot.storage.k8s.io \
    -l "app.kubernetes.io/managed-by=fleet,fleet.dev/cluster=${id},fleet.dev/key=$1" -o name 2>/dev/null || true
}

# itest_k8s_wait_no_objects <key> [timeout_s] — wait until no object carries
# instance key <key>; fails the test naming what is left.
itest_k8s_wait_no_objects() {
  local key="$1" left=""
  local deadline=$(( $(date +%s) + $(_scale_timeout "${2:-120}") ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    left=$(itest_k8s_key_objects "${key}")
    [ -n "${left}" ] || return 0
    sleep 2
  done
  fail "objects of key ${key} are left: $(printf '%s' "${left}" | tr '\n' ' ')"
}

# itest_k8s_pod <container_id> — the instance's pod's name.
itest_k8s_pod() {
  local parts sts
  parts=$(_be_cid_parts "$1") || return 1
  read -r _ _ sts <<<"${parts}"
  printf '%s-0\n' "${sts}"
}

# itest_k8s_sts <container_id> — the instance's StatefulSet's name.
itest_k8s_sts() {
  local parts sts
  parts=$(_be_cid_parts "$1") || return 1
  read -r _ _ sts <<<"${parts}"
  printf '%s\n' "${sts}"
}

# itest_k8s_spec_file <container_id> <file> — a file of the generation's spec
# Secret (pid1.json, exec.json, inspect.json, ...).
itest_k8s_spec_file() {
  local parts ns sts gen
  parts=$(_be_cid_parts "$1") || return 1
  read -r _ ns sts <<<"${parts}"
  gen="${1##*@g}"
  _be_kubectl -n "${ns}" get secret "${sts}-spec-g${gen}" -o "jsonpath={.data.${2//./\\.}}" 2>/dev/null | base64 -d 2>/dev/null
}

# itest_k8s_wait_pod_running <pod> [not-uid] [timeout_s] — wait until <pod>
# runs and is ready (and, with not-uid, is another pod than that one); prints
# its uid.
itest_k8s_wait_pod_running() {
  local pod="$1" not="${2:-}" out=""
  local deadline=$(( $(date +%s) + $(_scale_timeout "${3:-180}") ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    out=$(itest_k8s_kubectl get pod "${pod}" -o 'jsonpath={.metadata.uid} {.status.phase} {.status.containerStatuses[?(@.name=="dev")].ready} {.metadata.deletionTimestamp}' 2>/dev/null || true)
    set -- ${out}
    if [ "${2:-}" = "Running" ] && [ "${3:-}" = "true" ] && [ -z "${4:-}" ] && [ "$1" != "${not}" ]; then
      printf '%s\n' "$1"
      return 0
    fi
    sleep 1
  done
  fail "pod ${pod} never ran${not:+ as a new pod} (last: ${out})"
}
