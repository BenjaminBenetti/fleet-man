# fleet-man Makefile.
#
# Most of the build is plain `go build ./...`. This Makefile wraps the protobuf
# codegen (generated *.pb.go are checked in, so CI does not run it), the
# import-boundary lint, and the release-shape builds and their checks. The tools
# these targets need (buf, golangci-lint, the protoc-gen-go plugins) are
# installed by the devcontainer — see .devcontainer/setup.sh.

GOBIN := $(shell go env GOPATH)/bin

# Pinned to match go.mod's protobuf/grpc versions.
PROTOC_GEN_GO_VERSION      := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

.PHONY: proto proto-check lint build test mod-test
.PHONY: dist fleet-pod pod-tmux pod-binaries check-static check-binstore check-nok8s check-pod-tmux startup-budget

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

## mod-test: validate and test the fleet status mod (a Claude Code mod in
## TypeScript, embedded in the fleet binary). Needs `claude` on PATH.
MOD_DIR := internal/agentstrategy/statusmod
mod-test:
	claude plugin validate $(MOD_DIR)
	claude plugin test $(MOD_DIR)

# --- Release and pod builds -------------------------------------------------
#
# Every linux binary is static (CGO_ENABLED=0), so the same file runs on any
# distro and in any container image, glibc or musl. The pod build (-tags nok8s,
# see internal/buildflavor) is the client-go-free fleet for Kubernetes pods,
# shipped per arch with a static tmux for images that have none.

# VERSION: stamped into internal/version.Version (empty = dev build).
VERSION     ?=
LDFLAGS     := -s -w -X github.com/BenjaminBenetti/fleet-man/internal/version.Version=$(VERSION)
DIST        ?= dist
PLATFORMS   := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
POD_ARCHES  ?= amd64 arm64
# POD_BIN_DIR: where fleet-pod and pod-tmux write; the pod binary cache by default.
POD_BIN_DIR ?= $(HOME)/.fleet/bin
# TMUX_BUILD_FLAGS: extra `docker buildx build` flags for pod-tmux (CI passes cache flags).
TMUX_BUILD_FLAGS ?=
# TMUX_SMOKE_IMAGES: images check-pod-tmux runs the bundle in, beside the build's own bare Alpine.
TMUX_SMOKE_IMAGES ?= debian:bookworm-slim ubuntu:24.04 busybox:musl
BUILDCHECK  := go run ./internal/tools/buildcheck

## dist: release binaries in $(DIST): fleet-<os>-<arch> for each platform and
## fleet-pod-linux-<arch> for each pod arch.
dist:
	@mkdir -p $(DIST)
	@set -e; for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; echo "$(DIST)/fleet-$$os-$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $(DIST)/fleet-$$os-$$arch ./cmd/fleet; \
	done
	@$(MAKE) --no-print-directory fleet-pod POD_BIN_DIR=$(DIST)

## fleet-pod: the pod build, fleet-pod-linux-<arch>, into $(POD_BIN_DIR).
fleet-pod:
	@mkdir -p $(POD_BIN_DIR)
	@set -e; for arch in $(POD_ARCHES); do \
	  echo "$(POD_BIN_DIR)/fleet-pod-linux-$$arch"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -tags nok8s -ldflags "$(LDFLAGS)" -o $(POD_BIN_DIR)/fleet-pod-linux-$$arch ./cmd/fleet; \
	done

## pod-tmux: the static tmux bundle (deploy/tmux), fleet-tmux-linux-<arch>.tar.gz,
## into $(POD_BIN_DIR). Needs docker buildx; a foreign arch needs QEMU binfmt.
pod-tmux:
	@mkdir -p $(POD_BIN_DIR)
	@set -e; for arch in $(POD_ARCHES); do \
	  echo "$(POD_BIN_DIR)/fleet-tmux-linux-$$arch.tar.gz"; \
	  docker buildx build $(TMUX_BUILD_FLAGS) --platform linux/$$arch --target bundle \
	    --output type=local,dest=$(POD_BIN_DIR) deploy/tmux; \
	done

## pod-binaries: fleet-pod + pod-tmux, filling fleetd's pod binary cache.
pod-binaries: fleet-pod pod-tmux

## check-static: every linux binary in $(DIST), and the tmux in each bundle, is
## static; every binary is the build (full or pod) its name says.
check-static:
	$(BUILDCHECK) static $(wildcard $(DIST)/fleet-linux-* $(DIST)/fleet-pod-linux-* $(DIST)/fleet-tmux-linux-*.tar.gz)
	$(BUILDCHECK) flavor -want full $(wildcard $(DIST)/fleet-linux-* $(DIST)/fleet-darwin-*)
	$(BUILDCHECK) flavor -want pod $(wildcard $(DIST)/fleet-pod-linux-*)

## check-binstore: each linux daemon build in $(DIST) serves the pod builds
## beside it (same version, pod flavor) through its BinStore, never itself.
check-binstore:
	@set -e; for arch in $(POD_ARCHES); do \
	  $(BUILDCHECK) binstore -daemon $(DIST)/fleet-linux-$$arch -dir $(DIST) -arch "$(POD_ARCHES)"; \
	done

## check-nok8s: no k8s.io module reaches the pod build's import graph.
check-nok8s:
	$(BUILDCHECK) nok8s ./cmd/fleet

## check-pod-tmux: run the tmux bundles in $(DIST) in each of $(TMUX_SMOKE_IMAGES).
## --mount, not -v: -v would create a missing bundle as a root-owned directory.
check-pod-tmux:
	@set -e; for arch in $(POD_ARCHES); do \
	  bundle="$(abspath $(DIST))/fleet-tmux-linux-$$arch.tar.gz"; \
	  test -f "$$bundle" || { echo "check-pod-tmux: no $$bundle; run make pod-tmux POD_BIN_DIR=$(DIST)" >&2; exit 1; }; \
	  for img in $(TMUX_SMOKE_IMAGES); do \
	    echo "== $$img linux/$$arch"; \
	    docker run --rm --platform linux/$$arch \
	      --mount "type=bind,src=$$bundle,dst=/bundle.tar.gz,readonly" \
	      --mount "type=bind,src=$(abspath deploy/tmux/smoke.sh),dst=/smoke.sh,readonly" $$img \
	      sh -c 'mkdir -p /opt/fleet/tmux && tar -xzf /bundle.tar.gz -C /opt/fleet/tmux && sh /smoke.sh /opt/fleet/tmux'; \
	  done; \
	done

# BASE_REF: what startup-budget measures against (a local clone may want main).
BASE_REF      ?= origin/main
# STARTUP_FLAGS: extra buildcheck startup flags (-runs, -summary FILE, ...).
STARTUP_FLAGS ?=

## startup-budget: cold start and RSS of the pod build (version) and the host
## build (version, __complete) against $(BASE_REF); fails over budget.
startup-budget:
	@set -e; git rev-parse --verify --quiet '$(BASE_REF)^{commit}' >/dev/null \
	  || { echo "startup-budget: no commit $(BASE_REF); set BASE_REF" >&2; exit 1; }; \
	work=$$(mktemp -d); trap 'rm -rf "$$work"' EXIT; \
	mkdir "$$work/base"; git archive '$(BASE_REF)' | tar -x -C "$$work/base"; \
	echo "building $(BASE_REF) and HEAD"; \
	(cd "$$work/base" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w" -o "$$work/base-full" ./cmd/fleet \
	  && CGO_ENABLED=0 go build -trimpath -buildvcs=false -tags nok8s -ldflags "-s -w" -o "$$work/base-pod" ./cmd/fleet); \
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w" -o "$$work/full" ./cmd/fleet; \
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -tags nok8s -ldflags "-s -w" -o "$$work/pod" ./cmd/fleet; \
	$(BUILDCHECK) startup -base-label "$(BASE_REF)" \
	  -base-full "$$work/base-full" -full "$$work/full" -base-pod "$$work/base-pod" -pod "$$work/pod" $(STARTUP_FLAGS)
