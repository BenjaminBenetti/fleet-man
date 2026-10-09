# Development

Command reference for building and testing fleet-man. The devcontainer ships the
full toolchain (Go, Docker, buf, golangci-lint, and minikube/kubectl/kubectx/kubens/k9s).

## Build

```bash
make build                          # go build ./...
go build -o ./bin/fleet ./cmd/fleet # build just the fleet binary
```

## Test

```bash
make test                # go test ./...  (unit)
go vet ./...
golangci-lint run ./...  # client/server import boundary (depguard)
make mod-test            # fleet status mod: claude plugin validate + test (needs claude)
./integration/run.sh     # full integration suite (needs Docker)
FLEET_BIN=$(which fleet) ./integration/run.sh   # reuse a prebuilt binary
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
