#!/bin/bash
# Kubernetes tooling: minikube, kubectl, kubectx, kubens, k9s.
#
# Each tool is installed once, at its latest upstream release, into
# /usr/local/bin and verified against the published sha256; container restarts
# skip anything already on PATH. A rebuild picks up newer releases.
#
# Installed here rather than via the kubectl-helm-minikube devcontainer feature
# because that feature mounts a host-wide `minikube-config` volume over
# ~/.minikube, which every fleet instance on the host would then share.
#
# The cluster itself is not started here; see .devcontainer/minikube.sh.

set -uo pipefail

case "$(uname -m)" in
  x86_64)        ARCH=amd64 KXARCH=x86_64 ;;
  aarch64|arm64) ARCH=arm64 KXARCH=arm64 ;;
  *) echo "k8s-tools: unsupported arch $(uname -m), skipping" >&2; exit 0 ;;
esac

fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }

# Resolve a repo's latest release tag from the /releases/latest redirect rather
# than the API, which caps unauthenticated callers at 60 requests/hour.
latest_tag() {
  curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$1/releases/latest" | sed 's#.*/tag/##'
}

# check <dir> <file> <sums>: verify dir/file against its line in a sha256sums list.
# An absent entry fails too: sha256sum -c rejects empty input.
check() { (cd "$1" && awk -v f="$2" '$2 == f' "$3" | sha256sum -c --quiet -); }

install_minikube() {
  local url="https://storage.googleapis.com/minikube/releases/latest/minikube-linux-${ARCH}"
  fetch "$url" "$tmp/minikube"
  echo "$(curl -fsSL "$url.sha256")  minikube" | check "$tmp" minikube -
  sudo install -m 0755 "$tmp/minikube" /usr/local/bin/minikube
}

install_kubectl() {
  local v url
  v=$(curl -fsSL https://dl.k8s.io/release/stable.txt)
  url="https://dl.k8s.io/release/${v}/bin/linux/${ARCH}/kubectl"
  fetch "$url" "$tmp/kubectl"
  echo "$(curl -fsSL "$url.sha256")  kubectl" | check "$tmp" kubectl -
  sudo install -m 0755 "$tmp/kubectl" /usr/local/bin/kubectl
}

install_kubectx() {
  local v t f
  v=$(latest_tag ahmetb/kubectx)
  fetch "https://github.com/ahmetb/kubectx/releases/download/${v}/checksums.txt" "$tmp/kubectx.sums"
  for t in kubectx kubens; do
    f="${t}_${v}_linux_${KXARCH}.tar.gz"
    fetch "https://github.com/ahmetb/kubectx/releases/download/${v}/${f}" "$tmp/$f"
    check "$tmp" "$f" kubectx.sums
    tar -xzf "$tmp/$f" -C "$tmp" "$t"
    sudo install -m 0755 "$tmp/$t" "/usr/local/bin/$t"
  done
}

install_k9s() {
  local base="https://github.com/derailed/k9s/releases/latest/download"
  local f="k9s_Linux_${ARCH}.tar.gz"
  fetch "$base/checksums.sha256" "$tmp/k9s.sums"
  fetch "$base/$f" "$tmp/$f"
  check "$tmp" "$f" k9s.sums
  tar -xzf "$tmp/$f" -C "$tmp" k9s
  sudo install -m 0755 "$tmp/k9s" /usr/local/bin/k9s
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# kubectx installs kubens alongside it, so it is guarded on both.
for tool in minikube kubectl kubectx:kubens k9s; do
  missing=
  for b in ${tool//:/ }; do command -v "$b" >/dev/null 2>&1 || missing=1; done
  [ -n "$missing" ] || continue
  echo "k8s-tools: installing ${tool%%:*}"
  # Not `( ... ) || warn`: errexit is ignored inside an || list.
  ( set -e; "install_${tool%%:*}" )
  [ $? -eq 0 ] || echo "k8s-tools: ${tool%%:*} install failed" >&2
done
exit 0
