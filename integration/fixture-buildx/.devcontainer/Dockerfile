FROM docker.io/library/docker:28.5.2-cli AS cli
FROM mcr.microsoft.com/devcontainers/base:debian-12
COPY --from=cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=cli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
