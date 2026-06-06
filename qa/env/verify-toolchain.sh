#!/usr/bin/env bash
# Reports the version of every tool in the fleet QA toolchain and asserts each
# one is present. Three uses:
#   --no-daemon : image build time — fail the build if a tool is missing
#                 (dockerd isn't running yet, so skip the daemon check).
#   --smoke     : CI — also prove Docker-in-Docker works by running hello-world.
#   (no args)   : by hand inside the container, to sanity-check the environment.
set -uo pipefail

CHECK_DAEMON=1
SMOKE=0
for arg in "$@"; do
  case "${arg}" in
    --no-daemon) CHECK_DAEMON=0 ;;
    --smoke)     SMOKE=1 ;;
    -h|--help)   sed -n '2,9p' "$0"; exit 0 ;;
    *) echo "unknown arg: ${arg}" >&2; exit 2 ;;
  esac
done

fail=0

# report NAME CMD [ARGS...] — print NAME + first line of `CMD ARGS`, or MISSING.
report() {
  local name="$1"; shift
  if ! command -v "$1" >/dev/null 2>&1; then
    printf '  %-20s MISSING\n' "${name}"
    fail=1
    return
  fi
  printf '  %-20s %s\n' "${name}" "$("$@" 2>&1 | head -n1)"
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

if [ "${CHECK_DAEMON}" -eq 1 ]; then
  echo "docker daemon:"
  if docker info >/dev/null 2>&1; then
    echo "  reachable"
  else
    echo "  UNREACHABLE"
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
fi

if [ "${fail}" -ne 0 ]; then
  echo "toolchain verification FAILED" >&2
  exit 1
fi
echo "toolchain OK"
