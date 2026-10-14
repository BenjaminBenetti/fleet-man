// Package podagent is `fleet pod-agent`, the agent fleet runs inside every
// Kubernetes pod. fleetd starts it over a long-lived exec and speaks podwire to
// it over that exec's stdio; it runs commands as the pod's users (through
// internal/execas), writes and reads files as them, and answers RPCs.
//
// Its pieces:
//
//   - `pod-agent serve` (Serve) is the agent session: one per pod in the main
//     role, plus a bulk-role one fleetd opens on demand. A new main agent
//     that fleetd accepted takes over from the one recorded in the pid file
//     (pid and nonce): it asks it to release, then SIGTERMs it, SIGKILLs it
//     after a grace, and takes the lock.
//   - A resumable command runs under a shepherd (`pod-agent shepherd`), a
//     small process of its own that owns the command, its PTY and its
//     unacknowledged output, and listens on a socket in the run directory. A
//     shepherd outlives any agent and any exec stream (containerd kills an
//     exec's process when an SPDY exec stream is cut), so a held command
//     survives the loss of its transport, a fleetd restart and the agent's
//     replacement alike: a later stream re-attaches it by resume id. A pod
//     holds at most MaxHeld detached commands, each for at most the hold
//     grace; holding one more ends the oldest.
//   - `pod-agent exec --stdio` (ExecStdio) is a direct-mode exec: one command
//     over the exec's own stdio, framed and versioned, which fleet's kube-exec
//     shim uses for TTY attaches that must survive fleetd. It starts the
//     command under a shepherd too, so a dropped attach re-attaches with
//     `--resume`.
//   - On a privileged pod, every command starts in pod-init's user cgroup
//     (fleet-user), and the freeze, thaw and sync RPCs freeze it for a clone's
//     consistent copy: the agent, PID 1 and the watchdogs stay live in
//     fleet-agent. A freeze records its deadline first; the agent's timer
//     thaws at it, and pod-init a little after when the agent is gone.
//
// Everything that starts processes is linux-only; elsewhere the entry points
// fail.
package podagent
