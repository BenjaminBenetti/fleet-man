FROM mcr.microsoft.com/devcontainers/base:debian-12
ARG ITEST_BUILD_ARG=unset
RUN printf '%s\n' "${ITEST_BUILD_ARG}" > /etc/fleet-itest-build-arg
# The build context is the repo root (build.context ".."), not .devcontainer.
COPY README.md /etc/fleet-itest-context-readme
