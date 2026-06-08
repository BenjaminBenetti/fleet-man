# syntax=docker/dockerfile:1

# The fleet gateway image. The gateway is not a separate binary — it is the same
# `fleet` binary run as `fleet gateway` (see internal/cli/gateway.go and
# doc/gateway.md). This Dockerfile builds that binary and ships it in a minimal
# distroless image whose entrypoint is the gateway, so operators can run a public
# relay with a single `docker run`.
#
# Build context is the repository root (the build needs cmd/ + internal/ + go.mod).

# Kept in lockstep with go.mod by the release workflow, which reads the version
# from go.mod and passes it as a build-arg.
ARG GO_VERSION=1.25.8

# Build on the native BUILDPLATFORM and cross-compile to TARGETOS/TARGETARCH
# (provided by buildx). A pure-Go static binary cross-compiles cleanly, so this
# avoids slow QEMU emulation for the multi-arch build.
FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION} AS build
WORKDIR /src

# Resolve modules first so the dependency layer caches independently of source.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# VERSION is the release tag, stamped into internal/version.Version exactly as the
# release binaries are (see .github/workflows/release.yml).
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -ldflags "-s -w -X github.com/BenjaminBenetti/fleet-man/internal/version.Version=${VERSION}" \
      -o /out/fleet ./cmd/fleet

# distroless/static: just a static binary + CA roots (the daemon-facing TLS dial
# and any outbound HTTPS need the system roots). The root (uid 0) variant is used
# deliberately so the documented default ports — :443 (public) and :8443
# (control) — bind out of the box; operators who don't need privileged ports can
# pass --public-addr/--control-addr and drop the capability.
FROM gcr.io/distroless/static-debian12:latest
COPY --from=build /out/fleet /usr/local/bin/fleet

# :443 public listener (HTTPS, MCP agents + remote `fleet`); :8443 control
# listener (TLS, fleet daemons dial in). A GET /healthz on the public listener
# returns "ok".
EXPOSE 443 8443

ENTRYPOINT ["/usr/local/bin/fleet", "gateway"]
