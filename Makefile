# fleet-man Makefile.
#
# Most of the build is plain `go build ./...`; this Makefile only wraps the
# protobuf codegen, which is NOT run by CI (generated *.pb.go are checked in).

GOBIN := $(shell go env GOPATH)/bin

# Pinned to match go.mod's protobuf/grpc versions.
PROTOC_GEN_GO_VERSION      := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

.PHONY: proto proto-check build test

## proto: regenerate the fleetgrpc Go stubs from the .proto contract.
## Requires `buf` on PATH (go install github.com/bufbuild/buf/cmd/buf@latest).
proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	PATH="$(GOBIN):$$PATH" buf generate fleetgrpc

## proto-check: lint + compile the contract without generating.
proto-check:
	PATH="$(GOBIN):$$PATH" buf lint fleetgrpc
	PATH="$(GOBIN):$$PATH" buf build fleetgrpc

build:
	go build ./...

test:
	go test ./...
