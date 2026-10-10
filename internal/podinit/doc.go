// Package podinit is `fleet pod-init`, PID 1 of every Kubernetes instance pod:
// what docker's runtime (and, with --init, its tini) does for a devcontainer,
// done inside the pod. It reads pid1.json from the pod's spec Secret.
//
// In order, it:
//
//  1. Checks it is the binary the spec was rendered for (sha256 of its own
//     executable), and otherwise re-execs fleet-<sha> from the bin dir: the
//     spec's binary, else the previous one. A pod whose restage went wrong
//     still boots.
//  2. Restores /usr/bin/fleet (a link to /opt/fleet/bin/fleet that every seed
//     writes) if something removed it, so in-instance tools always find fleet.
//  3. Appends the pod's uid to /etc/passwd when it has no entry: on clusters
//     that assign an arbitrary uid (capability tier C), with the remoteUser's
//     home and shell.
//  4. Sets the hostname (--hostname; only where CAP_SYS_ADMIN is granted), the
//     DinD MTU in /etc/docker/daemon.json (the pod network's, so a nested
//     dockerd never fragments under an overlay CNI), and the rlimits
//     (--ulimit; raising a hard limit needs CAP_SYS_RESOURCE, else it is kept
//     and a warning printed).
//  5. On a privileged pod, lays out cgroups the way fleet needs them: two
//     children of the container's cgroup, fleet-agent (PID 1, the pod agent,
//     every kubectl exec) and fleet-user (the start script and everything a
//     user runs). It writes memory.oom.group=0 on the container cgroup and
//     keeps it 0 (containerd's kubelet resets it on a resize), and gives
//     fleet-user a memory.max just under the container's limit: an OOM then
//     kills one process, as on docker, instead of the whole pod. fleet-user
//     can be frozen for a consistent clone while the agent stays live.
//  6. Starts the captured CLI start script as containerUser (internal/execas,
//     the explicit class) in fleet-user, and stays PID 1: it reaps every
//     orphan, forwards signals (all of them with --init, as tini; without,
//     only those the start script catches, as the kernel treats a PID 1),
//     turns the kubelet's SIGTERM into --stop-signal, and copies the script's
//     output to its own (the container log) and to a 10 MB rolling file on
//     the instance disk, which outlives a node that took the container log
//     with it.
//
// It exits with the start script's status, 128+signal when a signal killed
// it, and execas's 125/126/127 when the script could not be started.
//
// The spec types build everywhere, for the daemon that renders pid1.json;
// running PID 1 needs linux.
package podinit
