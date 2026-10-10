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
./integration/run.sh     # full integration suite (needs Docker)
FLEET_BIN=$(which fleet) ./integration/run.sh   # reuse a prebuilt binary
make check-static check-nok8s      # dist/ binaries static; no client-go in fleet-pod
make startup-budget BASE_REF=main  # cold start + RSS budgets vs main
```

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
