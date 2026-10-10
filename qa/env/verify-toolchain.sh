#!/usr/bin/env bash
# Reports the version of every tool in the fleet QA toolchain and asserts each
# one is present. Uses:
#   --no-daemon : image build time — fail the build if a tool is missing
#                 (dockerd isn't running yet, so skip the daemon check).
#   --k8s       : also require a Ready cluster in the current kubectl context
#                 (implied by FLEET_QA_START_MINIKUBE=1).
#   --smoke     : also prove Docker-in-Docker works by running hello-world, and
#                 with a cluster that its kube-dns pods run (for local/manual
#                 use — CI never runs this privileged image).
#   (no args)   : by hand inside the container, to sanity-check the environment.
set -uo pipefail

CHECK_DAEMON=1
CHECK_K8S=0
[ "${FLEET_QA_START_MINIKUBE:-0}" = "1" ] && CHECK_K8S=1
SMOKE=0
for arg in "$@"; do
  case "${arg}" in
    --no-daemon) CHECK_DAEMON=0 ;;
    --k8s)       CHECK_K8S=1 ;;
    --smoke)     SMOKE=1 ;;
    -h|--help)   sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown arg: ${arg}" >&2; exit 2 ;;
  esac
done
# --no-daemon is the image build, where no cluster runs either.
[ "${CHECK_DAEMON}" -eq 1 ] || CHECK_K8S=0

fail=0

# report NAME CMD [ARGS...] — print NAME + first line of `CMD ARGS`, or mark it
# failed. Runs the full command and checks its exit status, so a missing binary
# OR a missing subcommand/plugin (e.g. `docker buildx` when the plugin isn't
# installed) is caught — not just whether the first word is on PATH.
report() {
  local name="$1"; shift
  local out
  if ! out="$("$@" 2>&1)"; then
    printf '  %-20s MISSING/FAILED\n' "${name}"
    fail=1
    return
  fi
  printf '  %-20s %s\n' "${name}" "$(printf '%s\n' "${out}" | head -n1)"
}

# dns_ready SECS — wait up to SECS for the cluster's CoreDNS pods to exist (a
# cluster that just came up may not have created them yet) and be Ready.
dns_ready() {
  local deadline=$((SECONDS + $1))
  until kubectl -n kube-system get pod -l k8s-app=kube-dns -o name 2>/dev/null | grep -q .; do
    [ "${SECONDS}" -lt "${deadline}" ] || return 1
    sleep 2
  done
  local left=$((deadline - SECONDS))
  [ "${left}" -gt 0 ] || left=1
  kubectl -n kube-system wait --for=condition=Ready pod -l k8s-app=kube-dns --timeout="${left}s" >/dev/null 2>&1
}

echo "fleet QA toolchain:"
report go                  go version
report git                 git --version
report docker              docker --version
report dockerd             dockerd --version
report "docker buildx"     docker buildx version
report "docker compose"    docker compose version
report node                node --version
report npm                 npm --version
report devcontainer        devcontainer --version
report tmux                tmux -V
report golangci-lint       golangci-lint version
report buf                 buf --version
report protoc-gen-go       protoc-gen-go --version
report protoc-gen-go-grpc  protoc-gen-go-grpc --version
report gh                  gh --version
report jq                  jq --version
report rg                  rg --version
report python3             python3 --version
report kubectl             kubectl version --client
report minikube            minikube version --short
report kind                kind version
report k3s                 k3s --version

if [ "${CHECK_DAEMON}" -eq 1 ]; then
  echo "docker daemon:"
  if docker info >/dev/null 2>&1; then
    echo "  reachable"
  else
    echo "  UNREACHABLE"
    fail=1
  fi
fi

if [ "${CHECK_K8S}" -eq 1 ]; then
  echo "kubernetes cluster:"
  ctx="$(kubectl config current-context 2>/dev/null)"
  if [ -n "${ctx}" ] && kubectl wait --for=condition=Ready node --all --timeout=10s >/dev/null 2>&1; then
    echo "  ${ctx}: $(kubectl version 2>/dev/null | awk '/^Server Version:/{print $3}'), nodes Ready"
  else
    echo "  UNREACHABLE or not Ready (context: ${ctx:-none})"
    fail=1
  fi
fi

if [ "${SMOKE}" -eq 1 ]; then
  echo "smoke: docker run hello-world"
  if docker run --rm hello-world >/dev/null 2>&1; then
    echo "  ok"
  else
    echo "  FAILED"
    fail=1
  fi
  if [ "${CHECK_K8S}" -eq 1 ]; then
    # kube-dns proves scheduling, the CNI and image pulls end to end; minikube,
    # kind and k3s all label their CoreDNS pods this way.
    echo "smoke: kube-dns pods Ready"
    if dns_ready 120; then
      echo "  ok"
    else
      echo "  FAILED"
      fail=1
    fi
  fi
fi

if [ "${fail}" -ne 0 ]; then
  echo "toolchain verification FAILED" >&2
  exit 1
fi
echo "toolchain OK"
