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

# Docker-in-Docker fix: The container may default to iptables-legacy, but the host
# kernel might only support nf_tables. Detect the mismatch and switch if needed.
#
# We check: is iptables currently set to legacy mode AND does the kernel have nf_tables
# loaded? If so, the legacy backend will fail to initialize and dockerd won't start.
needs_nft_fix() {
  # No update-alternatives or no nft binary → nothing we can do
  command -v update-alternatives >/dev/null 2>&1 || return 1
  [ -x /usr/sbin/iptables-nft ] || return 1

  # Already using nft → no fix needed
  current=$(update-alternatives --query iptables 2>/dev/null | awk '/^Value:/{print $2}')
  [ "$current" = "/usr/sbin/iptables-nft" ] && return 1

  # Check if the kernel is using nf_tables (the legacy backend won't work in this case)
  if [ -d /sys/module/nf_tables ] || grep -q nf_tables /proc/modules 2>/dev/null; then
    return 0
  fi

  # If we can't tell, try legacy iptables and see if it actually works
  if ! /usr/sbin/iptables-legacy -L -n >/dev/null 2>&1; then
    return 0
  fi

  return 1
}

if needs_nft_fix; then
  echo "Host kernel uses nf_tables but container has iptables-legacy — switching to nft backend"
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