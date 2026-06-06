# fleet-man QA environment.
#
# A heavyweight, self-contained image carrying everything an automated agent
# needs to QA a fleet PR end to end: build it, lint it, run the unit suite, and
# run the integration suite — which drives real devcontainers against a real
# Docker daemon. The daemon runs *inside* this container (Docker-in-Docker), so
# a cloud QA agent gets a clean, isolated environment per PR with no dependency
# on the host's Docker.
#
# Toolchain mirrors the project's devcontainer (.devcontainer/setup.sh) and CI
# (.github/workflows/{unit,integration}.yml) so QA runs match what a PR sees:
#   Go (pinned to go.mod), Docker engine + buildx + compose, Node + the
#   devcontainer CLI, tmux (TUI tests), golangci-lint (import-boundary lint),
#   buf + protoc plugins (proto contract), git, and gh.
#
# Published to GitHub Container Registry by .github/workflows/qa-image.yml.

ARG BASE_IMAGE=ubuntu:26.04
FROM ${BASE_IMAGE}

# --- versions -------------------------------------------------------------
# GO_VERSION is overridden at build time from go.mod by the publish workflow so
# the image's Go always tracks the repo; the default keeps a plain local build
# in sync too. The rest are pinned to match .devcontainer/setup.sh and CI.
ARG GO_VERSION=1.25.8
ARG NODE_MAJOR=20
ARG GOLANGCI_LINT_VERSION=v2.11.4
ARG PROTOC_GEN_GO_VERSION=v1.36.11
ARG PROTOC_GEN_GO_GRPC_VERSION=v1.5.1
ARG DEVCONTAINER_CLI_VERSION=latest
# buildx-provided; selects the right arch for Go / buf downloads (amd64|arm64).
ARG TARGETARCH

# QA user — non-root by default so PR artifacts aren't root-owned, with
# passwordless sudo (dockerd needs root) and docker-group membership.
ARG QA_USER=qa
ARG QA_UID=1000
ARG QA_GID=1000

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

# --- base OS packages -----------------------------------------------------
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl wget gnupg lsb-release apt-transport-https \
        git openssh-client \
        build-essential make pkg-config \
        tmux \
        jq ripgrep unzip zip xz-utils \
        python3 python3-pip python3-venv \
        sudo less vim nano bash-completion \
        procps iproute2 iptables uidmap kmod \
        iputils-ping netcat-openbsd dnsutils; \
    rm -rf /var/lib/apt/lists/*

# --- Docker engine (CE) + buildx + compose --------------------------------
# Installed from Docker's official apt repo. Ubuntu 26.04 ("resolute") is recent
# enough that Docker may not publish that suite yet, so fall back to the newest
# always-supported LTS suite (noble) — the CE packages are forward-compatible.
RUN set -eux; \
    install -m 0755 -d /etc/apt/keyrings; \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc; \
    chmod a+r /etc/apt/keyrings/docker.asc; \
    codename="$(. /etc/os-release && echo "${VERSION_CODENAME}")"; \
    if ! curl -fsSL "https://download.docker.com/linux/ubuntu/dists/${codename}/Release" >/dev/null 2>&1; then \
        echo "Docker apt repo has no '${codename}' suite yet; falling back to 'noble'."; \
        codename="noble"; \
    fi; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${codename} stable" \
        > /etc/apt/sources.list.d/docker.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin; \
    rm -rf /var/lib/apt/lists/*

# --- GitHub CLI -----------------------------------------------------------
RUN set -eux; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli.gpg; \
    chmod a+r /etc/apt/keyrings/githubcli.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends gh; \
    rm -rf /var/lib/apt/lists/*

# --- Node.js + devcontainer CLI -------------------------------------------
# NodeSource's repo is codename-independent (nodistro), so it works on any
# Ubuntu base. The devcontainer CLI is how fleet drives devcontainers.
RUN set -eux; \
    curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash -; \
    apt-get install -y --no-install-recommends nodejs; \
    npm install -g "@devcontainers/cli@${DEVCONTAINER_CLI_VERSION}"; \
    npm cache clean --force; \
    rm -rf /var/lib/apt/lists/*

# --- Go (pinned to go.mod) ------------------------------------------------
RUN set -eux; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o /tmp/go.tgz; \
    rm -rf /usr/local/go; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm /tmp/go.tgz; \
    /usr/local/go/bin/go version
ENV PATH="/usr/local/go/bin:${PATH}"

# --- Go-based tooling -----------------------------------------------------
# golangci-lint (import-boundary lint, pinned to CI), buf (proto contract; from
# the prebuilt release so multi-arch builds don't compile it under emulation),
# and the protoc-gen-* plugins (for `make proto`). All land in /usr/local/bin so
# every user has them; the build caches are wiped to keep the image lean.
RUN set -eux; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "${arch}" in \
        amd64) buf_arch=x86_64 ;; \
        arm64) buf_arch=aarch64 ;; \
        *) echo "unsupported arch: ${arch}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/bufbuild/buf/releases/latest/download/buf-Linux-${buf_arch}" -o /usr/local/bin/buf; \
    chmod +x /usr/local/bin/buf; \
    curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
        | sh -s -- -b /usr/local/bin "${GOLANGCI_LINT_VERSION}"; \
    GOBIN=/usr/local/bin GOTOOLCHAIN=auto go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"; \
    GOBIN=/usr/local/bin GOTOOLCHAIN=auto go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"; \
    go clean -cache -modcache; \
    rm -rf /root/.cache /root/go

# --- QA user --------------------------------------------------------------
# Ubuntu 26.04 ships a default 'ubuntu' user/group at uid/gid 1000; reclaim that
# id for the qa user (cloud platforms commonly assume uid 1000).
RUN set -eux; \
    if id -u ubuntu >/dev/null 2>&1; then userdel -r ubuntu 2>/dev/null || true; fi; \
    if getent group ubuntu >/dev/null 2>&1; then groupdel ubuntu 2>/dev/null || true; fi; \
    groupadd --gid "${QA_GID}" "${QA_USER}"; \
    useradd --uid "${QA_UID}" --gid "${QA_GID}" --create-home --shell /bin/bash "${QA_USER}"; \
    echo "${QA_USER} ALL=(ALL) NOPASSWD:ALL" > "/etc/sudoers.d/${QA_USER}"; \
    chmod 0440 "/etc/sudoers.d/${QA_USER}"; \
    groupadd -f docker; \
    usermod -aG docker "${QA_USER}"

# --- entrypoint + verification --------------------------------------------
COPY entrypoint.sh /usr/local/bin/qa-entrypoint
COPY verify-toolchain.sh /usr/local/bin/qa-verify-toolchain
RUN set -eux; \
    chmod +x /usr/local/bin/qa-entrypoint /usr/local/bin/qa-verify-toolchain; \
    # Build-time assertion: fail the image build if any tool is missing.
    # --no-daemon because dockerd isn't running during the build.
    qa-verify-toolchain --no-daemon

LABEL org.opencontainers.image.source="https://github.com/BenjaminBenetti/fleet-man" \
      org.opencontainers.image.title="fleet-man QA environment" \
      org.opencontainers.image.description="Toolchain image for automated QA of fleet PRs: Go, Docker (DinD), devcontainer CLI, tmux, golangci-lint, buf, gh."

# Bundled daemon stores layers here; a volume avoids overlay-on-overlay churn.
VOLUME /var/lib/docker

# GOTOOLCHAIN=auto lets `go` fetch a newer toolchain if a PR bumps go.mod past
# the pinned version, so the QA env can still build it.
ENV PATH="/home/${QA_USER}/go/bin:${PATH}" \
    GOTOOLCHAIN=auto \
    FLEET_QA_START_DOCKER=1

USER ${QA_USER}
WORKDIR /home/${QA_USER}

ENTRYPOINT ["/usr/local/bin/qa-entrypoint"]
CMD ["bash", "-l"]
