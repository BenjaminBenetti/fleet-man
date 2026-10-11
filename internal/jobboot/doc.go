// Package jobboot is the bootstrap protocol every fleet Kubernetes Job shares:
// the seed Job that materializes an instance's root generation, and the
// copier, tool, rollback and exporter Jobs built on it later. One protocol,
// tested once, instead of a bootstrap path per Job.
//
// A Job's pod runs the instance's own image (so the seed can copy its root),
// which has no fleet binary in it. So the container starts with a shell loop
// (Command) that waits for fleetd to stage one:
//
//  1. Wait. The container runs `sh -c` over a loop polling ToolsDir/.ready, an
//     emptyDir, once a second; after WaitSeconds without it the container
//     exits ExitNotStaged (70).
//  2. Stage. Once the pod is Running, fleetd probes it (ProbeScript: arch, uid,
//     gzip and sha256sum presence), then streams the static fleet-pod binary
//     and the job spec over exec stdin with StageScript: `head -c N`,
//     optionally through `gzip -dc`, a size check, a sha check where the image
//     has sha256sum, then a rename into place. The spec goes last and its
//     stage writes .ready, so the loop finds a complete job and execs
//     `fleet pod-job --spec ToolsDir/job.json`. pod-job checks its own sha
//     against the spec before anything else, so an image without sha256sum
//     still never runs a corrupt binary's job.
//  3. Feed. pod-job listens on ToolsDir/job.sock. fleetd connects through a
//     thin pipe (`fleet pod-job feed` over a kube exec, or a stream of the
//     pod's bulk session dialing the same socket) and runs Feed: a hello, the
//     workspace tar framed and sha-checked (when the job wants one), progress
//     frames back, then the job's Result, which fleetd acknowledges. A dropped
//     connection loses nothing: the job keeps working, a partly received
//     workspace is discarded, and the next connection starts over.
//  4. Stall. While the job waits on fleetd (for a first connection, bytes of
//     a workspace, or the release of a barrier), StallTimeout without a byte
//     ends it with ExitStalled (75), so a vanished daemon never leaves a Job
//     running up to its activeDeadlineSeconds.
//  5. Barriers. A job can stop at a named barrier (Server.Barrier) for
//     fleetd to act first: a clone's copier stops before its final delta,
//     for fleetd to freeze or stop the source, and after it, for fleetd to let
//     the source go. fleetd's Feed runs FeedOptions.Barrier and releases the
//     job (or refuses, failing it), telling it every BarrierKeepalive that it
//     is still at it.
//  6. Result. The job's full result (the seed's manifest) is written to the
//     instance PVC and returned over the socket. The container's termination
//     message carries only a small Termination (ok, the manifest's sha, the
//     bytes written, a short error), well under the kubelet's 4 KiB.
//
// The package builds everywhere: fleetd renders the command and scripts and
// runs Feed; the pod side (Server, RunPipe) needs only a unix socket.
package jobboot
