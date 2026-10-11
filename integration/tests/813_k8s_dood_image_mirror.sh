#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, with the fleet's image cache on, a docker-outside-of-docker instance's own engine (the pod's fleet-docker sidecar, a private engine per pod) pulls Docker Hub images through the fleet's mirror: fleet merges the mirror into the engine's daemon.json (FLEET_DOCKERD_CONFIG), which the sidecar reloads by itself. A second DooD instance's pull of the same image is a mirror hit, served from the mirror's store with nothing fetched from Docker Hub. A stop and start, and a pod the cluster replaces, each start the engine with a fresh config, and fleet points it at the mirror again (on every start, though the cluster's caches need no re-ensure).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the image mirror in the pod's own docker engine"

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-dood" setup_test
cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": { "imageCacheServer": true },
      "instances": []
    }
  }
}
EOF

alpha="${FIXTURE_REPO_NAME}/alpha"
beta="${FIXTURE_REPO_NAME}/beta"
image="busybox:1.36.1"

# engine_mirror <ref> — the registry mirror the instance's engine runs with.
engine_mirror() {
  "${FLEET_BIN}" exec "$1" -- docker info --format '{{range .RegistryConfig.Mirrors}}{{.}} {{end}}' 2>/dev/null | sed 's/ *$//'
}
# wait_mirror <ref> <when> — wait until the instance's engine runs with the
# fleet's mirror.
wait_mirror() {
  local deadline=$(( $(date +%s) + $(_scale_timeout 120) )) got=""
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    got=$(engine_mirror "$1") || got=""
    [ "${got}" = "${mirror}" ] && return 0
    sleep 2
  done
  fail "$1's engine never got the fleet's mirror ${mirror} (${2}; it has '${got}'); its daemon.json: $("${FLEET_BIN}" exec "$1" -- sh -c 'cat "$FLEET_DOCKERD_CONFIG"' 2>&1)"
}
# sidecar_log <ref> — the instance's engine sidecar's output.
sidecar_log() {
  itest_k8s_kubectl logs "$(itest_k8s_pod "$(itest_container_id "$1")")" -c fleet-docker 2>&1 || true
}
# The mirror's log: every blob or manifest it fetches from Docker Hub adds an
# entry to its expiry schedule; one served from its store does not. Its
# access log has a line per blob it served.
mirror_log() { itest_k8s_kubectl logs "deployment/${mirror_deploy}" 2>/dev/null || true; }
fetched() { mirror_log | grep -c 'Adding new scheduler entry for library/busybox@' || true; }
served_blobs() { mirror_log | grep -cE '"GET /v2/library/busybox/blobs/sha256:[0-9a-f]+[^ "]* HTTP/1\.[01]" 200 ' || true; }
# settled_fetched — fetched, once the mirror has stored what it fetched (it
# commits a blob, and logs its entry, just after serving it).
settled_fetched() {
  local before now
  before=$(fetched)
  for _ in $(seq 1 15); do
    sleep 2
    now=$(fetched)
    [ "${now}" = "${before}" ] && break
    before=${now}
  done
  printf '%s\n' "${before}"
}
catalog() { "${FLEET_BIN}" exec "${alpha}" -- curl -fsS "${mirror%/}/v2/_catalog"; }

info "fleet up alpha + beta (docker-outside-of-docker, image cache on)"
fleet_up alpha
fleet_up beta
itest_wait_service "${FIXTURE_REPO_NAME}" imagecache running 240
mirror="http://$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" services imgcache):5000/"
mirror_deploy=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" deployments imgcache)
wait_mirror "${alpha}" "after create"
wait_mirror "${beta}" "after create"

# fleet wrote the mirror into the engine's own daemon.json, root's, and the
# sidecar reloaded the engine itself: no dockerd runs in the dev container.
cfg=$("${FLEET_BIN}" exec "${alpha}" -- printenv FLEET_DOCKERD_CONFIG)
assert_contains "$("${FLEET_BIN}" exec "${alpha}" -- cat "${cfg}")" "${mirror%/}" "the engine's daemon.json (${cfg})"
assert_equals "0" "$("${FLEET_BIN}" exec "${alpha}" -- stat -c %u "${cfg}")" "the engine's daemon.json stays root's"
assert_contains "$(sidecar_log "${alpha}")" "changed; reloading the engine" "the sidecar should reload the engine on fleet's write"
[ -z "$("${FLEET_BIN}" exec "${alpha}" -- sh -c 'for c in /proc/[0-9]*/comm; do [ "$(cat "$c" 2>/dev/null)" = dockerd ] && echo "$c"; done; true')" ] \
  || fail "a dockerd runs in alpha's dev container: the engine should be the sidecar alone"

info "alpha pulls ${image} through the mirror"
pull_out=$("${FLEET_BIN}" exec "${alpha}" -- docker pull "${image}" 2>&1) || fail "docker pull in alpha failed: ${pull_out}"
assert_contains "$(catalog)" "library/busybox" "alpha's pull should go through the fleet's mirror"
fetched_alpha=$(settled_fetched)
[ "${fetched_alpha}" -gt 0 ] || fail "the mirror fetched nothing of ${image} from Docker Hub for alpha"
served_alpha=$(served_blobs)
info "the mirror fetched ${fetched_alpha} blobs/manifests from Docker Hub and served ${served_alpha} blobs"

info "beta pulls the same image: a mirror hit"
pull_out=$("${FLEET_BIN}" exec "${beta}" -- docker pull "${image}" 2>&1) || fail "docker pull in beta failed: ${pull_out}"
assert_equals "$("${FLEET_BIN}" exec "${alpha}" -- docker image inspect --format '{{.Id}}' "${image}")" \
  "$("${FLEET_BIN}" exec "${beta}" -- docker image inspect --format '{{.Id}}' "${image}")" "beta's engine should hold alpha's image"
served_beta=$(served_blobs)
[ "${served_beta}" -gt "${served_alpha}" ] \
  || fail "beta's engine should pull its layers from the mirror (blobs served ${served_alpha} -> ${served_beta})"
assert_equals "${fetched_alpha}" "$(settled_fetched)" "beta's pull should be served from the mirror's store, nothing fetched from Docker Hub"

# A start makes a fresh pod, whose engine starts with an empty config: the
# start points it at the mirror again.
info "fleet stop + start alpha"
"${FLEET_BIN}" stop "${alpha}" >/dev/null
"${FLEET_BIN}" start "${alpha}" >/dev/null
wait_mirror "${alpha}" "after a stop and start"
assert_contains "$(sidecar_log "${alpha}")" "changed; reloading the engine" "the new pod's engine should be reloaded on fleet's write"
pull_out=$("${FLEET_BIN}" exec "${alpha}" -- docker pull hello-world 2>&1) || fail "docker pull after a restart failed: ${pull_out}"
assert_contains "$(catalog)" "library/hello-world" "pulls after a stop and start should go through the fleet's mirror"

# So does a pod the cluster replaces on its own.
beta_pod=$(itest_k8s_pod "$(itest_container_id "${beta}")")
old_uid=$(itest_k8s_wait_pod_running "${beta_pod}")
info "deleting beta's pod ${beta_pod}"
itest_k8s_kubectl delete pod "${beta_pod}" --wait=false >/dev/null
itest_k8s_wait_pod_running "${beta_pod}" "${old_uid}" >/dev/null
wait_mirror "${beta}" "after the cluster replaced its pod"

info "fleet down alpha + beta"
"${FLEET_BIN}" down "${alpha}" >/dev/null
"${FLEET_BIN}" down "${beta}" >/dev/null

pass "the pod's own docker engine pulls through the fleet's mirror, a second instance's pull is a mirror hit, and every start points it there again"
