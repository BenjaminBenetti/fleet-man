package kubeshim

// UnsupportedCompose is the error code of a compose devcontainer
// (dockerComposeFile) on Kubernetes, which fleet does not support: the create
// is refused before any build (devcli.ReadMerged), and every compose call that
// still reaches the shim fails with it.
const UnsupportedCompose = "kubernetes:unsupported:compose"
