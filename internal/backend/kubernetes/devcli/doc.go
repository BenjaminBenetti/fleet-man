// Package devcli drives the official devcontainer CLI for the Kubernetes
// backend, on the daemon host, through the kubeshim docker shim:
//
//   - ReadMerged reads a workspace's configuration (read-configuration
//     --include-merged-configuration) for the create's preflight, and refuses
//     a compose devcontainer before anything is built;
//   - CaptureUp builds the image with the host's docker and captures the
//     docker run the CLI would make (`devcontainer up` with a capture-mode
//     shim), for the pod-spec translator;
//   - SetUp runs the lifecycle hooks in the instance's pod (`devcontainer
//     set-up` with a pod-mode shim).
//
// The CLI keeps every devcontainer.json semantic; fleet only redirects where
// its docker calls go. TestedCLIVersions are the CLI versions whose docker
// calls are recorded in testdata/signature (the capture-signature test).
package devcli
