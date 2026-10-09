#!/bin/bash

npm install -g @devcontainers/cli

# Build-time toolchain for the Makefile targets:
#   - protoc-gen-go / protoc-gen-go-grpc + buf  -> `make proto` / `make proto-check`
#   - golangci-lint                             -> `make lint` (import boundary)
# Each install is guarded so this stays cheap on container restarts. Versions
# match the pins in the Makefile and .github/workflows/unit.yml so local and CI
# agree. buf installs with GOTOOLCHAIN=auto so a buf release that needs a newer
# Go than this image still builds.
GOBIN="$(go env GOPATH)/bin"
command -v protoc-gen-go      >/dev/null 2>&1 || go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
command -v protoc-gen-go-grpc >/dev/null 2>&1 || go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
command -v buf                >/dev/null 2>&1 || GOTOOLCHAIN=auto go install github.com/bufbuild/buf/cmd/buf@latest
command -v golangci-lint      >/dev/null 2>&1 || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b "${GOBIN}" v2.11.4

# Kubernetes tooling (minikube, kubectl, kubectx, kubens, k9s). Installs only;
# the cluster stays off until .devcontainer/minikube.sh on.
bash "$(dirname "$0")/k8s-tools.sh"

# Docker-in-Docker fix: The container may default to iptables-legacy, but the host
# kernel might not support it. Switch to the nft backend only when legacy is
# actually unusable.
#
# Don't switch just because nf_tables is loaded: most kernels run both, and
# dockerd (started by the DinD entrypoint before this script) may already have
# built its DOCKER chains in legacy. Switching under it strands those chains and
# every later `docker run -p` fails with "No chain/target/match by that name".
needs_nft_fix() {
  # No update-alternatives or no nft binary → nothing we can do
  command -v update-alternatives >/dev/null 2>&1 || return 1
  [ -x /usr/sbin/iptables-nft ] || return 1

  # Already using nft → no fix needed
  current=$(update-alternatives --query iptables 2>/dev/null | awk '/^Value:/{print $2}')
  [ "$current" = "/usr/sbin/iptables-nft" ] && return 1

  # Legacy works for the tables dockerd needs → keep it. Needs root: unprivileged
  # iptables always fails, which would read as "legacy is broken".
  if sudo -n /usr/sbin/iptables-legacy -L -n >/dev/null 2>&1 &&
     sudo -n /usr/sbin/iptables-legacy -t nat -L -n >/dev/null 2>&1; then
    return 1
  fi

  return 0
}

if needs_nft_fix; then
  echo "iptables-legacy is unusable on this kernel — switching to nft backend"
  sudo update-alternatives --set iptables /usr/sbin/iptables-nft >/dev/null 2>&1 || true
  if [ -x /usr/sbin/ip6tables-nft ]; then
    sudo update-alternatives --set ip6tables /usr/sbin/ip6tables-nft >/dev/null 2>&1 || true
  fi
fi

# Start Docker daemon (DinD feature) if it's not already available.
if command -v docker >/dev/null 2>&1; then
  if ! docker info >/dev/null 2>&1; then
    if [ -x /usr/local/share/docker-init.sh ]; then
      sudo /usr/local/share/docker-init.sh /bin/true >/dev/null 2>&1 || true
    fi
  fi
fi