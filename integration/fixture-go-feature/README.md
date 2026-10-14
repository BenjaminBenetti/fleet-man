# fleet-man go-feature fixture

Adds the published Go feature, whose metadata asks for the debug grants
(`capAdd: SYS_PTRACE`, `securityOpt: seccomp=unconfined`) and `init: true`
that Go, Rust and C++ devcontainers carry. The Go tools are skipped to keep the
build short.
