// Package execas runs commands inside a Kubernetes pod as the identity docker
// would have given them. It is the one place the in-pod side of fleet decides
// whom a process runs as, with which environment and in which directory.
//
// On docker, fleet reaches into an instance in two ways: `devcontainer exec`
// (the remoteUser, its login-shell environment, the config's remoteEnv, the
// workspace folder) and `docker exec -u` (the same user, the bare container
// environment). kubectl exec has no -u, -w or -e, so a pod runs its container
// as root and every command passes through execas, which reproduces both, plus
// root for system operations and a literal docker exec for the pod-mode docker
// shim. These are the four Classes. One user for exec and script keeps one tmux
// server per instance, which is what PR #212 fixed on docker.
//
// What docker does, execas does the same way:
//
//   - Users and groups come from the container's own /etc/passwd and
//     /etc/group, parsed in pure Go (no NSS, no cgo), with docker's -u rules.
//     Supplementary groups are set the way initgroups(3) sets them, plus the
//     groups the container runtime gave the whole container (fsGroup,
//     supplementalGroups, docker's --group-add); a user process never keeps
//     the agent's root groups.
//   - The environment is the container's, with PATH and HOME defaulted as
//     docker does. The exec class adds the user's probed environment
//     (userEnvProbe, cached against a fingerprint of the shell's startup
//     files), SSH_AUTH_SOCK and the remoteEnv, resolving ${containerEnv:...}.
//   - A PTY is chowned to the user and the tty group, mode 0620.
//
// On clusters that deny root (capability tier C, restricted PSA or OpenShift
// restricted-v2), the pod runs as an arbitrary uid that pod-init appends to
// /etc/passwd. execas then cannot switch users, and every class runs as that
// one uid: single-uid mode.
//
// The pod agent plans and starts commands in-process (Host.Plan, Start). While
// its session is down, fleetd falls back to a plain kube exec of
// `fleet exec-as`, which reads one Request framed on stdin (never argv, which
// the API server logs), plans it, drops to the user and execs the command in
// place, so its stdio and exit status are the command's own (RunExecAs).
//
// The types (Spec, Request, the frame) build everywhere, for the daemon that
// renders and sends them; starting processes is linux-only.
package execas
