# fleet-man QA environment

A heavyweight, self-contained container image carrying everything an automated
agent needs to **QA a fleet PR end to end** — build it, lint it, run the unit
suite, and run the integration suite (which drives real devcontainers against a
real Docker daemon).

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
| **golangci-lint** (`v2.11.4`) | the client/server import-boundary lint CI enforces |
| **buf** + **protoc-gen-go** / **protoc-gen-go-grpc** | the `fleetgrpc` proto contract (`make proto` / `make proto-check`) |
| **git**, **gh** | check out PRs, report results |
| general utilities | `make`, `build-essential`, `jq`, `ripgrep`, `python3`, `curl`, `wget`, networking tools |

Versions track the project's devcontainer (`.devcontainer/setup.sh`) and CI
(`.github/workflows/{unit,integration}.yml`) so a QA run matches what a PR
actually sees. The Go version is injected from `go.mod` at build time, so it
never drifts from the repo.

Run `qa-verify-toolchain` inside the container to print every tool's version and
confirm the Docker daemon is reachable.

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
into a shell. Verify, then QA a PR:

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

If a working Docker daemon is already reachable (a mounted host socket), the
entrypoint detects it and does not start the bundled one.

## Building locally

```bash
docker build -t fleet-qa ./qa/env
docker run --rm -it --privileged fleet-qa qa-verify-toolchain --smoke
```

Build args (see the `Dockerfile` for the full list and defaults):

| Arg | Purpose |
|-----|---------|
| `BASE_IMAGE` | base image (default `ubuntu:26.04`) |
| `GO_VERSION` | Go toolchain version (CI passes the value from `go.mod`) |
| `NODE_MAJOR` | Node major version for the devcontainer CLI |
| `GOLANGCI_LINT_VERSION` / `PROTOC_GEN_GO_VERSION` / `PROTOC_GEN_GO_GRPC_VERSION` | pinned tool versions |

## Maintenance

- **Go** auto-tracks `go.mod` (no manual bump needed).
- **golangci-lint / protoc plugins** are pinned here; keep them matching
  `.devcontainer/setup.sh` and `.github/workflows/unit.yml` when those change.
- The weekly scheduled rebuild keeps the base OS and `latest`-pinned tools
  (buf, devcontainer CLI) current; pin those in the `Dockerfile` if you need
  reproducible rebuilds.
