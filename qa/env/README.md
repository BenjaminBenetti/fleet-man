# fleet-man QA environment

A heavyweight, self-contained container image carrying everything an automated
agent needs to **QA a fleet PR end to end** — build it, lint it, run the unit
suite, and run the integration suite (which drives real devcontainers against a
real Docker daemon), plus a throwaway Kubernetes cluster inside the container
for the kubernetes backend.

The image is built for cloud QA agents: spin one up per PR and it has a clean,
isolated environment with its own Docker daemon (Docker-in-Docker), so QA never
depends on — or pollutes — the host.

## What's inside

| Tool | Why fleet needs it |
|------|--------------------|
| **Go** (pinned to `go.mod`) | build `fleet`, run `go test` / `go vet` |
| **Docker** engine + CLI + buildx + compose | integration tests drive a real daemon; runs in-container (DinD) |
| **Node + `@devcontainers/cli`** | fleet's devcontainer backend |
| **tmux** | the TUI integration tests run `fleet` in a headless tmux |
| **openssh-server** | the TUI tests over Armada SSH reach a second user's fleetd through a local `sshd` (not started by default) |
| **golangci-lint** (`v2.11.4`) | the client/server import-boundary lint CI enforces |
| **buf** + **protoc-gen-go** / **protoc-gen-go-grpc** | the `fleetgrpc` proto contract (`make proto` / `make proto-check`) |
| **git**, **gh** | check out PRs, report results |
| **kubectl** (`v1.37.1`), **minikube** (`v1.39.0`), **kind** (`v0.33.0`), **k3s** (`v1.37.1+k3s1`) | the kubernetes backend: a throwaway cluster inside the container (see [Kubernetes](#kubernetes)) |
| general utilities | `make`, `build-essential`, `jq`, `ripgrep`, `python3`, `curl`, `wget`, networking tools |

Versions track the project's devcontainer (`.devcontainer/setup.sh`,
`.devcontainer/k8s-tools.sh`) and CI (`.github/workflows/{unit,integration}.yml`)
so a QA run matches what a PR actually sees. The Go version is injected from
`go.mod` at build time, so it never drifts from the repo.

Run `qa-verify-toolchain` inside the container to print every tool's version and
confirm the Docker daemon is reachable (`--k8s` also requires a Ready cluster).

## Published image

The publish workflow (`.github/workflows/qa-image.yml`) pushes to the GitHub
Container Registry on every change to `qa/env/**` on `main`, on manual dispatch,
and weekly (to absorb base-OS / toolchain security fixes):

```
ghcr.io/benjaminbenetti/fleet-man/qa:latest
ghcr.io/benjaminbenetti/fleet-man/qa:sha-<short-sha>   # immutable, pin this in automation
```

Images are multi-arch (`linux/amd64`, `linux/arm64`). Authentication uses the
workflow's built-in `GITHUB_TOKEN` — no external secrets.

> First publish only: the package is created private. Make it public (or grant
> the puller access) under the repo's **Packages** settings so cloud agents can
> pull it. The `org.opencontainers.image.source` label links the package back to
> this repo.

## Running it

The bundled Docker daemon needs `--privileged` to start:

```bash
docker run --rm -it --privileged \
  ghcr.io/benjaminbenetti/fleet-man/qa:latest
```

The entrypoint starts `dockerd` (Docker-in-Docker), waits for it, then drops you
into a shell. Before that it delegates the container's cgroup v2 controllers to
child cgroups (the moby dind fix), without which nested containers can't take
resource limits and nested Kubernetes nodes don't start. Verify, then QA a PR:

```bash
qa-verify-toolchain                 # confirm the toolchain + daemon

gh repo clone BenjaminBenetti/fleet-man
cd fleet-man
gh pr checkout <PR-number>

go vet ./...                        # static checks (CI runs this as a separate step)
make test                           # import-boundary lint + unit tests
go build -o /tmp/fleet ./cmd/fleet  # build the binary
FLEET_BIN=/tmp/fleet ./integration/run.sh   # full integration suite (needs the daemon)
```

(`gh` needs a token — `GH_TOKEN` / `gh auth login` — for private repos or to
post results.)

### Environment knobs

| Variable | Default | Effect |
|----------|---------|--------|
| `FLEET_QA_START_DOCKER` | `1` | set `0` to skip starting the bundled daemon (e.g. when mounting a host `/var/run/docker.sock`) |
| `FLEET_QA_DOCKERD_TIMEOUT` | `60` | seconds to wait for `dockerd` to become ready |
| `FLEET_QA_START_MINIKUBE` | `0` | set `1` to start a minikube cluster on the daemon and wait for its node to be Ready |
| `FLEET_QA_MINIKUBE_FLAGS` | (empty) | extra `minikube start` flags, e.g. `--memory=8g --cpus=4` |
| `FLEET_QA_MINIKUBE_TIMEOUT` | `180` | seconds to wait for the minikube node to be Ready |

If a working Docker daemon is already reachable (a mounted host socket), the
entrypoint detects it and does not start the bundled one.

## Kubernetes

With `FLEET_QA_START_MINIKUBE=1` the entrypoint runs `minikube start
--driver=docker` on the bundled daemon, makes `minikube` the current kubectl
context and waits for the node, as `.devcontainer/minikube.sh on` does in the
devcontainer. The cluster lives and dies with the container. If it fails to
come up the entrypoint carries on without it, and `qa-verify-toolchain` reports
the cluster unreachable; the `minikube start` output is in
`/tmp/qa-minikube.log`. On a shared daemon (a mounted host socket) it won't
start if the daemon already has a `minikube` container it didn't create, since
`minikube start` would delete that container as abandoned.

```bash
docker run --rm -it --privileged -e FLEET_QA_START_MINIKUBE=1 \
  -v fleet-qa-minikube:/home/qa/.minikube/cache \
  ghcr.io/benjaminbenetti/fleet-man/qa:latest
qa-verify-toolchain --smoke         # toolchain, daemon, node Ready, kube-dns pods Ready
```

Startup takes about a minute. The optional `fleet-qa-minikube` volume keeps
minikube's ~900 MB of downloads (the node image and the preloaded Kubernetes
images) between containers. Warm it with one run before sharing it between
containers started in parallel.

kind and k3s cover what a single minikube node doesn't: kind runs multi-node
clusters, and k3s is a distribution of its own (embedded containerd, no Docker
daemon needed). Start them by hand:

```bash
kind create cluster --wait 3m       # context kind-kind; add --config for more nodes

sudo sh -c 'nohup k3s server --write-kubeconfig-mode=0644 >/tmp/k3s.log 2>&1 &'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml   # node is Ready in ~10s
```

k3s works with `FLEET_QA_START_DOCKER=0` too. Its data lives in the
`/var/lib/rancher/k3s` volume, because its overlayfs snapshotter can't mount on
the container's own overlay root.

## Building locally

```bash
docker build -t fleet-qa ./qa/env
docker run --rm -it --privileged fleet-qa qa-verify-toolchain --smoke
docker run --rm -it --privileged -e FLEET_QA_START_MINIKUBE=1 fleet-qa qa-verify-toolchain --smoke
```

Build args (see the `Dockerfile` for the full list and defaults):

| Arg | Purpose |
|-----|---------|
| `BASE_IMAGE` | base image (default `ubuntu:26.04`) |
| `GO_VERSION` | Go toolchain version (CI passes the value from `go.mod`) |
| `NODE_MAJOR` | Node major version for the devcontainer CLI |
| `GOLANGCI_LINT_VERSION` / `PROTOC_GEN_GO_VERSION` / `PROTOC_GEN_GO_GRPC_VERSION` | pinned tool versions |
| `KUBECTL_VERSION` / `MINIKUBE_VERSION` / `KIND_VERSION` / `K3S_VERSION` | pinned Kubernetes tool versions |

## Maintenance

- **Go** auto-tracks `go.mod` (no manual bump needed).
- **golangci-lint / protoc plugins** are pinned here; keep them matching
  `.devcontainer/setup.sh` and `.github/workflows/unit.yml` when those change.
- **kubectl / minikube / kind / k3s** are pinned here; `.devcontainer/k8s-tools.sh`
  installs the latest releases, so bump these together to what it resolves
  (`minikube version`, `kubectl version --client` in the devcontainer). Keep
  them on one Kubernetes minor: kubectl supports one minor of skew, and the
  minikube and kind releases each fix a default cluster version. Each download
  is checked against the sha256 its release publishes.
- The weekly scheduled rebuild keeps the base OS and `latest`-pinned tools
  (buf, devcontainer CLI) current; pin those in the `Dockerfile` if you need
  reproducible rebuilds.
