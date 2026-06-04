# fleet-man Makefile.
#
# Most of the build is plain `go build ./...`. This Makefile wraps the protobuf
# codegen (generated *.pb.go are checked in, so CI does not run it) and the
# import-boundary lint. The tools these targets need (buf, golangci-lint, the
# protoc-gen-go plugins) are installed by the devcontainer — see
# .devcontainer/setup.sh.

GOBIN := $(shell go env GOPATH)/bin

# Pinned to match go.mod's protobuf/grpc versions.
PROTOC_GEN_GO_VERSION      := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

.PHONY: proto proto-check lint build test

## proto: regenerate the fleetgrpc Go stubs from the .proto contract.
## Requires `buf` on PATH (installed by the devcontainer).
proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	PATH="$(GOBIN):$$PATH" buf generate fleetgrpc

## proto-check: lint + compile the contract without generating.
proto-check:
	PATH="$(GOBIN):$$PATH" buf lint fleetgrpc
	PATH="$(GOBIN):$$PATH" buf build fleetgrpc

## lint: enforce the client/server import boundary (depguard, .golangci.yml).
## This is the same check CI runs; keeping it here lets `make test` catch a
## boundary violation locally before it reaches a PR.
lint:
	PATH="$(GOBIN):$$PATH" golangci-lint run ./...

build:
	go build ./...

## test: run the import-boundary lint, then the unit tests — mirroring CI.
test: lint
	go test ./...
