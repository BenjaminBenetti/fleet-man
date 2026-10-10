# Development

Command reference for building and testing fleet-man. The devcontainer ships the
full toolchain (Go, Docker, buf, golangci-lint, and minikube/kubectl/kubectx/kubens/k9s).

## Build

```bash
make build                          # go build ./...
go build -o ./bin/fleet ./cmd/fleet # build just the fleet binary
make dist VERSION=v1.2.3            # release binaries in dist/ (static linux, darwin, fleet-pod)
make pod-binaries                   # fleet-pod + static tmux per arch into ~/.fleet/bin (docker buildx)
```

Every linux release binary is static (`CGO_ENABLED=0`). `fleet-pod` is the
`-tags nok8s` build for Kubernetes pods, without client-go. The tmux bundle builds
natively per arch, so a foreign arch needs QEMU binfmt; without it, pass
`POD_ARCHES=amd64` (or your host arch).

## Test

```bash
make test                # go test ./...  (unit)
go vet ./...
golangci-lint run ./...  # client/server import boundary (depguard)
make mod-test            # fleet status mod: claude plugin validate + test (needs claude)
make check-static check-nok8s      # dist/ binaries static; no client-go in fleet-pod
make startup-budget BASE_REF=main  # cold start + RSS budgets vs main
```

## Integration tests

`integration/run.sh` drives the real binary against real devcontainers and runs
the TUI in tmux (see `integration/README.md`). On a disposable machine, such as
a CI runner, run it directly:

```bash
./integration/run.sh                            # build, then run every test
FLEET_BIN=/path/to/fleet ./integration/run.sh   # reuse a prebuilt binary
```

Inside a fleet instance, which this devcontainer usually is, never run it
directly. The TUI tests bind, unbind and kill on the default tmux server, the
one hosting your own sessions, and the tests create containers and spawn
daemons on your Docker host. Run it in a throwaway QA container
([`qa/env`](qa/env/README.md)) instead, which has its own Docker daemon and tmux:

```bash
CGO_ENABLED=0 go build -o /tmp/fleet-itest/fleet ./cmd/fleet
docker run --rm --privileged --name "fleet-itest-$RANDOM" \
  -v "$PWD":/src:ro -v /tmp/fleet-itest/fleet:/usr/local/bin/fleet-under-test:ro \
  ghcr.io/benjaminbenetti/fleet-man/qa:latest \
  bash -lc 'cp -a /src /tmp/repo && mkdir -p /tmp/fleet-bin &&
    cp /usr/local/bin/fleet-under-test /tmp/fleet-bin/fleet && cd /tmp/repo &&
    FLEET_BIN=/tmp/fleet-bin/fleet FLEET_ITEST_RANGE=0-100 ./integration/run.sh'
```

- The checkout is mounted read-only and copied, so nothing writes into it. The
  binary is copied to a path the qa user owns because `613_server_dev_restart`
  touches it. `CGO_ENABLED=0` keeps it independent of the image's libc.
- `FLEET_ITEST_RANGE=START-END` keeps the tests whose numeric prefix is in
  `[START, END)` (drop it to run everything); `FLEET_ITEST_SKIP` is a
  comma-separated list of test basenames to skip.
- Shard a full run by starting several containers at once, each with its own
  range and `--name`.
- The image tag is `qa:latest` on main and `qa:feat-kubernetes` for work on that
  branch. After changing `qa/env/`, build and use your own:
  `docker build -t fleet-qa qa/env`.

## Protobuf (`fleetgrpc`)

Generated `*.pb.go` are checked in; regenerate only when the `.proto` contract changes.

```bash
make proto        # regenerate the Go stubs (needs buf)
make proto-check  # lint + compile the contract, no codegen
```

## Kubernetes (minikube)

The cluster is off by default to save memory. Once on, `minikube` is the current
kubectl context.

```bash
.devcontainer/minikube.sh on    # start/resume (extra args go to `minikube start`)
.devcontainer/minikube.sh off   # stop, keeping state
.devcontainer/minikube.sh status
```

### Dev loop

Unit tests never need a cluster. Use the devcontainer's minikube for hands-on
work, and a private cluster in a QA container for integration tests:

1. Turn the cluster on and work in a namespace of your own, deleting it when
   you're done:

   ```bash
   .devcontainer/minikube.sh on
   kubectl create namespace fleet-dev-$USER && kubens fleet-dev-$USER
   k9s                                  # watch pods while you work
   kubectl delete namespace fleet-dev-$USER
   ```

2. Give integration tests that need a cluster a private one: add
   `FLEET_QA_START_MINIKUBE=1` to the QA container recipe above. Its entrypoint
   starts a minikube on the container's own Docker daemon and waits for the
   node, so tests never touch the dev cluster or each other:

   ```bash
   docker run --rm --privileged --name "fleet-itest-$RANDOM" \
     -e FLEET_QA_START_MINIKUBE=1 -e FLEET_QA_MINIKUBE_FLAGS="--memory=4g --cpus=2" \
     -v fleet-qa-minikube:/home/qa/.minikube/cache \
     ...                                # mounts, image and command as above
   ```

   The cluster adds about a minute to startup. The `fleet-qa-minikube` volume
   keeps minikube's ~900 MB of downloads between runs; warm it with one run
   before starting several containers at once, and cap each one's cluster with
   `FLEET_QA_MINIKUBE_FLAGS`. To look inside a running one:
   `docker exec -it <name> kubectl get pods -A`.

The QA image pins kubectl, minikube, kind and k3s; `qa/env/README.md` covers
multi-node kind and k3s clusters.
