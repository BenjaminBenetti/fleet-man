// Package kubeshim is the docker CLI the Kubernetes backend hands the
// devcontainer CLI, so the CLI keeps every devcontainer.json semantic while
// the container itself runs in a pod.
//
// It is multicall: the fleet executable, reached through a shim directory's
// `docker` or `docker-compose` link (cmd/fleet dispatches on Invoked before
// anything else). The kubeshim.json beside the links picks the mode:
//
//   - Capture mode serves `devcontainer up` and `read-configuration` on the
//     daemon host. Builds, pulls and image inspects go to the real docker;
//     the container verbs see an engine without containers; and the final
//     `docker run` is recorded as a Capture instead of run, which ends the CLI
//     with ExitCaptured. The pod-spec translator turns the Capture into the
//     instance's pod.
//   - Pod mode serves `devcontainer set-up`, which runs the lifecycle hooks
//     against the instance's pod. Inspect answers with the pod's Container
//     (the captured labels, so set-up finds devcontainer.metadata, and the
//     Created and StartedAt that gate its marker files), and exec runs in the
//     pod through an Execer served on the directory's exec socket.
//
// Compose is not supported on Kubernetes: every compose call fails with
// UnsupportedCompose, the backstop for the refusal before any build. Any other
// call the CLI was not expected to make fails loudly (exit 125), and every
// call is journaled (Dir.Calls).
package kubeshim
