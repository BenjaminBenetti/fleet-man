#!/usr/bin/env bash
# Description: with buildkit_server on, an instance's docker buildx builds on the fleet's shared server by default, so a second instance's build of the same Dockerfile is CACHED; on a cluster only an instance's own client certificate gets it there, and a start gives the instance a fresh one.
# itest: k8s
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-buildx" setup_test
# claude=false codex=false homedir=/home/vscode gh=false buildkit=true
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false true

alpha="${FIXTURE_REPO_NAME}/alpha"
beta="${FIXTURE_REPO_NAME}/beta"

# build <ref> — build the test's Dockerfile in the instance on its default
# builder (no output: the result stays in the build cache), printing the
# build's progress.
build() {
  "${FLEET_BIN}" exec "$1" -- sh -c 'mkdir -p /tmp/bk && cd /tmp/bk &&
    printf "FROM docker.io/library/busybox:1.37.0\nRUN echo shared-cache > /marker && sleep 2\n" > Dockerfile &&
    docker buildx build --progress=plain . 2>&1'
}

info "fleet up alpha + beta (buildkit on, docker CLI + buildx and no daemon in the image)"
fleet_up alpha
fleet_up beta
itest_wait_service "${FIXTURE_REPO_NAME}" buildkit running 240

info "the shared builder is each instance's default"
builders=$("${FLEET_BIN}" exec "${beta}" -- docker buildx ls 2>&1) || fail "docker buildx ls: ${builders}"
assert_contains "${builders}" "fleet-shared*" "fleet-shared should be beta's default builder"

info "alpha builds; beta's build of the same Dockerfile is cached"
out=$(build "${alpha}") || fail "alpha's build failed: ${out}"
out=$(build "${beta}") || fail "beta's build failed: ${out}"
printf '%s\n' "${out}" | grep -E 'RUN echo shared-cache' | head -n 3
printf '%s\n' "${out}" | grep -A2 'RUN echo shared-cache' | grep -q CACHED \
  || fail "beta's RUN step is not CACHED: the instances do not share the fleet's build cache:
${out}"

if itest_backend_is kubernetes; then
  info "without its client certificate beta's builds are refused (mutual TLS)"
  "${FLEET_BIN}" exec "${beta}" -- sh -c 'rm -f "$HOME/.fleet/buildkit/key.pem" "$HOME/.fleet/buildkit/cert.pem"'
  if out=$(build "${beta}"); then
    fail "a build without the instance's certificate went through: ${out}"
  fi

  info "a stop and start gives beta a fresh certificate, and it builds again"
  "${FLEET_BIN}" stop "${beta}"
  "${FLEET_BIN}" start "${beta}"
  out=$(build "${beta}") || fail "beta's build after a start failed: ${out}"
  assert_contains "${out}" "CACHED" "beta's build after its start"
fi

pass "the fleet's instances share one build cache through the shared buildkit server"
