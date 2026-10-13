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

### Known flaky tests

Rerun these before chasing them in an unrelated change; fix one when you
can make it fail on purpose. Each failed only under heavy host load.

| Test | Seen | What happens |
| --- | --- | --- |
| `internal/podagent` `TestTakeoverHungHolder` | once, load average ~50 | the SIGSTOPped agent ends with exit 0, not SIGKILL; 60/60 under CPU stress since |
| `internal/mcpbridge` `TestBridgeWithGoSDK` | once | "lost the connection to the fleet daemon" for the call after the restart |
| `internal/server` `TestGitHostKeyStreamRequiresRemoteToken` | on CI | `Send` meets `io.EOF` before the refusal |
| `internal/server` `TestEditorSessionWritesTheConfigAndLogs` | once, `-race` beside ten clusters | the creation log read before its unfinished line was ended |
| `integration/tests/716_deb_cache.sh` (kubernetes) | 2 of 4 runs beside other clusters | `hits 0 -> 0`: the apt detector answered DIRECT |
| `integration/tests/809_k8s_relay_failure_logged.sh` | once | the TUI's tmux server went away after C-c in the log stream |

Tests that hang up a child (`TestAbortHangsUpATTY`, the SIGHUP case of
`TestRecorderDiesWithFleetHoweverFleetDies`) skip when SIGHUP is ignored in
the test process, as under `nohup`: the child inherits it.

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
- `FLEET_ITEST_RETRIES=N` runs a failed test up to N more times; one that
  passes only on a retry passes, and the summary names it flaky. CI sets 1.
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
   node, so tests never touch the dev cluster or each other.
   `FLEET_ITEST_BACKEND=kubernetes` then runs the suite on it: only the tests
   marked `# itest: k8s`, against the current context (`minikube`) unless
   `FLEET_ITEST_CLUSTER` names another (see `integration/README.md`):

   fleetd serves its pods the pod build of the same source, so build that
   beside the binary (in the same tree state) and name it with
   `FLEET_POD_BINARY_<ARCH>`; `FLEET_POD_TMUX_<ARCH>` names the tmux bundle
   (`make pod-tmux`) for images without tmux:

   ```bash
   CGO_ENABLED=0 go build -o /tmp/fleet-itest/fleet ./cmd/fleet
   make fleet-pod POD_ARCHES=amd64 POD_BIN_DIR=/tmp/fleet-itest
   docker run --rm --privileged --name "fleet-itest-$RANDOM" \
     -e FLEET_QA_START_MINIKUBE=1 -e FLEET_QA_MINIKUBE_FLAGS="--memory=6g --cpus=4" \
     -e FLEET_ITEST_BACKEND=kubernetes \
     -e FLEET_POD_BINARY_AMD64=/tmp/fleet-bin/fleet-pod-linux-amd64 \
     -v fleet-qa-minikube:/home/qa/.minikube/cache \
     -v "$PWD":/src:ro -v /tmp/fleet-itest:/usr/local/fleet-bin:ro \
     ghcr.io/benjaminbenetti/fleet-man/qa:feat-kubernetes \
     bash -lc 'cp -a /src /tmp/repo && mkdir -p /tmp/fleet-bin &&
       cp /usr/local/fleet-bin/* /tmp/fleet-bin/ && cd /tmp/repo &&
       FLEET_BIN=/tmp/fleet-bin/fleet ./integration/run.sh'
   ```

   The cluster adds about a minute to startup. The `fleet-qa-minikube` volume
   keeps minikube's ~900 MB of downloads between runs; warm it with one run
   before starting several containers at once, and cap each one's cluster with
   `FLEET_QA_MINIKUBE_FLAGS`. To look inside a running one:
   `docker exec -it <name> kubectl get pods -A`.

   - The two builds must report one version, so make both the same way. In a
     git worktree the copy's `.git` names a folder outside the container, and a
     build there fails to stamp it ("error obtaining VCS status"): build both
     on the host as above, or both in the container with
     `GOFLAGS=-buildvcs=false`.
   - Tests whose images have no tmux (`804_k8s_restricted_bundled_tmux`) need
     `FLEET_POD_TMUX_AMD64` as well: `make pod-tmux POD_ARCHES=amd64
     POD_BIN_DIR=/tmp/fleet-itest`, then
     `-e FLEET_POD_TMUX_AMD64=/tmp/fleet-bin/fleet-tmux-linux-amd64.tar.gz`.
   - `FLEET_ITEST_ARTIFACTS=<dir>` keeps each failed test's `fleet.log`,
     `state.json` and the cluster's objects, events and pod logs there.

### Nightly jobs

`.github/workflows/nightly-k8s.yml` runs `integration/nightly/*.sh`, each of
which makes its own throwaway cluster, runs itests or Go tests on it, leaves
its measurements and failure dumps in `$FLEET_NIGHTLY_OUT/<job>/` and deletes
the cluster (`integration/README.md` "Nightly jobs" lists them). Run one
locally in a QA container without `FLEET_QA_START_MINIKUBE` (the job starts
its own clusters), with the binaries built as above and an output folder:

```bash
docker run --rm --privileged --init --name "fleet-nightly-$RANDOM" \
  -v "$PWD":/src:ro -v /tmp/fleet-itest:/usr/local/fleet-bin:ro \
  -v /tmp/fleet-nightly:/out -e FLEET_NIGHTLY_OUT=/out \
  -v fleet-qa-minikube:/home/qa/.minikube/cache \
  ghcr.io/benjaminbenetti/fleet-man/qa:feat-kubernetes \
  bash -lc 'cp -a /src /tmp/repo && mkdir -p /tmp/fleet-bin &&
    cp /usr/local/fleet-bin/* /tmp/fleet-bin/ && cd /tmp/repo &&
    FLEET_BIN=/tmp/fleet-bin/fleet FLEET_POD_BINARY_AMD64=/tmp/fleet-bin/fleet-pod-linux-amd64 \
    FLEET_POD_TMUX_AMD64=/tmp/fleet-bin/fleet-tmux-linux-amd64.tar.gz \
    ./integration/nightly/kind-multinode.sh'
```

What a job takes: `kind-multinode` three 4 GB nodes, `kind-nfs` two,
`scale 100` a 12 GB node (`scale 300` about 28 GB), the others one 6-8 GB
minikube (`bench` and `netem` measure: run them on a quiet machine), `selinux-k3s` a 6 GB virtual machine (it needs `/dev/kvm` and,
in the QA container, `sudo apt-get install -y qemu-system-x86 qemu-utils
cloud-image-utils` first; mount a folder on disk at `FLEET_NIGHTLY_CACHE`
for the ~600 MB image and at `FLEET_NIGHTLY_VM_DIR` for the VM's disk).
`FLEET_NIGHTLY_KEEP=1` keeps the cluster for a look afterwards.

The QA image pins kubectl, minikube, kind and k3s; `qa/env/README.md` covers
multi-node kind and k3s clusters.

### Live tests against a cluster

Go tests that need a real cluster are gated by an environment variable, so
`go test ./...` stays cluster-free. Each makes its own `fleet-it-*` namespaces
and deletes them:

```bash
FLEET_KUBE_TEST_CONTEXT=minikube go test -count=1 -run Live ./internal/...
FLEET_CONFORMANCE=kubernetes:minikube go test -count=1 ./internal/backend/conformance/
```

`FLEET_KUBE_TEST_IMAGE` picks the dev image the backend's live tests run and
`FLEET_TEST_TMUX_BUNDLE` a tmux bundle for it. The preflight probes and the
cache runtime have gates of their own, each naming a context
(`FLEET_PREFLIGHT_STORAGE_LIVE`, `FLEET_PREFLIGHT_SHARE_LIVE`,
`FLEET_PREFLIGHT_NETWORK_LIVE`, `FLEET_KUBERT_LIVE`; the test's skip message
says what else it needs). The pod seed's and pod-init's root tests skip
unless run as root: `go test -c` the package and run the binary under
`sudo FLEET_PODSEED_REQUIRE_ROOT=1`, which turns a skip into a failure.
`FLEET_PODINIT_DOCKER=1` runs pod-init as PID 1 in a docker container.

fleetd serves pods only a pod build of exactly its own source (`make
fleet-pod`, or `FLEET_POD_BINARY_<ARCH>`): `-trimpath` drops the version a
release daemon checks, so such a build is refused. After `make dist`, `make
check-binstore` checks that each daemon build serves the pod builds beside
it.
